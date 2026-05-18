package quantity

import "testing"

func TestCPUToMillicores(t *testing.T) {
	cases := map[string]int64{
		"250m": 250,
		"1":    1000,
		"0.5":  500,
		"":     0,
	}
	for input, want := range cases {
		got, err := CPUToMillicores(input)
		if err != nil {
			t.Fatalf("CPUToMillicores(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("CPUToMillicores(%q)=%d, want %d", input, got, want)
		}
	}
}

func TestMemoryToMiB(t *testing.T) {
	cases := map[string]int64{
		"512Mi":  512,
		"1Gi":    1024,
		"1048576": 1,
		"":       0,
	}
	for input, want := range cases {
		got, err := MemoryToMiB(input)
		if err != nil {
			t.Fatalf("MemoryToMiB(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("MemoryToMiB(%q)=%d, want %d", input, got, want)
		}
	}
}

