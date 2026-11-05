// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const scheduleTestBase = "name: base\nisTemplate: true\ndescription: \"Base\"\nimage: base:latest\n" + requiredBlueprintFields

// TestScheduleInheritsPerField checks that a child overriding one schedule
// key keeps the rest of the template's schedule.
func TestScheduleInheritsPerField(t *testing.T) {
	bm := newTestManager(t, map[string]string{
		"base.yaml": scheduleTestBase + `
schedule:
  to: "2026-12-01T18:00:00Z"
  stopGrace: 15m
  terminateGrace: 7d
`,
		"child.yaml": "name: child\ntemplate: base\ndescription: \"Child\"\nimage: child:latest\n" + requiredBlueprintFields + `
schedule:
  stopGrace: 30m
`,
	})

	bp, err := bm.GetBlueprint("child", TestScope())
	if err != nil {
		t.Fatalf("GetBlueprint: %v", err)
	}
	if bp.Schedule == nil {
		t.Fatal("expected the child to have a schedule")
	}
	if bp.Schedule.StopGrace != "30m" {
		t.Errorf("stopGrace = %q, want the child's 30m", bp.Schedule.StopGrace)
	}
	if bp.Schedule.To != "2026-12-01T18:00:00Z" || bp.Schedule.TerminateGrace != "7d" {
		t.Errorf("expected to and terminateGrace inherited, got %+v", bp.Schedule)
	}
}

// TestScheduleInheritedWhole checks that a child without its own schedule
// block gets the template's.
func TestScheduleInheritedWhole(t *testing.T) {
	bm := newTestManager(t, map[string]string{
		"base.yaml": scheduleTestBase + `
schedule:
  to: "2026-12-01T18:00:00Z"
`,
		"child.yaml": "name: child\ntemplate: base\ndescription: \"Child\"\nimage: child:latest\n" + requiredBlueprintFields,
	})

	bp, err := bm.GetBlueprint("child", TestScope())
	if err != nil {
		t.Fatalf("GetBlueprint: %v", err)
	}
	if bp.Schedule == nil || bp.Schedule.To != "2026-12-01T18:00:00Z" {
		t.Fatalf("expected inherited schedule, got %+v", bp.Schedule)
	}
}

// TestScheduleChildClearsInheritedTo checks that a child can drop an
// inherited expiry with an explicit empty value.
func TestScheduleChildClearsInheritedTo(t *testing.T) {
	bm := newTestManager(t, map[string]string{
		"base.yaml": scheduleTestBase + `
schedule:
  to: "2026-12-01T18:00:00Z"
  stopGrace: 15m
`,
		"child.yaml": "name: child\ntemplate: base\ndescription: \"Child\"\nimage: child:latest\n" + requiredBlueprintFields + `
schedule:
  to: ""
`,
	})

	bp, err := bm.GetBlueprint("child", TestScope())
	if err != nil {
		t.Fatalf("GetBlueprint: %v", err)
	}
	if bp.Schedule == nil || bp.Schedule.To != "" || bp.Schedule.StopGrace != "15m" {
		t.Fatalf("expected to cleared and stopGrace kept, got %+v", bp.Schedule)
	}
}

// TestScheduleUnquotedTimestamp checks that a timestamp written without
// quotes, which YAML types as a timestamp, still reads as the string.
func TestScheduleUnquotedTimestamp(t *testing.T) {
	bm := newTestManager(t, map[string]string{
		"bp.yaml": "name: bp\ndescription: \"Bp\"\nimage: bp:latest\n" + requiredBlueprintFields + `
schedule:
  from: 2026-11-10T08:00:00Z
  to: 2026-11-10T18:00:00Z
`,
	})

	bp, err := bm.GetBlueprint("bp", TestScope())
	if err != nil {
		t.Fatalf("GetBlueprint: %v", err)
	}
	if bp.Schedule == nil || bp.Schedule.From != "2026-11-10T08:00:00Z" || bp.Schedule.To != "2026-11-10T18:00:00Z" {
		t.Fatalf("got %+v", bp.Schedule)
	}
}

