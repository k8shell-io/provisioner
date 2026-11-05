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

func TestScheduleConfigTimezone(t *testing.T) {
	t.Run("defaults to the system's local zone", func(t *testing.T) {
		c := ScheduleConfig{}
		if err := c.normalize(); err != nil {
			t.Fatal(err)
		}
		if c.Location != time.Local {
			t.Fatalf("location = %v, want the local zone", c.Location)
		}
	})

	t.Run("configured zone", func(t *testing.T) {
		c := ScheduleConfig{Timezone: "Europe/Prague"}
		if err := c.normalize(); err != nil {
			t.Fatal(err)
		}
		if c.Location.String() != "Europe/Prague" {
			t.Fatalf("location = %v", c.Location)
		}
	})

	t.Run("unknown zone", func(t *testing.T) {
		c := ScheduleConfig{Timezone: "Mars/Olympus"}
		if err := c.normalize(); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestScheduleConfigForceStartDuration(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"defaults to one hour", "", time.Hour, false},
		{"configured", "30m", 30 * time.Minute, false},
		{"a day", "24h", 24 * time.Hour, false},
		{"not a duration", "soon", 0, true},
		{"zero", "0s", 0, true},
		{"negative", "-1h", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ScheduleConfig{ForceStartDurationRaw: tt.raw}
			err := c.normalize()
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && c.ForceStartDuration != tt.want {
				t.Fatalf("duration = %v, want %v", c.ForceStartDuration, tt.want)
			}
		})
	}
}
