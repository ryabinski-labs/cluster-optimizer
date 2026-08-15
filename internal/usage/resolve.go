package usage

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/model"
)

// DefaultLookback is the window used for percentile queries. A week covers a
// full weekly traffic cycle, so a workload that is busy only on Mondays is
// not mistaken for an idle one.
const DefaultLookback = 7 * 24 * time.Hour

// DefaultQuantile is the percentile taken from the lookback window. p95 keeps
// genuine peaks while discarding the one-off spike that would otherwise pin a
// workload's sizing to its worst second of the week.
const DefaultQuantile = 0.95

// MinCoverage is the fraction of pods a provider must answer for before its
// readings are treated as describing the cluster. A source that knows about a
// third of the pods cannot support a claim about whether a node is removable.
const MinCoverage = 0.80

// InstantFromPods builds the fallback provider from the live samples the
// collector already attached to each pod, so the weakest source needs no
// separate collection path.
func InstantFromPods(pods []model.Pod) *InstantProvider {
	readings := map[string]Reading{}
	for _, pod := range pods {
		if pod.UsageCPUm == nil && pod.UsageMemoryMiB == nil {
			continue
		}
		var r Reading
		if pod.UsageCPUm != nil {
			r.CPUm = *pod.UsageCPUm
		}
		if pod.UsageMemoryMiB != nil {
			r.MemoryMiB = *pod.UsageMemoryMiB
		}
		r.Samples = 1
		readings[Key(pod.Namespace, pod.Name)] = r
	}
	return &InstantProvider{Readings: readings}
}

// PodKeys lists the active pods a usage source is expected to answer for.
// Completed pods are excluded: a source cannot be faulted for not reporting
// on a pod that has stopped running.
func PodKeys(pods []model.Pod) []string {
	keys := make([]string, 0, len(pods))
	for _, pod := range pods {
		if !pod.Active() || pod.NodeName == "" {
			continue
		}
		keys = append(keys, Key(pod.Namespace, pod.Name))
	}
	return keys
}

// InstantProvider wraps the single metrics.k8s.io sample the collector
// already takes. It exists so the weakest source is a first-class, clearly
// labelled member of the same interface rather than an implicit default —
// its readings display fine and are never actionable.
type InstantProvider struct {
	// Readings are the per-pod samples, keyed by Key(namespace, name).
	Readings map[string]Reading
}

func (p *InstantProvider) Name() string { return "metrics-server" }

func (p *InstantProvider) PodUsage(context.Context, time.Duration, float64) (Set, error) {
	if len(p.Readings) == 0 {
		return Set{Fidelity: FidelityNone, Source: p.Name()}, fmt.Errorf("no live metrics available")
	}
	return Set{
		Pods:     p.Readings,
		Fidelity: FidelityInstant,
		Source:   p.Name(),
	}, nil
}

// Config selects and configures the usage pipeline.
type Config struct {
	// Mode is "auto", "prometheus", or "instant". Auto tries the strongest
	// configured source and falls back; naming one makes a missing source an
	// error rather than a silent downgrade.
	Mode string
	// PrometheusURL enables the Prometheus provider when set.
	PrometheusURL string
	CPUQuery      string
	MemoryQuery   string
	Lookback      time.Duration
	Quantile      float64
	// PodKeys are the pods the caller cares about, used to measure coverage.
	PodKeys []string
}

func (c Config) lookback() time.Duration {
	if c.Lookback > 0 {
		return c.Lookback
	}
	return DefaultLookback
}

func (c Config) quantile() float64 {
	if c.Quantile > 0 && c.Quantile < 1 {
		return c.Quantile
	}
	return DefaultQuantile
}