// TestScheduleInvalidBlueprintRejectedOnLoad checks that an invalid schedule,
// including one that only becomes invalid through inheritance, fails the load.
func TestScheduleInvalidBlueprintRejectedOnLoad(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
	}{
		{"bad stopGrace", map[string]string{
			"bp.yaml": "name: bp\ndescription: \"Bp\"\nimage: bp:latest\n" + requiredBlueprintFields + "schedule:\n  stopGrace: soon\n",
		}},
		{"from after inherited to", map[string]string{
			"base.yaml": scheduleTestBase + "schedule:\n  to: \"2026-12-01T18:00:00Z\"\n",
			"child.yaml": "name: child\ntemplate: base\ndescription: \"Child\"\nimage: child:latest\n" + requiredBlueprintFields +
				"schedule:\n  from: \"2027-01-01T00:00:00Z\"\n",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatalf("write fixture %s: %v", name, err)
				}
			}
			if _, err := NewBlueprintManager(LoadOptions{Dir: dir, EnableWatch: true}); err == nil {
				t.Fatal("expected the load to fail")
			}
		})
	}
}

func TestValidateRawBlueprintSchedule(t *testing.T) {
	bm := newTestManager(t, nil)

	doc := func(schedule string) []byte {
		return []byte("name: my-blueprint\ndescription: A test blueprint\nimage: myimage:latest\n" +
			requiredBlueprintFields + schedule)
	}

	t.Run("valid", func(t *testing.T) {
		issues, _, err := bm.ValidateRawBlueprint(doc(`
schedule:
  from: "2026-11-10T08:00:00Z"
  to: "2026-11-10T18:00:00Z"
  stopGrace: 15m
  terminateGrace: 7d
`))
		if err != nil || len(issues) != 0 {
			t.Fatalf("expected no issues, got %+v (err %v)", issues, err)
		}
	})

	for _, tt := range []struct {
		name, schedule, field string
	}{
		{"bad to", "schedule:\n  to: tomorrow\n", "schedule.to"},
		{"from after to", "schedule:\n  from: \"2026-11-11T00:00:00Z\"\n  to: \"2026-11-10T00:00:00Z\"\n", "schedule.to"},
		{"bad stopGrace", "schedule:\n  stopGrace: -5m\n", "schedule.stopGrace"},
		{"bad terminateGrace", "schedule:\n  terminateGrace: 1w\n", "schedule.terminateGrace"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issues, _, err := bm.ValidateRawBlueprint(doc(tt.schedule))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(issues) != 1 || issues[0].Field != tt.field {
				t.Fatalf("expected one issue on %s, got %+v", tt.field, issues)
			}
			assertAllFieldsSet(t, issues)
		})
	}
}

func newTestManagerInZone(t *testing.T, files map[string]string, zone string) *BlueprintManager {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Skipf("no time zone data: %v", err)
	}
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	bm, err := NewBlueprintManager(LoadOptions{Dir: dir, EnableWatch: true, ScheduleLocation: loc})
	if err != nil {
		t.Fatalf("NewBlueprintManager: %v", err)
	}
	return bm
}

