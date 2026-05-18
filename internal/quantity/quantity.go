package quantity

import (
	"fmt"
	"strconv"
	"strings"
)

func CPUToMillicores(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if strings.HasSuffix(raw, "m") {
		return strconv.ParseInt(strings.TrimSuffix(raw, "m"), 10, 64)
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("parse cpu quantity %q: %w", raw, err)
	}
	return int64(value * 1000), nil
}

func MemoryToMiB(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	units := []struct {
		suffix string
		scale  float64
	}{
		{"Ki", 1.0 / 1024.0},
		{"Mi", 1},
		{"Gi", 1024},
		{"Ti", 1024 * 1024},
		{"K", 1000.0 / 1024.0 / 1024.0},
		{"M", 1000.0 * 1000.0 / 1024.0 / 1024.0},
		{"G", 1000.0 * 1000.0 * 1000.0 / 1024.0 / 1024.0},
	}
	for _, unit := range units {
		if strings.HasSuffix(raw, unit.suffix) {
			value, err := strconv.ParseFloat(strings.TrimSuffix(raw, unit.suffix), 64)
			if err != nil {
				return 0, fmt.Errorf("parse memory quantity %q: %w", raw, err)
			}
			return int64(value * unit.scale), nil
		}
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("parse memory quantity %q: %w", raw, err)
	}
	return int64(value / 1024.0 / 1024.0), nil
}

func FormatCPU(millicores int64) string {
	return fmt.Sprintf("%dm", millicores)
}

func FormatMiB(mib int64) string {
	return fmt.Sprintf("%dMi", mib)
}

