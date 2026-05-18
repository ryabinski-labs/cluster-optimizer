package model

import "time"

type Node struct {
	Name                 string `json:"name"`
	InstanceType         string `json:"instance_type,omitempty"`
	AllocatableCPUm      int64  `json:"allocatable_cpu_m"`
	AllocatableMemoryMiB int64  `json:"allocatable_memory_mib"`
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
	Namespace   string   `json:"namespace"`
	Name        string   `json:"name"`
	TargetKind  string   `json:"target_kind"`
	TargetName  string   `json:"target_name"`
	MinReplicas int32    `json:"min_replicas"`
	MaxReplicas int32    `json:"max_replicas"`
	Metrics     []string `json:"metrics"`
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
