package api

import "testing"

// A score shows with at most one decimal.
func TestExperimentScoreValue(t *testing.T) {
	for v, want := range map[float64]string{0: "0", 7: "7", 6.25: "6.3", 13.0 / 3: "4.3"} {
		if got := metricValue("score", &v); got != want {
			t.Errorf("%v: %q; want %q", v, got, want)
		}
	}
}
