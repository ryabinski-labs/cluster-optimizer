// Package imagegc reclaims node disk by removing container images that no
// running (or still-present) container references — the stale image versions
// that accumulate on a Kubernetes node from repeated deploys.
//
// The kubelet has its own image garbage collector, but it only runs once the
// image filesystem crosses imageGCHighThresholdPercent (85% by default) and it
// is not configurable on managed control planes such as DOKS. That leaves a
// gap: a cloud disk alert typically fires at 70%, so a node can sit "disk
// high" indefinitely without the kubelet ever cleaning up. This package runs
// earlier, on a configurable threshold, as an opt-in DaemonSet
// (cmd/node-image-gc).
//
// Like every other mutation path in this repo it is dry-run by default and
// only deletes when explicitly switched live. The reclaim logic here is pure
// (it talks to a small Runtime interface), so it is unit-tested with a fake
// CRI; the real containerd-backed implementation lives in cri.go.
package imagegc

import (
	"context"
	"fmt"
	"log"
	"sort"
)

// Image is a cached container image as reported by the CRI.
type Image struct {
	ID        string
	RepoTags  []string
	SizeBytes int64
}

// Container is a container known to the runtime (running or exited). Both
// identifiers are recorded because the CRI reports the resolved image as an ID
// (ImageRef, e.g. sha256:...) while the pod spec references it by name/tag.
type Container struct {
	ImageRef  string
	ImageName string
}

// Runtime is the slice of the CRI image/runtime services this package needs.
// Implemented by the real containerd client (cri.go) and by fakes in tests.
type Runtime interface {
	ListImages(ctx context.Context) ([]Image, error)
	ListContainers(ctx context.Context) ([]Container, error)
	RemoveImage(ctx context.Context, id string) error
}

// DefaultDiskThresholdPercent is the root-filesystem utilization below which a
// run is a no-op. Chosen below DigitalOcean's 70% disk alert and well below
// the kubelet's 85% image GC, so pruning keeps a node off the alert line
// without fighting the kubelet.
const DefaultDiskThresholdPercent = 65.0

// Options gates and configures a reclaim run. The zero value plus NewOptions
// is a safe dry-run with the default threshold and no cap.
type Options struct {
	// Live actually removes images. Default false: dry-run only logs.
	Live bool

	// DiskThresholdPercent skips the run entirely when the node's root disk
	// is below this fullness. 0 disables the gate (always prune).
	DiskThresholdPercent float64

	// MaxRemovals caps removals per run (largest images first, to free the
	// most space within the cap). 0 means no cap.
	MaxRemovals int
}

// NewOptions returns safe defaults: dry-run, default threshold, no cap.
func NewOptions() Options {
	return Options{DiskThresholdPercent: DefaultDiskThresholdPercent}
}

// Result summarises one reclaim run for logging / audit.
type Result struct {
	Mode           string // "live" or "dry-run"
	Skipped        bool
	SkipReason     string
	DiskPercent    float64
	TotalImages    int
	InUseImages    int
	Candidates     int
	Removed        int
	RemovalErrors  int
	ReclaimedBytes int64 // bytes freed (live) or that would be freed (dry-run)
}

// Reclaim selects images not referenced by any container and, when Live,
// removes them. diskPercent is the node's current root-filesystem utilization;
// the run is a no-op below Options.DiskThresholdPercent. The pure separation
// (caller measures disk + supplies the Runtime) keeps this unit-testable.
func Reclaim(ctx context.Context, rt Runtime, diskPercent float64, opts Options) (Result, error) {
	result := Result{Mode: "dry-run", DiskPercent: diskPercent}
	if opts.Live {
		result.Mode = "live"
	}
	mode := "DRY-RUN"
	if opts.Live {
		mode = "LIVE"
	}

	if opts.DiskThresholdPercent > 0 && diskPercent < opts.DiskThresholdPercent {
		result.Skipped = true
		result.SkipReason = fmt.Sprintf("disk %.1f%% below threshold %.1f%%", diskPercent, opts.DiskThresholdPercent)
		log.Printf("Node image GC (%s): %s, nothing to do.", mode, result.SkipReason)
		return result, nil
	}

	images, err := rt.ListImages(ctx)
	if err != nil {
		return result, fmt.Errorf("list images: %w", err)
	}
	containers, err := rt.ListContainers(ctx)
	if err != nil {
		return result, fmt.Errorf("list containers: %w", err)
	}
	result.TotalImages = len(images)

	inUse := make(map[string]bool, len(containers)*2)
	for _, c := range containers {
		if c.ImageRef != "" {
			inUse[c.ImageRef] = true
		}
		if c.ImageName != "" {
			inUse[c.ImageName] = true
		}
	}

	var candidates []Image
	for _, img := range images {
		if imageInUse(img, inUse) {
			result.InUseImages++
			continue
		}
		candidates = append(candidates, img)
	}
	result.Candidates = len(candidates)

	if len(candidates) == 0 {
		log.Printf("Node image GC (%s): disk %.1f%%, %d image(s), none unreferenced — nothing to prune.", mode, diskPercent, result.TotalImages)
		return result, nil
	}

	// Largest first: a MaxRemovals cap frees the most space, and an
	// uncapped run is order-independent.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].SizeBytes > candidates[j].SizeBytes
	})
	if opts.MaxRemovals > 0 && len(candidates) > opts.MaxRemovals {
		log.Printf("Node image GC (%s): %d unreferenced image(s), capping this run at %d (largest first).", mode, len(candidates), opts.MaxRemovals)
		candidates = candidates[:opts.MaxRemovals]
	}

	if !opts.Live {
		for _, img := range candidates {
			result.ReclaimedBytes += img.SizeBytes
			log.Printf("Node image GC DRY-RUN: would remove %s (%s, %.0f MB)", imageLabel(img), img.ID, float64(img.SizeBytes)/(1024*1024))
		}
		log.Printf("Node image GC DRY-RUN: would remove %d image(s), reclaiming ~%.0f MB. Set CLUSTER_OPTIMIZER_NODE_GC_LIVE=true to actually delete.", len(candidates), float64(result.ReclaimedBytes)/(1024*1024))
		return result, nil
	}

	for _, img := range candidates {
		log.Printf("Node image GC: removing %s (%s)...", imageLabel(img), img.ID)
		if err := rt.RemoveImage(ctx, img.ID); err != nil {
			// RemoveImage fails closed if the runtime still considers the
			// image in use — a safety net beyond our own reference check.
			log.Printf("Node image GC: WARNING: failed to remove %s: %v", img.ID, err)
			result.RemovalErrors++
			continue
		}
		result.Removed++
		result.ReclaimedBytes += img.SizeBytes
	}
	log.Printf("Node image GC: removed %d of %d image(s), reclaimed ~%.0f MB (%d error(s)).", result.Removed, len(candidates), float64(result.ReclaimedBytes)/(1024*1024), result.RemovalErrors)
	return result, nil
}

func imageInUse(img Image, inUse map[string]bool) bool {
	if inUse[img.ID] {
		return true
	}
	for _, tag := range img.RepoTags {
		if inUse[tag] {
			return true
		}
	}
	return false
}

func imageLabel(img Image) string {
	if len(img.RepoTags) > 0 && img.RepoTags[0] != "" {
		return img.RepoTags[0]
	}
	return "<untagged>"
}