// Resolve picks the strongest usable usage source and returns its readings.
//
// "Usable" means two things, both enforced here rather than left to callers:
// the provider answered, and it answered for enough of the cluster to support
// a statement about the cluster. A provider that fails either test is
// downgraded, and the reason travels in Set.Err so the UI can say why the
// numbers it is showing are weaker than expected — a silent downgrade would
// be worse than no data, because the weaker number looks identical.
func Resolve(ctx context.Context, cfg Config, fallback *InstantProvider) Set {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "" {
		mode = "auto"
	}

	var notes []string
	switch mode {
	case "auto", "prometheus", "instant":
	default:
		// A misspelled provider name would otherwise fall through to the
		// weakest source and stay there silently: the operator asked for
		// percentile evidence, got a live sample, and nothing said why
		// enforcement never unlocked.
		notes = append(notes, fmt.Sprintf("unknown usage provider %q; falling back to the live sample", cfg.Mode))
		log.Printf("Usage: unknown provider %q (expected auto, prometheus, or instant); falling back to the live metrics sample", cfg.Mode)
		mode = "instant"
	}
	tryProvider := func(p Provider) (Set, bool) {
		set, err := p.PodUsage(ctx, cfg.lookback(), cfg.quantile())
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s unavailable: %v", p.Name(), err))
			log.Printf("Usage: %s unavailable: %v", p.Name(), err)
			return Set{}, false
		}
		if coverage := set.Coverage(cfg.PodKeys); len(cfg.PodKeys) > 0 && coverage < MinCoverage {
			notes = append(notes, fmt.Sprintf("%s covered only %.0f%% of pods (need %.0f%%)",
				p.Name(), coverage*100, MinCoverage*100))
			log.Printf("Usage: %s covered only %.0f%% of pods; not usable as cluster-wide evidence",
				p.Name(), coverage*100)
			return Set{}, false
		}
		return set, true
	}

	wantPrometheus := mode == "prometheus" || (mode == "auto" && cfg.PrometheusURL != "")
	if wantPrometheus {
		if cfg.PrometheusURL == "" {
			notes = append(notes, "prometheus selected but no URL configured")
		} else {
			prom := &PrometheusProvider{
				BaseURL:     cfg.PrometheusURL,
				CPUQuery:    cfg.CPUQuery,
				MemoryQuery: cfg.MemoryQuery,
			}
			if set, ok := tryProvider(prom); ok {
				set.Err = strings.Join(notes, "; ")
				return set
			}
		}
		if mode == "prometheus" {
			// Explicitly requested and unavailable. Return no data rather
			// than quietly substituting a weaker source: the operator asked
			// for percentile evidence and did not get it.
			return Set{Fidelity: FidelityNone, Source: "prometheus", Err: strings.Join(notes, "; ")}
		}
	}

	if fallback != nil {
		if set, ok := tryProvider(fallback); ok {
			set.Err = strings.Join(notes, "; ")
			return set
		}
	}

	return Set{Fidelity: FidelityNone, Err: strings.Join(notes, "; ")}
}

// Effective returns the request figure a capacity decision should use for one
// pod: the larger of what the pod reserves and what it actually uses plus
// headroom.
//
// Taking the max is the whole point. Trimming to observed usage alone assumes
// the percentile window saw the workload's real peak; honouring the request
// alone ignores that most requests are guesses. The max concedes to whichever
// number is more demanding, so the simulation is never optimistic in either
// direction.
//
// When the reading is not actionable the request is returned unchanged — weak
// usage data must not be able to shrink a pod's footprint.
func Effective(requestCPUm, requestMemMiB int64, reading Reading, fidelity Fidelity, headroomPercent int) (cpuM, memMiB int64) {
	if !fidelity.Actionable() {
		return requestCPUm, requestMemMiB
	}
	if headroomPercent < 0 {
		headroomPercent = 0
	}
	scale := func(v int64) int64 { return v + v*int64(headroomPercent)/100 }
	cpuM = max64(requestCPUm, scale(reading.CPUm))
	memMiB = max64(requestMemMiB, scale(reading.MemoryMiB))
	return cpuM, memMiB
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
