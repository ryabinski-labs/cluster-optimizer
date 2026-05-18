package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/model"
	"github.com/GipsyChef/cluster-optimizer/internal/quantity"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func Collect(ctx context.Context, clusterID string) (model.Snapshot, error) {
	cfg, err := kubeConfig()
	if err != nil {
		return model.Snapshot{}, err
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return model.Snapshot{}, err
	}
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return model.Snapshot{}, err
	}

	podUsage := collectPodMetrics(ctx, dynamicClient)
	nodes, err := collectNodes(ctx, clientset)
	if err != nil {
		return model.Snapshot{}, err
	}
	pods, err := collectPods(ctx, clientset, podUsage)
	if err != nil {
		return model.Snapshot{}, err
	}
	workloads, err := collectWorkloads(ctx, clientset, pods)
	if err != nil {
		return model.Snapshot{}, err
	}
	pdbs, err := collectPDBs(ctx, clientset)
	if err != nil {
		return model.Snapshot{}, err
	}
	hpas, err := collectHPAs(ctx, clientset)
	if err != nil {
		return model.Snapshot{}, err
	}

	return model.Snapshot{
		ClusterID:  clusterID,
		CapturedAt: time.Now().UTC(),
		Nodes:      nodes,
		Pods:       pods,
		Workloads:  workloads,
		PDBs:       pdbs,
		HPAs:       hpas,
	}, nil
}

func kubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return nil, fmt.Errorf("load in-cluster config failed and home dir is unavailable: %w", homeErr)
		}
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func collectNodes(ctx context.Context, clientset *kubernetes.Clientset) ([]model.Node, error) {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	result := make([]model.Node, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		cpu, _ := quantity.CPUToMillicores(node.Status.Allocatable.Cpu().String())
		mem, _ := quantity.MemoryToMiB(node.Status.Allocatable.Memory().String())
		instanceType := node.Labels["node.kubernetes.io/instance-type"]
		if instanceType == "" {
			instanceType = node.Labels["beta.kubernetes.io/instance-type"]
		}
		result = append(result, model.Node{
			Name:                 node.Name,
			InstanceType:         instanceType,
			AllocatableCPUm:      cpu,
			AllocatableMemoryMiB: mem,
		})
	}
	return result, nil
}

func collectPods(ctx context.Context, clientset *kubernetes.Clientset, usage map[string]resourceUsage) ([]model.Pod, error) {
	pods, err := clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	result := make([]model.Pod, 0, len(pods.Items))
	for _, pod := range pods.Items {
		result = append(result, podModel(pod, usage[pod.Namespace+"/"+pod.Name]))
	}
	return result, nil
}

func podModel(pod corev1.Pod, usage resourceUsage) model.Pod {
	var reqCPU, reqMem, limCPU, limMem int64
	for _, container := range pod.Spec.Containers {
		reqCPU += cpuMilli(container.Resources.Requests)
		reqMem += memoryMiB(container.Resources.Requests)
		limCPU += cpuMilli(container.Resources.Limits)
		limMem += memoryMiB(container.Resources.Limits)
	}
	ownerKind, ownerName := firstOwner(pod.OwnerReferences)
	return model.Pod{
		Namespace:         pod.Namespace,
		Name:              pod.Name,
		NodeName:          pod.Spec.NodeName,
		Phase:             string(pod.Status.Phase),
		Labels:            pod.Labels,
		OwnerKind:         ownerKind,
		OwnerName:         ownerName,
		RequestsCPUm:      reqCPU,
		RequestsMemoryMiB: reqMem,
		LimitsCPUm:        limCPU,
		LimitsMemoryMiB:   limMem,
		UsageCPUm:         usage.cpu,
		UsageMemoryMiB:    usage.memory,
	}
}

