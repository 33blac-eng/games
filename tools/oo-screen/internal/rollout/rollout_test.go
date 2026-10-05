package rollout

import (
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func rep(node, res string, at time.Time) Report {
	return Report{Node: node, Version: "1.2.0", Result: res, At: at}
}

func plan() Plan {
	return Plan{Stages: []int{1, 10, 100}, MinSoak: time.Hour, MinReports: 2, MaxFailRate: 0.2, MaxStageTime: 24 * time.Hour}
}

func TestValidate(t *testing.T) {
	bad := []Plan{{}, {Stages: []int{10, 5}, MinReports: 1}, {Stages: []int{101}, MinReports: 1},
		{Stages: []int{10}, MinReports: 0}, {Stages: []int{10}, MinReports: 1, MaxFailRate: 1}}
	for i, p := range bad {
		if p.Validate() == nil {
			t.Errorf("plan %d accepted", i)
		}
	}
	if err := DefaultPlan().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFullStagedRollout(t *testing.T) {
	p := plan()
	s := State{Version: "1.2.0", StageStarted: t0}
	var reps []Report
	// soak not reached: hold even with reports
	reps = append(reps, rep("a", ResultOK, t0.Add(time.Minute)), rep("b", ResultOK, t0.Add(2*time.Minute)))
	s, d, _ := Step(p, s, reps, t0.Add(30*time.Minute))
	if d.Action != Hold || d.Percent != 1 {
		t.Fatalf("want hold@1, got %+v", d)
	}
	s, d, _ = Step(p, s, reps, t0.Add(time.Hour))
	if d.Action != Advance || d.Percent != 10 {
		t.Fatalf("want advance@10, got %+v", d)
	}
	// old-stage reports do not count for the new stage
	now := t0.Add(3 * time.Hour)
	s, d, _ = Step(p, s, reps, now)
	if d.Action != Hold || d.Percent != 10 {
		t.Fatalf("want hold@10, got %+v", d)
	}
	reps = append(reps, rep("c", ResultOK, now), rep("d", ResultOK, now), rep("e", ResultInconclusive, now))
	s, d, _ = Step(p, s, reps, now)
	if d.Action != Advance || d.Percent != 100 {
		t.Fatalf("want advance@100, got %+v", d)
	}
	_, d, _ = Step(p, s, reps, now.Add(time.Hour))
	if d.Action != Done || d.Percent != 100 || d.OK != 4 || d.Inconcl != 1 {
		t.Fatalf("want done@100, got %+v", d)
	}
}

func TestHaltOnFailRate(t *testing.T) {
	p := plan()
	s := State{Version: "1.2.0", Stage: 1, StageStarted: t0}
	reps := []Report{rep("a", ResultOK, t0), rep("b", ResultOK, t0), rep("c", ResultOK, t0), rep("d", ResultFail, t0)}
	// 1/4 = 0.25 > 0.2
	s, d, _ := Step(p, s, reps, t0.Add(time.Minute))
	if d.Action != Halt || d.Percent != 0 || !s.Halted {
		t.Fatalf("want halt, got %+v", d)
	}
	// halt is sticky even if more ok reports arrive
	for i := 0; i < 50; i++ {
		reps = append(reps, rep(fmt.Sprint("n", i), ResultOK, t0.Add(time.Hour)))
	}
	_, d, _ = Step(p, s, reps, t0.Add(10*time.Hour))
	if d.Action != Halt || d.Percent != 0 {
		t.Fatalf("halt not sticky: %+v", d)
	}
}

func TestLatestReportPerNodeWins(t *testing.T) {
	reps := []Report{rep("a", ResultInconclusive, t0), rep("a", ResultOK, t0.Add(time.Minute)),
		{Node: "a", Version: "9.9.9", Result: ResultFail, At: t0}}
	ok, fail, inc := Tally(reps, "1.2.0")
	if ok != 1 || fail != 0 || inc != 0 {
		t.Fatalf("tally %d/%d/%d", ok, fail, inc)
	}
}

func TestHaltOnSilentStage(t *testing.T) {
	p := plan()
	s := State{Version: "1.2.0", StageStarted: t0}
	reps := []Report{rep("a", ResultOK, t0), rep("b", ResultInconclusive, t0)}
	_, d, _ := Step(p, s, reps, t0.Add(23*time.Hour))
	if d.Action != Hold {
		t.Fatalf("want hold, got %+v", d)
	}
	s, d, _ = Step(p, s, reps, t0.Add(24*time.Hour))
	if d.Action != Halt || !s.Halted {
		t.Fatalf("want halt on silence, got %+v", d)
	}
}

func TestBadState(t *testing.T) {
	if _, _, err := Step(plan(), State{}, nil, t0); err == nil {
		t.Fatal("empty version accepted")
	}
	if _, _, err := Step(plan(), State{Version: "1", Stage: 3}, nil, t0); err == nil {
		t.Fatal("bad stage accepted")
	}
}
