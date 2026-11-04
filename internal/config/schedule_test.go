// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package config

import (
	"testing"
	"time"
)

func TestScheduleConfigNormalize(t *testing.T) {
	tests := []struct {
		name         string
		in           ScheduleConfig
		wantErr      bool
		wantMode     string
		wantInterval time.Duration
	}{
		{"defaults", ScheduleConfig{}, false, ScheduleEnforcementOff, DefaultScheduleInterval},
		{"dry run", ScheduleConfig{Enforcement: "dry-run", IntervalRaw: "10s"}, false, ScheduleEnforcementDryRun, 10 * time.Second},
		{"enforce", ScheduleConfig{Enforcement: "enforce"}, false, ScheduleEnforcementEnforce, DefaultScheduleInterval},
		{"unknown mode", ScheduleConfig{Enforcement: "on"}, true, "", 0},
		{"bad interval", ScheduleConfig{IntervalRaw: "often"}, true, "", 0},
		{"too short interval", ScheduleConfig{IntervalRaw: "100ms"}, true, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.in
			err := c.normalize()
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if c.Enforcement != tt.wantMode || c.Interval != tt.wantInterval {
				t.Fatalf("got %q %v, want %q %v", c.Enforcement, c.Interval, tt.wantMode, tt.wantInterval)
			}
		})
	}
}
