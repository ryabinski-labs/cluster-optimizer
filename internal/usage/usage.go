// Package usage supplies observed resource usage with an explicit statement
// of how much that observation is worth.
//
// The engine previously had exactly one usage source: a single instantaneous
// sample from metrics.k8s.io, taken once per CronJob tick. That number is fine
// for describing a cluster and useless for deciding to remove a node from one
// — it cannot distinguish a workload that is genuinely idle from one sampled
// between bursts.
//
// Every reading here therefore carries its Fidelity. Downstream consumers are
// expected to gate on it rather than treat all usage numbers alike: reporting
// may use any fidelity, but enforcement must require evidence that actually
// spans time.
package usage

import (
	"context"
	"time"
)

// Fidelity describes what kind of observation produced a reading, ordered
// from strongest to weakest.
type Fidelity string

const (
	// FidelityHistoricalP95 is a true percentile over a lookback window,
	// computed by a time-series database. This is the only fidelity that
	// justifies acting on usage rather than on requests.
	FidelityHistoricalP95 Fidelity = "historical_p95"
	// FidelityRecommender is a recommendation from VPA, which derives it
	// from a decaying histogram of real usage. Comparable in strength to a
	// historical percentile, but per-workload rather than per-pod.
	FidelityRecommender Fidelity = "recommender"
	// FidelityRollup is a percentile over samples this tool collected
	// itself, one per run. Usable once enough runs have accumulated.
	FidelityRollup Fidelity = "sampled_rollup"
	// FidelityInstant is one live sample. Adequate for display, never
	// adequate for removing capacity.
	FidelityInstant Fidelity = "instant"
	// FidelityNone means no usage data was available at all.
	FidelityNone Fidelity = "none"
)

// Actionable reports whether this fidelity is strong enough to justify a
// capacity decision. Deliberately conservative: a fidelity not listed here is
// not actionable, so adding a new weak source cannot accidentally unlock
// enforcement.
func (f Fidelity) Actionable() bool {
	switch f {
	case FidelityHistoricalP95, FidelityRecommender, FidelityRollup:
		return true
	}
	return false
}

// Rank orders fidelities so a resolver can prefer the strongest available.
func (f Fidelity) Rank() int {
	switch f {
	case FidelityHistoricalP95:
		return 4
	case FidelityRecommender:
		return 3
	case FidelityRollup:
		return 2
	case FidelityInstant:
		return 1
	}
	return 0
}

// Reading is one pod's observed usage plus the provenance needed to decide
// whether to trust it.
type Reading struct {
	CPUm      int64 `json:"cpu_m"`
	MemoryMiB int64 `json:"memory_mib"`
	// Samples is how many observations backed the figure. Zero means the
	// source did not report it.
	Samples int `json:"samples,omitempty"`
}

// Set is a full usage snapshot: per-pod readings keyed by "namespace/name",
// plus the provenance that applies to all of them.
type Set struct {
	// Pods maps "namespace/name" to that pod's reading.
	Pods map[string]Reading `json:"pods,omitempty"`
	// Fidelity is what kind of observation these readings are.
	Fidelity Fidelity `json:"fidelity"`
	// Source names the concrete provider, for the audit trail.
	Source string `json:"source"`
	// Lookback is the window the readings summarise. Zero for instant.
	Lookback time.Duration `json:"-"`
	// Quantile is the percentile computed, e.g. 0.95. Zero for instant.
	Quantile float64 `json:"quantile,omitempty"`
	// Err records why a stronger provider was skipped, so the UI can explain
	// a downgrade rather than silently showing weaker numbers.
	Err string `json:"error,omitempty"`
}

// Get returns the reading for a pod and whether one exists.
func (s Set) Get(namespace, name string) (Reading, bool) {
	if s.Pods == nil {
		return Reading{}, false
	}
	r, ok := s.Pods[namespace+"/"+name]
	return r, ok
}

// Coverage reports how many of the supplied pod keys have a reading. A
// provider that answers for only a fraction of the cluster is not usable
// evidence for a whole-pool decision, however good those few readings are.
func (s Set) Coverage(keys []string) float64 {
	if len(keys) == 0 {
		return 0
	}
	var found int
	for _, key := range keys {
		if _, ok := s.Pods[key]; ok {
			found++
		}
	}
	return float64(found) / float64(len(keys))
}

// Empty reports whether the set carries no readings.
func (s Set) Empty() bool { return len(s.Pods) == 0 }

// Actionable reports whether these readings are strong enough to justify a
// capacity decision.
func (s Set) Actionable() bool { return s.Fidelity.Actionable() && !s.Empty() }

// Provider fetches observed usage from one backend.
type Provider interface {
	// Name identifies the provider in logs and the audit trail.
	Name() string
	// PodUsage returns per-pod usage over the requested lookback. A provider
	// that cannot serve the window should say so with an error rather than
	// silently returning a shorter one.
	PodUsage(ctx context.Context, lookback time.Duration, quantile float64) (Set, error)
}

// Key builds the map key used throughout this package.
func Key(namespace, name string) string { return namespace + "/" + name }
