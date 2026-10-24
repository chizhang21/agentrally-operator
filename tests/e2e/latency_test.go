//go:build e2e

package e2e_test

import (
	"sort"
	"testing"
	"time"
)

// Small lifecycle samples are smoke measurements, not a production SLO.
func logLatencySamples(t *testing.T, operation string, samples []time.Duration) {
	t.Helper()
	if len(samples) == 0 {
		return
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	percentile := func(percent int) time.Duration { return sorted[(percent*len(sorted)+99)/100-1] }
	t.Logf("latency %s n=%d p50=%s p95=%s (nearest-rank observed smoke samples)", operation, len(sorted), percentile(50), percentile(95))
}
