package model

import "time"

type Node struct {
	Name                 string `json:"name"`
	InstanceType         string `json:"instance_type,omitempty"`
	AllocatableCPUm      int64  `json:"allocatable_cpu_m"`
	AllocatableMemoryMiB int64  `json:"allocatable_memory_mib"`
	// DiskCapacityBytes / DiskUsedBytes describe the node's root filesystem
	// as reported by the kubelet stats/summary endpoint. Zero when the
	// endpoint was unreachable (collection is best-effort: a node without
	// disk stats is simply not evaluated by the disk rule).
	DiskCapacityBytes int64 `json:"disk_capacity_bytes,omitempty"`
	DiskUsedBytes     int64 `json:"disk_used_bytes,omitempty"`
	// ImageFsUsedBytes is the space the container runtime's image filesystem
	// occupies on that node — the bytes a node image GC can reclaim by
	// pruning images no running container references. On DOKS the image
	// filesystem shares the root disk, so this is usually the dominant slice
	// of DiskUsedBytes.
	ImageFsUsedBytes int64 `json:"image_fs_used_bytes,omitempty"`
	// DiskPressure mirrors the node's DiskPressure status condition: true
	// once the kubelet has started evicting pods / refusing to admit new
	// ones because the disk is critically full.
	DiskPressure bool `json:"disk_pressure,omitempty"`
}

// DiskUsedPercent returns root-filesystem utilization as a percentage, or 0
// when capacity is unknown (kubelet stats were unavailable).
func (n Node) DiskUsedPercent() float64 {
	if n.DiskCapacityBytes <= 0 {
		return 0
	}
	return 100 * float64(n.DiskUsedBytes) / float64(n.DiskCapacityBytes)
}

type Pod struct {
	Namespace         string            `json:"namespace"`
	Name              string            `json:"name"`
	NodeName          string            `json:"node_name,omitempty"`
	Phase             string            `json:"phase"`
	Labels            map[string]string `json:"labels,omitempty"`
	OwnerKind         string            `json:"owner_kind,omitempty"`
	OwnerName         string            `json:"owner_name,omitempty"`
	Images            []string          `json:"images,omitempty"`
	RequestsCPUm      int64             `json:"requests_cpu_m"`
	RequestsMemoryMiB int64             `json:"requests_memory_mib"`
	LimitsCPUm        int64             `json:"limits_cpu_m"`
	LimitsMemoryMiB   int64             `json:"limits_memory_mib"`
	UsageCPUm         *int64            `json:"usage_cpu_m,omitempty"`
	UsageMemoryMiB    *int64            `json:"usage_memory_mib,omitempty"`
}

type Workload struct {
	Namespace         string            `json:"namespace"`
	Name              string            `json:"name"`
	Kind              string            `json:"kind"`
	Replicas          int32             `json:"replicas"`
	Labels            map[string]string `json:"labels,omitempty"`
	Selector          map[string]string `json:"selector,omitempty"`
	Images            []string          `json:"images,omitempty"`
	RuntimeHints      []string          `json:"runtime_hints,omitempty"`
	RequestsCPUm      int64             `json:"requests_cpu_m"`
	RequestsMemoryMiB int64             `json:"requests_memory_mib"`
	LimitsCPUm        int64             `json:"limits_cpu_m"`
	LimitsMemoryMiB   int64             `json:"limits_memory_mib"`
	UsageCPUm         *int64            `json:"usage_cpu_m,omitempty"`
	UsageMemoryMiB    *int64            `json:"usage_memory_mib,omitempty"`
}

type PDB struct {
	Namespace      string            `json:"namespace"`
	Name           string            `json:"name"`
	Selector       map[string]string `json:"selector,omitempty"`
	MinAvailable   string            `json:"min_available,omitempty"`
	MaxUnavailable string            `json:"max_unavailable,omitempty"`
}

type HPA struct {
	Namespace                     string   `json:"namespace"`
	Name                          string   `json:"name"`
	TargetKind                    string   `json:"target_kind"`
	TargetName                    string   `json:"target_name"`
	MinReplicas                   int32    `json:"min_replicas"`
	MaxReplicas                   int32    `json:"max_replicas"`
	Metrics                       []string `json:"metrics"`
	CPUUtilizationTarget          *int32   `json:"cpu_utilization_target,omitempty"`
	CPUAverageValueTargetm        *int64   `json:"cpu_average_value_target_m,omitempty"`
	ScaleUpStabilizationSeconds   *int32   `json:"scale_up_stabilization_seconds,omitempty"`
	ScaleDownStabilizationSeconds *int32   `json:"scale_down_stabilization_seconds,omitempty"`
}

type Snapshot struct {
	ClusterID  string     `json:"cluster_id"`
	CapturedAt time.Time  `json:"captured_at"`
	Nodes      []Node     `json:"nodes"`
	Pods       []Pod      `json:"pods"`
	Workloads  []Workload `json:"workloads"`
	PDBs       []PDB      `json:"pdbs"`
	HPAs       []HPA      `json:"hpas"`
}
