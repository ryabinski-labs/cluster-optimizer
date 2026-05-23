// Package applier executes a plan.Plan against a Kubernetes cluster. It is
// the live-mutation entry point and the most safety-sensitive code in this
// project; read this header before changing anything.
//
// SAFETY MODEL
//
// The applier refuses to mutate the cluster unless ALL of these are true:
//
//  1. Options.AutoApply == true  — explicit caller intent, set by the
//     --auto-apply flag in main.
//  2. Options.AutoApplyEnvSet == true  — second independent gate, sourced
//     from CLUSTER_OPTIMIZER_AUTOAPPLY=true at process start.
//  3. The halt ConfigMap (default: cluster-optimizer/cluster-optimizer-halt,
//     key "halt") is missing OR has value != "true". An operator can stop
//     all mutations without redeploying by writing halt=true.
//
// Even when these are all true, the applier only patches workloads of kinds
// it knows (Deployment, DaemonSet, StatefulSet), in namespaces the planner
// already validated, with values the planner already bounded by floor and
// max-trim-fraction. Every intended change is logged before it is applied.
//
// In dry-run mode (the default), the applier logs what it WOULD do and
// returns a Result with Applied=false on each entry. Operators are expected
// to read dry-run output for at least one full CronJob cycle before turning
// on live apply.
package applier

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/GipsyChef/cluster-optimizer/internal/plan"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// DefaultHaltNamespace and DefaultHaltConfigMap locate the kill switch.
const (
	DefaultHaltNamespace = "cluster-optimizer"
	DefaultHaltConfigMap = "cluster-optimizer-halt"
	DefaultHaltKey       = "halt"
	// FieldManager scopes our server-side patches so subsequent kubectl
	// apply from the user's manifest repo cleanly takes ownership back.
	FieldManager = "cluster-optimizer-applier"
)

// Options gates the applier. Construct via NewOptions to fill defaults.
type Options struct {
	// AutoApply: caller (main.go) sets this true only when --auto-apply
	// was passed. Default false.
	AutoApply bool
	// AutoApplyEnvSet: main.go sets this true only when
	// CLUSTER_OPTIMIZER_AUTOAPPLY=true at process start. Default false.
	AutoApplyEnvSet bool
	// HaltNamespace / HaltConfigMap / HaltKey identify the kill switch.
	HaltNamespace string
	HaltConfigMap string
	HaltKey       string
}

// NewOptions returns an Options struct with safe defaults; mutation
// remains off until both AutoApply and AutoApplyEnvSet are set true.
func NewOptions() Options {
	return Options{
		HaltNamespace: DefaultHaltNamespace,
		HaltConfigMap: DefaultHaltConfigMap,
		HaltKey:       DefaultHaltKey,
	}
}

// Outcome captures what happened to one PlannedAction.
type Outcome struct {
	Action  plan.PlannedAction `json:"action"`
	Applied bool               `json:"applied"`
	DryRun  bool               `json:"dry_run"`
	Reason  string             `json:"reason,omitempty"`
	Error   string             `json:"error,omitempty"`
}

// Result is the full applier output. Halted=true means the kill switch
// short-circuited everything.
type Result struct {
	DryRun   bool      `json:"dry_run"`
	Halted   bool      `json:"halted"`
	Outcomes []Outcome `json:"outcomes"`
}

// Apply walks the plan and either logs what it would do (dry-run) or
// patches the workloads via the Kubernetes API. The clientset argument
// must be non-nil; pass a fake clientset in tests.
func Apply(ctx context.Context, clientset kubernetes.Interface, p plan.Plan, opts Options) Result {
	live := opts.AutoApply && opts.AutoApplyEnvSet
	result := Result{DryRun: !live}

	// Halt switch is deliberately only consulted in live mode. Dry-run
	// produces logs, not mutations, so blocking it would just hide
	// information from the operator who set halt=true to debug. If you
	// add side effects to dry-run later, move this check above the if.
	if live {
		halted, reason := checkHalt(ctx, clientset, opts)
		if halted {
			log.Printf("Applier: halt configmap set (%s), refusing to apply", reason)
			result.Halted = true
			for _, action := range p.Actions {
				result.Outcomes = append(result.Outcomes, Outcome{Action: action, DryRun: false, Reason: "halt switch active"})
			}
			return result
		}
	}

	for _, action := range p.Actions {
		describePlan(action, live)
		outcome := Outcome{Action: action, DryRun: !live}
		if !live {
			outcome.Reason = "dry-run; set --auto-apply and CLUSTER_OPTIMIZER_AUTOAPPLY=true to apply"
			result.Outcomes = append(result.Outcomes, outcome)
			continue
		}
		if err := patchWorkload(ctx, clientset, action); err != nil {
			log.Printf("Applier: FAILED to patch %s/%s/%s: %v", action.Namespace, action.WorkloadKind, action.WorkloadName, err)
			outcome.Error = err.Error()
		} else {
			log.Printf("Applier: applied %s on %s/%s/%s container=%s", action.Kind, action.Namespace, action.WorkloadKind, action.WorkloadName, action.Container)
			outcome.Applied = true
		}
		result.Outcomes = append(result.Outcomes, outcome)
	}
	return result
}

