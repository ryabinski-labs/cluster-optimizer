package imagegc

import (
	"context"
	"errors"
	"testing"
)

type fakeRuntime struct {
	images     []Image
	containers []Container
	removed    []string
	removeErr  map[string]error
	listErr    error
}

func (f *fakeRuntime) ListImages(context.Context) ([]Image, error) {
	return f.images, f.listErr
}
func (f *fakeRuntime) ListContainers(context.Context) ([]Container, error) {
	return f.containers, nil
}
func (f *fakeRuntime) RemoveImage(_ context.Context, id string) error {
	if err := f.removeErr[id]; err != nil {
		return err
	}
	f.removed = append(f.removed, id)
	return nil
}

func baseRuntime() *fakeRuntime {
	return &fakeRuntime{
		images: []Image{
			{ID: "sha256:inuse", RepoTags: []string{"app:running"}, SizeBytes: 100},
			{ID: "sha256:stale1", RepoTags: []string{"app:old1"}, SizeBytes: 300},
			{ID: "sha256:stale2", RepoTags: []string{"app:old2"}, SizeBytes: 200},
		},
		containers: []Container{{ImageRef: "sha256:inuse", ImageName: "app:running"}},
	}
}

func TestDryRunRemovesNothing(t *testing.T) {
	rt := baseRuntime()
	res, err := Reclaim(context.Background(), rt, 90, NewOptions())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Mode != "dry-run" {
		t.Errorf("mode = %q, want dry-run", res.Mode)
	}
	if len(rt.removed) != 0 {
		t.Errorf("dry-run must not remove, removed %v", rt.removed)
	}
	if res.Candidates != 2 {
		t.Errorf("candidates = %d, want 2", res.Candidates)
	}
	if res.InUseImages != 1 {
		t.Errorf("in-use = %d, want 1", res.InUseImages)
	}
	if res.ReclaimedBytes != 500 {
		t.Errorf("would-reclaim = %d, want 500", res.ReclaimedBytes)
	}
}

func TestLiveRemovesUnreferencedOnly(t *testing.T) {
	rt := baseRuntime()
	opts := NewOptions()
	opts.Live = true
	res, err := Reclaim(context.Background(), rt, 90, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Removed != 2 || res.ReclaimedBytes != 500 {
		t.Errorf("removed=%d reclaimed=%d, want 2/500", res.Removed, res.ReclaimedBytes)
	}
	for _, id := range rt.removed {
		if id == "sha256:inuse" {
			t.Fatal("removed an in-use image")
		}
	}
}

func TestBelowThresholdSkips(t *testing.T) {
	rt := baseRuntime()
	opts := NewOptions()
	opts.Live = true
	res, err := Reclaim(context.Background(), rt, 50, opts) // threshold 65
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Skipped {
		t.Fatal("expected skip below threshold")
	}
	if len(rt.removed) != 0 {
		t.Errorf("must not remove below threshold, removed %v", rt.removed)
	}
}

func TestMaxRemovalsCapsLargestFirst(t *testing.T) {
	rt := baseRuntime()
	opts := NewOptions()
	opts.Live = true
	opts.MaxRemovals = 1
	res, err := Reclaim(context.Background(), rt, 90, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Removed != 1 {
		t.Fatalf("removed = %d, want 1 (capped)", res.Removed)
	}
	if rt.removed[0] != "sha256:stale1" { // 300 > 200, largest first
		t.Errorf("removed %q, want largest sha256:stale1", rt.removed[0])
	}
}

func TestImageInUseByTagIsKept(t *testing.T) {
	rt := &fakeRuntime{
		images:     []Image{{ID: "sha256:a", RepoTags: []string{"svc:v1"}, SizeBytes: 10}},
		containers: []Container{{ImageName: "svc:v1"}}, // referenced by tag, not ID
	}
	opts := NewOptions()
	opts.Live = true
	res, _ := Reclaim(context.Background(), rt, 99, opts)
	if res.Removed != 0 || res.InUseImages != 1 {
		t.Fatalf("image referenced by tag must be kept: %#v", res)
	}
}

func TestRemoveErrorIsCountedNotFatal(t *testing.T) {
	rt := baseRuntime()
	rt.removeErr = map[string]error{"sha256:stale1": errors.New("image in use")}
	opts := NewOptions()
	opts.Live = true
	res, err := Reclaim(context.Background(), rt, 90, opts)
	if err != nil {
		t.Fatalf("a single remove error must not fail the run: %v", err)
	}
	if res.Removed != 1 || res.RemovalErrors != 1 {
		t.Errorf("removed=%d errors=%d, want 1/1", res.Removed, res.RemovalErrors)
	}
}

func TestListImagesErrorIsReturned(t *testing.T) {
	rt := baseRuntime()
	rt.listErr = errors.New("cri down")
	if _, err := Reclaim(context.Background(), rt, 90, NewOptions()); err == nil {
		t.Fatal("expected error when ListImages fails")
	}
}
