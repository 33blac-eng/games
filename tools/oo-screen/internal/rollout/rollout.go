// Package rollout is the O4 staged-rollout controller for agent auto-update
// (S6). It is a pure decision function over (plan, state, health reports):
// the oo-rollout tool persists state, collects reports and re-signs the
// manifest with the percent the controller chose.
//
// Health signal: each agent that swapped to a new version reports the
// autoupdate.Startup outcome (ok / fail / inconclusive) — the same wave-2
// health gate that triggers the agent's local rollback.
//
// Halt = re-sign the manifest at rollout_percent 0: no new node takes the
// version. Nodes that failed rolled back locally (and deny the version);
// nodes that committed stay on it until a newer release.
package rollout

import (
	"errors"
	"fmt"
	"time"
)

// Result values an agent reports.
const (
	ResultOK           = "ok"
	ResultFail         = "fail"
	ResultInconclusive = "inconclusive"
)

// Report is one agent's health verdict for one version.
type Report struct {
	Node    string    `json:"node"`
	Version string    `json:"version"`
	Result  string    `json:"result"`
	At      time.Time `json:"at"`
}

// Plan configures the stages and the gate.
type Plan struct {
	Stages      []int         `json:"stages"`        // strictly increasing percents, e.g. 1,10,50,100
	MinSoak     time.Duration `json:"min_soak"`      // minimum time on a stage before advancing
	MinReports  int           `json:"min_reports"`   // conclusive reports needed on a stage to advance
	MaxFailRate float64       `json:"max_fail_rate"` // fail/(ok+fail) above this halts
	// MaxStageTime: a stage that never collects MinReports within this time
	// halts (a silent fleet is not a green light). 0 = wait forever.
	MaxStageTime time.Duration `json:"max_stage_time"`
}

// DefaultPlan: 1% canary, then 10/50/100.
func DefaultPlan() Plan {
	return Plan{Stages: []int{1, 10, 50, 100}, MinSoak: 2 * time.Hour, MinReports: 3,
		MaxFailRate: 0.05, MaxStageTime: 48 * time.Hour}
}

// Validate checks plan sanity.
func (p Plan) Validate() error {
	if len(p.Stages) == 0 {
		return errors.New("rollout: no stages")
	}
	prev := 0
	for _, s := range p.Stages {
		if s <= prev || s > 100 {
			return fmt.Errorf("rollout: stages must be strictly increasing within 1..100, got %v", p.Stages)
		}
		prev = s
	}
	if p.MinReports < 1 || p.MaxFailRate < 0 || p.MaxFailRate >= 1 || p.MinSoak < 0 {
		return errors.New("rollout: need min_reports>=1, 0<=max_fail_rate<1, min_soak>=0")
	}
	return nil
}

// State is persisted between steps.
type State struct {
	Version      string    `json:"version"`
	Stage        int       `json:"stage"` // index into Plan.Stages
	StageStarted time.Time `json:"stage_started"`
	// Since: reports older than this are ignored. init sets it, so
	// `init -force` really restarts a halted rollout of the same version
	// instead of re-counting the fail reports that halted it.
	Since  time.Time `json:"since"`
	Halted bool      `json:"halted"`
	Reason string    `json:"reason,omitempty"`
}

// Action is what a step decided.
type Action string

// Actions.
const (
	Hold    Action = "hold"
	Advance Action = "advance"
	Halt    Action = "halt"
	Done    Action = "done"
)

// Decision: Percent is the rollout_percent the manifest must carry now.
type Decision struct {
	Action  Action
	Percent int
	Reason  string
	OK      int
	Fail    int
	Inconcl int
}

// Tally counts the latest report per node for version (a node that reports
// inconclusive and later ok counts once, by its last report).
func Tally(reports []Report, version string) (ok, fail, inc int) {
	last := map[string]Report{}
	for _, r := range reports {
		if r.Version != version || r.Node == "" {
			continue
		}
		if p, seen := last[r.Node]; !seen || !r.At.Before(p.At) {
			last[r.Node] = r
		}
	}
	for _, r := range last {
		switch r.Result {
		case ResultOK:
			ok++
		case ResultFail:
			fail++
		default:
			inc++
		}
	}
	return
}

// Step decides the next action and returns the updated state. The fail rate
// is cumulative over all reports for the version since State.Since (a canary
// failure still counts at 50 %); MinReports is per stage (reports since the stage started).
func Step(p Plan, s State, reports []Report, now time.Time) (State, Decision, error) {
	if err := p.Validate(); err != nil {
		return s, Decision{}, err
	}
	if s.Version == "" || s.Stage < 0 || s.Stage >= len(p.Stages) {
		return s, Decision{}, errors.New("rollout: state has no version or bad stage")
	}
	if s.Halted {
		return s, Decision{Action: Halt, Percent: 0, Reason: s.Reason}, nil
	}
	if !s.Since.IsZero() {
		// Report timestamps are RFC3339 with whole seconds, so a report from
		// the same second as init would otherwise sort before Since.
		since := s.Since.Truncate(time.Second)
		var fresh []Report
		for _, r := range reports {
			if !r.At.Before(since) {
				fresh = append(fresh, r)
			}
		}
		reports = fresh
	}
	ok, fail, inc := Tally(reports, s.Version)
	d := Decision{OK: ok, Fail: fail, Inconcl: inc, Percent: p.Stages[s.Stage]}
	if conclusive := ok + fail; conclusive > 0 && float64(fail)/float64(conclusive) > p.MaxFailRate {
		s.Halted = true
		s.Reason = fmt.Sprintf("fail rate %d/%d > %.2f at %d%%", fail, conclusive, p.MaxFailRate, p.Stages[s.Stage])
		d.Action, d.Percent, d.Reason = Halt, 0, s.Reason
		return s, d, nil
	}
	var stageReports []Report
	for _, r := range reports {
		if !r.At.Before(s.StageStarted) {
			stageReports = append(stageReports, r)
		}
	}
	sok, sfail, _ := Tally(stageReports, s.Version)
	if s.Stage == len(p.Stages)-1 {
		d.Action, d.Reason = Done, "final stage"
		return s, d, nil
	}
	soaked := now.Sub(s.StageStarted) >= p.MinSoak
	enough := sok+sfail >= p.MinReports
	if soaked && enough {
		s.Stage++
		s.StageStarted = now
		d.Action, d.Percent = Advance, p.Stages[s.Stage]
		d.Reason = fmt.Sprintf("stage gate passed: %d ok / %d fail on stage", sok, sfail)
		return s, d, nil
	}
	if p.MaxStageTime > 0 && now.Sub(s.StageStarted) >= p.MaxStageTime && !enough {
		s.Halted = true
		s.Reason = fmt.Sprintf("only %d conclusive reports in %s at %d%%", sok+sfail, p.MaxStageTime, p.Stages[s.Stage])
		d.Action, d.Percent, d.Reason = Halt, 0, s.Reason
		return s, d, nil
	}
	d.Action = Hold
	d.Reason = fmt.Sprintf("soaked=%v reports=%d/%d", soaked, sok+sfail, p.MinReports)
	return s, d, nil
}