func collectWorkloads(ctx context.Context, clientset *kubernetes.Clientset, pods []model.Pod) ([]model.Workload, error) {
	byOwner := aggregatePods(pods)
	var workloads []model.Workload
	deployments, err := clientset.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, item := range deployments.Items {
		workloads = append(workloads, deploymentModel(item, byOwner))
	}
	statefulSets, err := clientset.AppsV1().StatefulSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, item := range statefulSets.Items {
		workloads = append(workloads, workloadModel("StatefulSet", item.Namespace, item.Name, item.Status.ReadyReplicas, item.Spec.Template.Labels, item.Spec.Selector.MatchLabels, byOwner[key(item.Namespace, "StatefulSet", item.Name)]))
	}
	daemonSets, err := clientset.AppsV1().DaemonSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, item := range daemonSets.Items {
		workloads = append(workloads, workloadModel("DaemonSet", item.Namespace, item.Name, item.Status.NumberReady, item.Spec.Template.Labels, item.Spec.Selector.MatchLabels, byOwner[key(item.Namespace, "DaemonSet", item.Name)]))
	}
	return workloads, nil
}

func deploymentModel(item appsv1.Deployment, aggregates map[string]podAggregate) model.Workload {
	return workloadModel("Deployment", item.Namespace, item.Name, item.Status.ReadyReplicas, item.Spec.Template.Labels, item.Spec.Selector.MatchLabels, aggregates[key(item.Namespace, "ReplicaSet", item.Name)])
}

func workloadModel(kind, namespace, name string, replicas int32, labels, selector map[string]string, aggregate podAggregate) model.Workload {
	return model.Workload{
		Namespace:         namespace,
		Name:              name,
		Kind:              kind,
		Replicas:          replicas,
		Labels:            labels,
		Selector:          selector,
		RequestsCPUm:      aggregate.requestsCPU,
		RequestsMemoryMiB: aggregate.requestsMem,
		LimitsCPUm:        aggregate.limitsCPU,
		LimitsMemoryMiB:   aggregate.limitsMem,
		UsageCPUm:         aggregate.usageCPU,
		UsageMemoryMiB:    aggregate.usageMem,
	}
}

func collectPDBs(ctx context.Context, clientset *kubernetes.Clientset) ([]model.PDB, error) {
	pdbs, err := clientset.PolicyV1().PodDisruptionBudgets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	result := make([]model.PDB, 0, len(pdbs.Items))
	for _, pdb := range pdbs.Items {
		result = append(result, model.PDB{
			Namespace:      pdb.Namespace,
			Name:           pdb.Name,
			Selector:       selectorLabels(pdb.Spec.Selector),
			MinAvailable:   intOrString(pdb.Spec.MinAvailable),
			MaxUnavailable: intOrString(pdb.Spec.MaxUnavailable),
		})
	}
	return result, nil
}

func collectHPAs(ctx context.Context, clientset *kubernetes.Clientset) ([]model.HPA, error) {
	hpas, err := clientset.AutoscalingV2().HorizontalPodAutoscalers(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	result := make([]model.HPA, 0, len(hpas.Items))
	for _, hpa := range hpas.Items {
		result = append(result, model.HPA{
			Namespace:   hpa.Namespace,
			Name:        hpa.Name,
			TargetKind:  hpa.Spec.ScaleTargetRef.Kind,
			TargetName:  hpa.Spec.ScaleTargetRef.Name,
			MinReplicas: minReplicas(hpa),
			MaxReplicas: hpa.Spec.MaxReplicas,
			Metrics:     hpaMetrics(hpa.Spec.Metrics),
		})
	}
	return result, nil
}

type resourceUsage struct {
	cpu    *int64
	memory *int64
}

func collectPodMetrics(ctx context.Context, dynamicClient dynamic.Interface) map[string]resourceUsage {
	gvr := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}
	list, err := dynamicClient.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return map[string]resourceUsage{}
	}
	result := map[string]resourceUsage{}
	for _, item := range list.Items {
		usage := metricUsage(item)
		result[item.GetNamespace()+"/"+item.GetName()] = usage
	}
	return result
}

