package kit

import "testing"

func TestMemoryLimitFor(t *testing.T) {
	const mib = 1 << 20
	tests := []struct {
		name    string
		env     map[string]string
		want    int64
		wantSet bool
	}{
		{"90% of the container's limit", map[string]string{"MEMORY_LIMIT_BYTES": "268435456"}, 256 * mib / 10 * 9, true},
		{"the chart's 64 MiB default", map[string]string{"MEMORY_LIMIT_BYTES": "67108864"}, 64 * mib / 10 * 9, true},
		{"GOMEMLIMIT set by hand wins", map[string]string{"MEMORY_LIMIT_BYTES": "268435456", "GOMEMLIMIT": "100MiB"}, 0, false},
		{"not running in the chart", map[string]string{}, 0, false},
		{"not a number", map[string]string{"MEMORY_LIMIT_BYTES": "256Mi"}, 0, false},
		{"zero", map[string]string{"MEMORY_LIMIT_BYTES": "0"}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := memoryLimitFor(func(k string) string { return tt.env[k] })
			if got != tt.want || ok != tt.wantSet {
				t.Errorf("memoryLimitFor = %d, %v; want %d, %v", got, ok, tt.want, tt.wantSet)
			}
		})
	}
}
