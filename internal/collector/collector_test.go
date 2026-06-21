package collector

import "testing"

func TestParseNodeFsStats(t *testing.T) {
	// Trimmed kubelet stats/summary payload (the shape DOKS returns).
	raw := []byte(`{
		"node": {
			"nodeName": "pool-x",
			"fs": {"capacityBytes": 84331524096, "usedBytes": 59719876608, "availableBytes": 21116977152},
			"runtime": {"imageFs": {"usedBytes": 43808088064}}
		},
		"pods": []
	}`)
	cap, used, imageFs, err := parseNodeFsStats(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cap != 84331524096 {
		t.Errorf("capacity = %d, want 84331524096", cap)
	}
	if used != 59719876608 {
		t.Errorf("used = %d, want 59719876608", used)
	}
	if imageFs != 43808088064 {
		t.Errorf("imageFs = %d, want 43808088064", imageFs)
	}
}

func TestParseNodeFsStatsMissingFieldsAreZero(t *testing.T) {
	// A kubelet that omits imageFs (or the whole runtime block) must not error;
	// the missing numbers come back as zero so the disk rule simply skips them.
	raw := []byte(`{"node": {"fs": {"capacityBytes": 100}}}`)
	cap, used, imageFs, err := parseNodeFsStats(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cap != 100 || used != 0 || imageFs != 0 {
		t.Errorf("got cap=%d used=%d imageFs=%d, want 100/0/0", cap, used, imageFs)
	}
}

func TestParseNodeFsStatsInvalidJSON(t *testing.T) {
	if _, _, _, err := parseNodeFsStats([]byte("not json")); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}
