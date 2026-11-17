package cmd

import (
	"testing"
	"time"
)

func TestSilenceThreshold(t *testing.T) {
	cases := []struct {
		name     string
		lifetime time.Duration
		want     time.Duration
	}{
		{name: "below default", lifetime: 30 * time.Second, want: 80 * time.Second},
		{name: "default", lifetime: 60 * time.Second, want: 80 * time.Second},
		{name: "above default", lifetime: 120 * time.Second, want: 140 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := silenceThreshold(tc.lifetime); got != tc.want {
				t.Errorf("silenceThreshold(%s) = %s, want %s", tc.lifetime, got, tc.want)
			}
		})
	}
}
