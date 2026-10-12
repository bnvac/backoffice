package app

import (
	"strings"
	"testing"
	"time"
)

func TestParseStepsStripsTheMarkersPeoplePasteIn(t *testing.T) {
	raw := `
- solder the headers
* flash MicroPython
1. check 3V3 before connecting anything
2) wire the BME280
[ ] mount it in the case
[x] order a spare
  • tidy the bench

`
	got := ParseSteps(raw)
	want := []string{
		"solder the headers",
		"flash MicroPython",
		"check 3V3 before connecting anything",
		"wire the BME280",
		"mount it in the case",
		"order a spare",
		"tidy the bench",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d steps, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// "2.54mm pitch headers" is a step, not step 54. Trimming numbers blindly is
// the obvious way to get this wrong.
func TestParseStepsKeepsMeasurements(t *testing.T) {
	for _, line := range []string{
		"2.54mm pitch headers",
		"3.3V not 5V",
		"0.1uF decoupling cap on every rail",
	} {
		got := ParseSteps(line)
		if len(got) != 1 || got[0] != line {
			t.Errorf("ParseSteps(%q) = %q, want it left alone", line, got)
		}
	}
}

func TestParseStepsDropsBlanksAndOverlongLines(t *testing.T) {
	raw := "real step\n\n   \n" + strings.Repeat("x", 600)
	got := ParseSteps(raw)
	if len(got) != 1 || got[0] != "real step" {
		t.Errorf("got %q, want just the real step", got)
	}
}

func TestProgressCounts(t *testing.T) {
	steps := []Step{{}, {}, {}, {}}
	steps[0].DoneAt = time.Now()
	steps[1].DoneAt = time.Now()

	p := StepProgress(steps)
	if p.Done != 2 || p.Total != 4 {
		t.Fatalf("got %+v, want 2 of 4", p)
	}
	if p.Percent() != 50 {
		t.Errorf("Percent() = %d, want 50", p.Percent())
	}
	if p.Complete() {
		t.Error("half done is not complete")
	}
	if p.Left() != "2 of 4" {
		t.Errorf("Left() = %q", p.Left())
	}
}

// Rounding down matters: 9 of 10 must not read as 100%.
func TestProgressRoundsDown(t *testing.T) {
	steps := make([]Step, 10)
	for i := 0; i < 9; i++ {
		steps[i].DoneAt = time.Now()
	}
	p := StepProgress(steps)
	if p.Percent() != 90 {
		t.Errorf("Percent() = %d, want 90", p.Percent())
	}
	if p.Complete() {
		t.Error("9 of 10 is not complete")
	}

	steps[9].DoneAt = time.Now()
	p = StepProgress(steps)
	if p.Percent() != 100 || !p.Complete() || p.Left() != "all done" {
		t.Errorf("a full list should read as finished, got %+v %q", p, p.Left())
	}
}

func TestEmptyProgressIsNotADivideByZero(t *testing.T) {
	p := StepProgress(nil)
	if p.Percent() != 0 || p.Complete() || p.Any() {
		t.Errorf("an empty list should be quiet, got %+v", p)
	}
}

func TestStepsRoundTripAndTick(t *testing.T) {
	app := newTestApp(t)
	pid, err := app.store.CreateProject("Greenhouse logger", "")
	if err != nil {
		t.Fatal(err)
	}

	n, err := app.store.AddSteps(pid, ParseSteps("solder the headers\nflash it\ntest it"))
	if err != nil || n != 3 {
		t.Fatalf("AddSteps: %v (added %d)", err, n)
	}

	steps, err := app.store.ListSteps(pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 || steps[0].Body != "solder the headers" || steps[2].Body != "test it" {
		t.Fatalf("order not kept: %+v", steps)
	}

	if _, err := app.store.ToggleStep(steps[1].ID); err != nil {
		t.Fatal(err)
	}
	steps, _ = app.store.ListSteps(pid)
	if !steps[1].Done() {
		t.Error("toggle did not tick the step")
	}
	if steps[1].DoneAt.IsZero() {
		t.Error("a ticked step should record when")
	}

	// And back again.
	if _, err := app.store.ToggleStep(steps[1].ID); err != nil {
		t.Fatal(err)
	}
	steps, _ = app.store.ListSteps(pid)
	if steps[1].Done() {
		t.Error("toggling twice should leave it unticked")
	}
}

func TestMoveStepSwapsWithItsNeighbour(t *testing.T) {
	app := newTestApp(t)
	pid, _ := app.store.CreateProject("Build", "")
	app.store.AddSteps(pid, []string{"one", "two", "three"})

	steps, _ := app.store.ListSteps(pid)
	if _, err := app.store.MoveStep(steps[2].ID, true); err != nil {
		t.Fatal(err)
	}
	steps, _ = app.store.ListSteps(pid)
	if got := []string{steps[0].Body, steps[1].Body, steps[2].Body}; got[1] != "three" || got[2] != "two" {
		t.Errorf("after moving the last one up: %v", got)
	}

	// Moving the first one up is a no-op, not an error.
	steps, _ = app.store.ListSteps(pid)
	if _, err := app.store.MoveStep(steps[0].ID, true); err != nil {
		t.Errorf("moving the top step up should be harmless: %v", err)
	}
	after, _ := app.store.ListSteps(pid)
	if after[0].Body != steps[0].Body {
		t.Error("the list changed when it should not have")
	}
}

// Deleting out of the middle must not disturb what is left.
func TestDeletingAStepKeepsTheRestInOrder(t *testing.T) {
	app := newTestApp(t)
	pid, _ := app.store.CreateProject("Build", "")
	app.store.AddSteps(pid, []string{"one", "two", "three", "four"})

	steps, _ := app.store.ListSteps(pid)
	gotPID, err := app.store.DeleteStep(steps[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotPID != pid {
		t.Errorf("DeleteStep returned project %d, want %d", gotPID, pid)
	}

	steps, _ = app.store.ListSteps(pid)
	want := []string{"one", "three", "four"}
	if len(steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(steps))
	}
	for i, w := range want {
		if steps[i].Body != w {
			t.Errorf("step %d: got %q, want %q", i, steps[i].Body, w)
		}
	}

	// And a step added afterwards still lands at the end.
	app.store.AddStep(pid, "five")
	steps, _ = app.store.ListSteps(pid)
	if steps[len(steps)-1].Body != "five" {
		t.Errorf("new step did not go last: %+v", steps)
	}
}

func TestResetUnticksEverything(t *testing.T) {
	app := newTestApp(t)
	pid, _ := app.store.CreateProject("Build", "")
	app.store.AddSteps(pid, []string{"one", "two"})

	steps, _ := app.store.ListSteps(pid)
	for _, s := range steps {
		app.store.SetStepDone(s.ID, true)
	}
	if err := app.store.ClearDoneSteps(pid); err != nil {
		t.Fatal(err)
	}
	steps, _ = app.store.ListSteps(pid)
	for _, s := range steps {
		if s.Done() {
			t.Errorf("%q survived the reset", s.Body)
		}
	}
}

func TestAStepNeedsSomethingToDo(t *testing.T) {
	app := newTestApp(t)
	pid, _ := app.store.CreateProject("Build", "")
	if _, err := app.store.AddStep(pid, "   "); err == nil {
		t.Error("a blank step should be refused")
	}
}

// Steps belong to their project and go when it does.
func TestStepsGoWithTheProject(t *testing.T) {
	app := newTestApp(t)
	pid, _ := app.store.CreateProject("Build", "")
	app.store.AddSteps(pid, []string{"one", "two"})

	if err := app.store.DeleteProject(pid); err != nil {
		t.Fatal(err)
	}
	steps, err := app.store.ListSteps(pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Errorf("%d steps outlived their project", len(steps))
	}
}

func TestProjectProgressIsOneQueryForEveryProject(t *testing.T) {
	app := newTestApp(t)
	a, _ := app.store.CreateProject("A", "")
	b, _ := app.store.CreateProject("B", "")
	app.store.AddSteps(a, []string{"one", "two", "three", "four"})
	app.store.AddSteps(b, []string{"one", "two"})

	steps, _ := app.store.ListSteps(a)
	app.store.SetStepDone(steps[0].ID, true)

	all, err := app.store.ProjectProgress()
	if err != nil {
		t.Fatal(err)
	}
	if got := all[a]; got.Done != 1 || got.Total != 4 {
		t.Errorf("project A: got %+v, want 1 of 4", got)
	}
	if got := all[b]; got.Done != 0 || got.Total != 2 {
		t.Errorf("project B: got %+v, want 0 of 2", got)
	}
	// A project with no steps is simply absent, and reads as the zero value.
	if got := all[99]; got.Any() {
		t.Errorf("an unknown project should have no progress, got %+v", got)
	}
}
