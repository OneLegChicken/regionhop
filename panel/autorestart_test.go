package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAutoRestartInterval(t *testing.T) {
	cases := []struct {
		every int
		unit  string
		want  time.Duration
	}{
		{1, "hours", time.Hour},
		{6, "hours", 6 * time.Hour},
		{1, "days", 24 * time.Hour},
		{3, "days", 72 * time.Hour},
		{1, "weeks", 7 * 24 * time.Hour},
		{2, "weeks", 14 * 24 * time.Hour},
		{1, "months", 0},
		{1, "", 0},
	}
	for _, c := range cases {
		if got := (autoRestartSettings{Every: c.every, Unit: c.unit}).interval(); got != c.want {
			t.Errorf("%d %s = %v, want %v", c.every, c.unit, got, c.want)
		}
	}
}

func TestValidateAutoRestart(t *testing.T) {
	good := []autoRestartSettings{
		{Every: 1, Unit: "hours"}, {Every: 8760, Unit: "hours"},
		{Every: 365, Unit: "days"}, {Every: 52, Unit: "weeks"},
	}
	for _, s := range good {
		if err := validateAutoRestart(s); err != nil {
			t.Errorf("%+v should be valid: %v", s, err)
		}
	}
	bad := []autoRestartSettings{
		{Every: 0, Unit: "hours"}, {Every: -3, Unit: "days"},
		{Every: 8761, Unit: "hours"}, {Every: 366, Unit: "days"}, {Every: 53, Unit: "weeks"},
		{Every: 1, Unit: "minutes"}, {Every: 1, Unit: ""},
	}
	for _, s := range bad {
		if err := validateAutoRestart(s); err == nil {
			t.Errorf("%+v should be rejected", s)
		}
	}
	// The message must name the actual problem.
	if err := validateAutoRestart(autoRestartSettings{Every: 0, Unit: "days"}); err == nil || !strings.Contains(err.Error(), "at least 1") {
		t.Errorf("zero interval message: %v", err)
	}
	if err := validateAutoRestart(autoRestartSettings{Every: 5, Unit: "minutes"}); err == nil || !strings.Contains(err.Error(), "hours, days or weeks") {
		t.Errorf("bad unit message: %v", err)
	}
}

func TestAutoRestartStep(t *testing.T) {
	base := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	on := autoRestartSettings{Enabled: true, Every: 2, Unit: "hours"}

	// Disabled: never runs and never touches the schedule.
	off := on
	off.Enabled = false
	if s, run := autoRestartStep(off, base); run || !reflect.DeepEqual(s, off) {
		t.Errorf("disabled schedule changed or ran: %+v %v", s, run)
	}

	// First sight of an enabled schedule arms the countdown, no restart yet.
	s, run := autoRestartStep(on, base)
	if run || s.LastRun != base.Unix() {
		t.Fatalf("arming: run=%v last=%d", run, s.LastRun)
	}

	// Not due until a full interval has passed.
	if _, run := autoRestartStep(s, base.Add(2*time.Hour-time.Second)); run {
		t.Error("ran one second early")
	}
	// Due exactly at the interval, and the countdown restarts from then.
	due := base.Add(2 * time.Hour)
	s2, run := autoRestartStep(s, due)
	if !run || s2.LastRun != due.Unix() {
		t.Fatalf("due: run=%v last=%d want %d", run, s2.LastRun, due.Unix())
	}
	// Right after a run it must not fire again on the next tick.
	if _, run := autoRestartStep(s2, due.Add(time.Minute)); run {
		t.Error("fired twice in a row")
	}
	// A long outage runs once, not once per missed interval.
	s3, run := autoRestartStep(s2, due.Add(10*24*time.Hour))
	if !run {
		t.Fatal("overdue schedule should run")
	}
	if _, run := autoRestartStep(s3, due.Add(10*24*time.Hour+time.Minute)); run {
		t.Error("overdue schedule ran again immediately")
	}
}

func TestAutoRestartSaveLoad(t *testing.T) {
	old := autoRestartPath
	autoRestartPath = filepath.Join(t.TempDir(), "auto-restart.json")
	defer func() { autoRestartPath = old }()

	if got := loadAutoRestart(); !reflect.DeepEqual(got, defaultAutoRestart()) {
		t.Errorf("missing file should give defaults, got %+v", got)
	}
	want := autoRestartSettings{Enabled: true, Every: 12, Unit: "hours", LastRun: 1790000000, Only: []string{"de1", "us1"}}
	if err := saveAutoRestart(want); err != nil {
		t.Fatal(err)
	}
	if got := loadAutoRestart(); !reflect.DeepEqual(got, want) {
		t.Errorf("round trip: got %+v want %+v", got, want)
	}
	// A hand-edited or corrupt file must fall back to the defaults, never
	// produce a nonsense schedule.
	for _, content := range []string{`{`, `{"enabled":true,"every":0,"unit":"days"}`, `{"enabled":true,"every":5,"unit":"fortnights"}`} {
		if err := writeFileForTest(autoRestartPath, content); err != nil {
			t.Fatal(err)
		}
		if got := loadAutoRestart(); !reflect.DeepEqual(got, defaultAutoRestart()) {
			t.Errorf("%q should fall back to defaults, got %+v", content, got)
		}
	}
}

func TestAutoRestartIncludes(t *testing.T) {
	all := autoRestartSettings{}
	for _, n := range []string{"de1", "anything"} {
		if !all.includes(n) {
			t.Errorf("empty selection must cover %q", n)
		}
	}
	some := autoRestartSettings{Only: []string{"de1", "us1"}}
	for n, want := range map[string]bool{"de1": true, "us1": true, "fr1": false, "": false, "de": false} {
		if got := some.includes(n); got != want {
			t.Errorf("includes(%q) = %v, want %v", n, got, want)
		}
	}
}

// A schedule saved before the selection existed has no "only" key and must
// keep meaning "every location".
func TestAutoRestartOldFileMeansAllLocations(t *testing.T) {
	old := autoRestartPath
	autoRestartPath = filepath.Join(t.TempDir(), "auto-restart.json")
	defer func() { autoRestartPath = old }()
	if err := writeFileForTest(autoRestartPath, `{"enabled":true,"every":2,"unit":"days","last_run":1790000000}`); err != nil {
		t.Fatal(err)
	}
	got := loadAutoRestart()
	if !got.Enabled || got.Every != 2 || len(got.Only) != 0 || !got.includes("whatever") {
		t.Fatalf("old-format file misread: %+v", got)
	}
}

func TestFormatScheduleTime(t *testing.T) {
	if formatScheduleTime(0) != "" {
		t.Error("zero should format as empty")
	}
	if got := formatScheduleTime(time.Date(2026, 10, 12, 9, 5, 0, 0, time.UTC).Unix()); got != "2026-10-12 09:05 UTC" {
		t.Errorf("got %q", got)
	}
}
