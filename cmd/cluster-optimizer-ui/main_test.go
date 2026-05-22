package main

import (
	"testing"
	"time"
)

func TestDaysBetweenInclusiveCountsUTCCalendarDays(t *testing.T) {
	start := time.Date(2026, 5, 19, 23, 30, 0, 0, time.UTC)
	end := time.Date(2026, 5, 20, 0, 30, 0, 0, time.UTC)

	if got := daysBetweenInclusive(start, end); got != 2 {
		t.Fatalf("daysBetweenInclusive() = %d, want 2", got)
	}
}

func TestDaysBetweenInclusiveSameUTCDate(t *testing.T) {
	start := time.Date(2026, 5, 19, 5, 30, 0, 0, time.UTC)
	end := time.Date(2026, 5, 19, 23, 30, 0, 0, time.UTC)

	if got := daysBetweenInclusive(start, end); got != 1 {
		t.Fatalf("daysBetweenInclusive() = %d, want 1", got)
	}
}

func TestDaysBetweenInclusiveSwapsReversedInputs(t *testing.T) {
	start := time.Date(2026, 5, 20, 0, 30, 0, 0, time.UTC)
	end := time.Date(2026, 5, 19, 23, 30, 0, 0, time.UTC)

	if got := daysBetweenInclusive(start, end); got != 2 {
		t.Fatalf("daysBetweenInclusive() = %d, want 2", got)
	}
}

func TestRuleCanPatchAPIYAMLCoversHPASensitivity(t *testing.T) {
	if !ruleCanPatchAPIYAML("cpu-hpa-low-request-sensitive") {
		t.Fatal("cpu-hpa-low-request-sensitive should be patchable as an api.yml change")
	}
	if ruleCanPatchAPIYAML("fixed-replica-capacity-without-autoscaler") {
		t.Fatal("fixed-replica-capacity-without-autoscaler is not an api.yml patch rule")
	}
}

func TestNeedsResourceTargetExcludesHPASensitivity(t *testing.T) {
	if needsResourceTarget("cpu-hpa-low-request-sensitive") {
		t.Fatal("cpu-hpa-low-request-sensitive must not require a CPU/memory request target")
	}
}