func describePlan(a plan.PlannedAction, live bool) {
	mode := "DRY-RUN"
	if live {
		mode = "LIVE"
	}
	switch {
	case a.NewCPUm > 0 && a.NewMemMiB > 0:
		log.Printf("Applier %s: %s/%s/%s container=%s cpu %dm->%dm mem %dMi->%dMi reason=%s (rule=%s, seen=%d)",
			mode, a.Namespace, a.WorkloadKind, a.WorkloadName, a.Container,
			a.CurrentCPUm, a.NewCPUm, a.CurrentMemMiB, a.NewMemMiB, a.Reason, a.FindingRuleID, a.OccurrenceCount)
	case a.NewMemMiB > 0:
		log.Printf("Applier %s: %s/%s/%s container=%s mem %dMi->%dMi reason=%s (rule=%s, seen=%d)",
			mode, a.Namespace, a.WorkloadKind, a.WorkloadName, a.Container,
			a.CurrentMemMiB, a.NewMemMiB, a.Reason, a.FindingRuleID, a.OccurrenceCount)
	case a.NewCPUm > 0:
		log.Printf("Applier %s: %s/%s/%s container=%s cpu %dm->%dm reason=%s (rule=%s, seen=%d)",
			mode, a.Namespace, a.WorkloadKind, a.WorkloadName, a.Container,
			a.CurrentCPUm, a.NewCPUm, a.Reason, a.FindingRuleID, a.OccurrenceCount)
	}
}

func checkHalt(ctx context.Context, clientset kubernetes.Interface, opts Options) (bool, string) {
	cm, err := clientset.CoreV1().ConfigMaps(opts.HaltNamespace).Get(ctx, opts.HaltConfigMap, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, ""
		}
		// Fail closed: if we can't read the halt switch, refuse to apply.
		return true, fmt.Sprintf("unreadable halt configmap: %v", err)
	}
	value, ok := cm.Data[opts.HaltKey]
	if !ok {
		return false, ""
	}
	if value == "true" {
		return true, "halt=true"
	}
	return false, ""
}

// patchWorkload issues a strategic merge patch for the single container
// inside the named workload. Server-side patch isn't used here because the
// resource lives in the user's repo and we want kubectl apply to win.
// Strategic merge with the container `name` field as merge key updates the
// matching container in-place without touching other containers.
func patchWorkload(ctx context.Context, clientset kubernetes.Interface, a plan.PlannedAction) error {
	requests := corev1.ResourceList{}
	if a.NewCPUm > 0 {
		requests[corev1.ResourceCPU] = *resource.NewMilliQuantity(a.NewCPUm, resource.DecimalSI)
	}
	if a.NewMemMiB > 0 {
		requests[corev1.ResourceMemory] = *resource.NewQuantity(a.NewMemMiB*1024*1024, resource.BinarySI)
	}
	if len(requests) == 0 {
		return fmt.Errorf("no fields to patch")
	}
	body := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{
						{
							"name":      a.Container,
							"resources": map[string]any{"requests": requestsMap(requests)},
						},
					},
				},
			},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	switch a.WorkloadKind {
	case "Deployment":
		_, err = clientset.AppsV1().Deployments(a.Namespace).Patch(ctx, a.WorkloadName, types.StrategicMergePatchType, payload, metav1.PatchOptions{FieldManager: FieldManager})
	case "DaemonSet":
		_, err = clientset.AppsV1().DaemonSets(a.Namespace).Patch(ctx, a.WorkloadName, types.StrategicMergePatchType, payload, metav1.PatchOptions{FieldManager: FieldManager})
	case "StatefulSet":
		_, err = clientset.AppsV1().StatefulSets(a.Namespace).Patch(ctx, a.WorkloadName, types.StrategicMergePatchType, payload, metav1.PatchOptions{FieldManager: FieldManager})
	default:
		return fmt.Errorf("unsupported workload kind %q", a.WorkloadKind)
	}
	return err
}

func requestsMap(list corev1.ResourceList) map[string]string {
	out := map[string]string{}
	for k, v := range list {
		out[string(k)] = v.String()
	}
	return out
}
