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