func TestScheduleLocalTimesAndTimezone(t *testing.T) {
	bm := newTestManagerInZone(t, nil, "Europe/Prague")
	doc := func(schedule string) []byte {
		return []byte("name: my-blueprint\ndescription: A test blueprint\nimage: myimage:latest\n" +
			requiredBlueprintFields + schedule)
	}

	for name, schedule := range map[string]string{
		"local times with T":          "schedule:\n  from: 2026-11-10T09:00\n  to: 2026-11-14T19:00:00\n",
		"local times with a space":    "schedule:\n  from: \"2026-11-10 09:00\"\n  to: \"2026-11-14 19:00\"\n",
		"with a timezone":             "schedule:\n  timezone: Asia/Tokyo\n  to: \"2026-11-14 19:00\"\n",
		"timezone and absolute times": "schedule:\n  timezone: Europe/Prague\n  to: \"2026-11-14T18:00:00Z\"\n",
		"mixing local and absolute":   "schedule:\n  from: \"2026-11-10 09:00\"\n  to: \"2026-11-14T18:00:00Z\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			issues, _, err := bm.ValidateRawBlueprint(doc(schedule))
			if err != nil || len(issues) != 0 {
				t.Fatalf("expected no issues, got %+v (err %v)", issues, err)
			}
		})
	}

	for _, tt := range []struct{ name, schedule, field string }{
		{"unknown timezone", "schedule:\n  timezone: Mars/Olympus\n  to: \"2026-11-14 19:00\"\n", "schedule.timezone"},
		{"offset without a leading zero", "schedule:\n  to: \"2026-11-14T19:00:00+1:00\"\n", "schedule.to"},
		{"local from after local to", "schedule:\n  from: \"2026-11-14 19:00\"\n  to: \"2026-11-14 18:00\"\n", "schedule.to"},
		{"date only", "schedule:\n  to: \"2026-11-14\"\n", "schedule.to"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issues, _, err := bm.ValidateRawBlueprint(doc(tt.schedule))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(issues) != 1 || issues[0].Field != tt.field {
				t.Fatalf("expected one issue on %s, got %+v", tt.field, issues)
			}
		})
	}
}

// A local from/to pair is ordered by the instants it stands for, not by its text.
func TestScheduleOrderIsByInstant(t *testing.T) {
	bm := newTestManagerInZone(t, nil, "Europe/Prague")
	doc := func(from, to string) []byte {
		return []byte("name: my-blueprint\ndescription: A test blueprint\nimage: myimage:latest\n" + requiredBlueprintFields +
			"schedule:\n  from: \"" + from + "\"\n  to: \"" + to + "\"\n")
	}

	// 09:00 Prague is 08:00Z: before 08:30Z although "09:00" sorts after "08:30"
	if issues, _, err := bm.ValidateRawBlueprint(doc("2026-11-10 09:00", "2026-11-10T08:30:00Z")); err != nil || len(issues) != 0 {
		t.Fatalf("from 08:00Z, to 08:30Z should be valid, got %+v (err %v)", issues, err)
	}
	// and after 07:30Z although "09:00" sorts before "09:30"
	issues, _, err := bm.ValidateRawBlueprint(doc("2026-11-10 09:00", "2026-11-10T07:30:00Z"))
	if err != nil || len(issues) != 1 || issues[0].Field != "schedule.to" {
		t.Fatalf("from 08:00Z, to 07:30Z should be rejected, got %+v (err %v)", issues, err)
	}
}

func TestScheduleTimezoneInheritsPerField(t *testing.T) {
	bm := newTestManagerInZone(t, map[string]string{
		"base.yaml": scheduleTestBase + `
schedule:
  timezone: Europe/Prague
  to: "2026-12-01 18:00"
  stopGrace: 15m
`,
		"inherits.yaml": "name: inherits\ntemplate: base\ndescription: \"C\"\nimage: c:latest\n" + requiredBlueprintFields + `
schedule:
  stopGrace: 30m
`,
		"overrides.yaml": "name: overrides\ntemplate: base\ndescription: \"C\"\nimage: c:latest\n" + requiredBlueprintFields + `
schedule:
  timezone: Asia/Tokyo
`,
	}, "UTC")

	inh, err := bm.GetBlueprint("inherits", TestScope())
	if err != nil {
		t.Fatal(err)
	}
	if inh.Schedule.Timezone != "Europe/Prague" || inh.Schedule.To != "2026-12-01 18:00" || inh.Schedule.StopGrace != "30m" {
		t.Fatalf("inherits: %+v", inh.Schedule)
	}

	ovr, err := bm.GetBlueprint("overrides", TestScope())
	if err != nil {
		t.Fatal(err)
	}
	// the text is inherited as written; it is read in the child's zone
	if ovr.Schedule.Timezone != "Asia/Tokyo" || ovr.Schedule.To != "2026-12-01 18:00" {
		t.Fatalf("overrides: %+v", ovr.Schedule)
	}
}
