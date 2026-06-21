// Command node-image-gc prunes unreferenced container images on the node it
// runs on, to keep node disk below the cloud provider's alert line without
// waiting for the kubelet's own 85% image GC (which is not tunable on managed
// control planes like DOKS).
//
// It is meant to run as a DaemonSet (manifests/daemonset-image-gc.yaml): one
// pod per node, talking to that node's containerd socket over the CRI gRPC
// API. Disk fullness is measured by statfs on a read-only host mount.
//
// Safety mirrors the rest of cluster-optimizer: dry-run by default (logs what
// it would remove), live only when CLUSTER_OPTIMIZER_NODE_GC_LIVE=true, it
// honours the shared halt ConfigMap, and it only ever removes images no
// container references.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/applier"
	"github.com/GipsyChef/cluster-optimizer/internal/imagegc"
	"golang.org/x/sys/unix"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "node-image-gc: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	live          bool
	threshold     float64
	maxRemovals   int
	interval      time.Duration
	hostFsPath    string
	criEndpoint   string
	haltNamespace string
	haltConfigMap string
	haltKey       string
}

func run(args []string) error {
	var cfg config
	flags := flag.NewFlagSet("node-image-gc", flag.ContinueOnError)
	flags.BoolVar(&cfg.live, "live", envBoolOr("CLUSTER_OPTIMIZER_NODE_GC_LIVE", false), "actually remove images (default: dry-run)")
	flags.Float64Var(&cfg.threshold, "disk-threshold", envFloatOr("CLUSTER_OPTIMIZER_NODE_GC_DISK_THRESHOLD", imagegc.DefaultDiskThresholdPercent), "only prune when root disk is at least this percent full (0 = always)")
	flags.IntVar(&cfg.maxRemovals, "max-removals", envIntOr("CLUSTER_OPTIMIZER_NODE_GC_MAX_REMOVALS", 0), "cap image removals per run, largest first (0 = no cap)")
	flags.DurationVar(&cfg.interval, "interval", envDurationOr("CLUSTER_OPTIMIZER_NODE_GC_INTERVAL", 0), "run repeatedly on this interval (0 = run once and exit); DaemonSet sets e.g. 30m")
	flags.StringVar(&cfg.hostFsPath, "host-fs", envOr("CLUSTER_OPTIMIZER_NODE_GC_HOST_FS", "/host"), "path to the node root filesystem (read-only host mount) used to measure disk usage")
	flags.StringVar(&cfg.criEndpoint, "cri-endpoint", envOr("CONTAINER_RUNTIME_ENDPOINT", "/run/containerd/containerd.sock"), "CRI socket path/URI")
	flags.StringVar(&cfg.haltNamespace, "halt-namespace", applier.DefaultHaltNamespace, "namespace of the shared halt ConfigMap")
	flags.StringVar(&cfg.haltConfigMap, "halt-configmap", applier.DefaultHaltConfigMap, "name of the shared halt ConfigMap")
	flags.StringVar(&cfg.haltKey, "halt-key", applier.DefaultHaltKey, "key in the halt ConfigMap")
	if err := flags.Parse(args); err != nil {
		return err
	}

	// In-cluster client is only needed to consult the halt switch in live
	// mode. Dry-run never mutates, so a missing client there is non-fatal.
	var clientset kubernetes.Interface
	if cfg.live {
		cs, err := inClusterClient()
		if err != nil {
			return fmt.Errorf("live mode needs an in-cluster client to read the halt switch: %w", err)
		}
		clientset = cs
	}

	ctx := context.Background()
	if cfg.interval <= 0 {
		return runOnce(ctx, cfg, clientset)
	}

	log.Printf("node-image-gc: running every %s (live=%v, threshold=%.0f%%)", cfg.interval, cfg.live, cfg.threshold)
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for {
		if err := runOnce(ctx, cfg, clientset); err != nil {
			log.Printf("node-image-gc: run failed: %v", err)
		}
		<-ticker.C
	}
}

func runOnce(ctx context.Context, cfg config, clientset kubernetes.Interface) error {
	diskPercent, err := diskUsedPercent(cfg.hostFsPath)
	if err != nil {
		return fmt.Errorf("measure disk at %q: %w", cfg.hostFsPath, err)
	}

	if cfg.live {
		if halted, reason := haltCheck(ctx, clientset, cfg); halted {
			log.Printf("node-image-gc: halt switch active (%s), refusing to remove images", reason)
			return nil
		}
	}

	client, err := imagegc.DialCRI(ctx, cfg.criEndpoint)
	if err != nil {
		return err
	}
	defer client.Close()

	opts := imagegc.NewOptions()
	opts.Live = cfg.live
	opts.DiskThresholdPercent = cfg.threshold
	opts.MaxRemovals = cfg.maxRemovals
	_, err = imagegc.Reclaim(ctx, client, diskPercent, opts)
	return err
}

// diskUsedPercent returns root-filesystem utilization for the filesystem
// containing path, computed the same way df does (used / total of the blocks
// the kernel reports), so it matches the cloud provider's disk metric.
func diskUsedPercent(path string) (float64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bfree * uint64(st.Bsize)
	if total == 0 {
		return 0, fmt.Errorf("statfs reported zero-size filesystem")
	}
	used := total - free
	return 100 * float64(used) / float64(total), nil
}

// haltCheck consults the shared halt ConfigMap. Fail closed: if it cannot be
// read (and is not simply absent), refuse to mutate.
func haltCheck(ctx context.Context, clientset kubernetes.Interface, cfg config) (bool, string) {
	cm, err := clientset.CoreV1().ConfigMaps(cfg.haltNamespace).Get(ctx, cfg.haltConfigMap, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, ""
		}
		return true, fmt.Sprintf("unreadable halt configmap: %v", err)
	}
	if cm.Data[cfg.haltKey] == "true" {
		return true, "halt=true"
	}
	return false, ""
}

func inClusterClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBoolOr(key string, fallback bool) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return v
}

func envIntOr(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

func envFloatOr(key string, fallback float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return v
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return v
}