func metricUsage(item unstructured.Unstructured) resourceUsage {
	containers, ok, _ := unstructured.NestedSlice(item.Object, "containers")
	if !ok {
		return resourceUsage{}
	}
	var cpuTotal, memTotal int64
	var sawCPU, sawMem bool
	for _, raw := range containers {
		container, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		usage, ok := container["usage"].(map[string]interface{})
		if !ok {
			continue
		}
		if value, ok := usage["cpu"].(string); ok {
			cpu, err := quantity.CPUToMillicores(value)
			if err == nil {
				cpuTotal += cpu
				sawCPU = true
			}
		}
		if value, ok := usage["memory"].(string); ok {
			mem, err := quantity.MemoryToMiB(value)
			if err == nil {
				memTotal += mem
				sawMem = true
			}
		}
	}
	var cpuPtr, memPtr *int64
	if sawCPU {
		cpuPtr = &cpuTotal
	}
	if sawMem {
		memPtr = &memTotal
	}
	return resourceUsage{cpu: cpuPtr, memory: memPtr}
}

type podAggregate struct {
	requestsCPU int64
	requestsMem int64
	limitsCPU   int64
	limitsMem   int64
	usageCPU    *int64
	usageMem    *int64
}

func cpuMilli(resources corev1.ResourceList) int64 {
	value, ok := resources[corev1.ResourceCPU]
	if !ok {
		return 0
	}
	return value.MilliValue()
}

func memoryMiB(resources corev1.ResourceList) int64 {
	value, ok := resources[corev1.ResourceMemory]
	if !ok {
		return 0
	}
	return value.Value() / 1024 / 1024
}

func aggregatePods(pods []model.Pod) map[string]podAggregate {
	aggregates := map[string]podAggregate{}
	for _, pod := range pods {
		if pod.Phase == string(corev1.PodSucceeded) || pod.Phase == string(corev1.PodFailed) {
			continue
		}
		ownerKind := pod.OwnerKind
		ownerName := pod.OwnerName
		if ownerKind == "" {
			ownerKind = "Pod"
			ownerName = pod.Name
		}
		if ownerKind == "ReplicaSet" {
			ownerName = normalizeReplicaSet(ownerName)
		}
		k := key(pod.Namespace, ownerKind, ownerName)
		aggregate := aggregates[k]
		aggregate.requestsCPU += pod.RequestsCPUm
		aggregate.requestsMem += pod.RequestsMemoryMiB
		aggregate.limitsCPU += pod.LimitsCPUm
		aggregate.limitsMem += pod.LimitsMemoryMiB
		aggregate.usageCPU = addPtr(aggregate.usageCPU, pod.UsageCPUm)
		aggregate.usageMem = addPtr(aggregate.usageMem, pod.UsageMemoryMiB)
		aggregates[k] = aggregate
	}
	return aggregates
}

func firstOwner(owners []metav1.OwnerReference) (string, string) {
	if len(owners) == 0 {
		return "", ""
	}
	return owners[0].Kind, owners[0].Name
}

func selectorLabels(selector *metav1.LabelSelector) map[string]string {
	if selector == nil {
		return nil
	}
	return selector.MatchLabels
}

func intOrString(value *intstr.IntOrString) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func minReplicas(hpa autoscalingv2.HorizontalPodAutoscaler) int32 {
	if hpa.Spec.MinReplicas == nil {
		return 1
	}
	return *hpa.Spec.MinReplicas
}

func hpaMetrics(metrics []autoscalingv2.MetricSpec) []string {
	result := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		if metric.Type == autoscalingv2.ResourceMetricSourceType && metric.Resource != nil {
			result = append(result, string(metric.Resource.Name))
			continue
		}
		result = append(result, strings.ToLower(string(metric.Type)))
	}
	return result
}

func normalizeReplicaSet(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return name
	}
	return strings.Join(parts[:len(parts)-1], "-")
}

func key(namespace, kind, name string) string {
	return namespace + "/" + kind + "/" + name
}

func addPtr(current, next *int64) *int64 {
	if next == nil {
		return current
	}
	value := *next
	if current != nil {
		value += *current
	}
	return &value
}
