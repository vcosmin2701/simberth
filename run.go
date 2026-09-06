package simberth

// The run model is the contract three things agree on: the Go orchestrator that
// owns a run, the TypeScript agent runner that reports what it did, and the
// macOS app that shows it. It is deliberately flat and JSON-first — every field
// here is decoded by gui/*.swift, so renaming one is a breaking change exactly
// as it is for the types in output.go.
//
// Events are append-only. A run is reconstructed by replaying its events in
// order, which is what lets the GUI attach to a run already in progress and
// lets a finished run be reopened from disk with no daemon running.

import (
	"encoding/json"
	"fmt"
	"time"
)

// EventType names a state change in a run. New types may be added; consumers
// must ignore ones they don't recognize so an older GUI keeps working.
type EventType string

const (
	EventRunStarted  EventType = "run.started"
	EventRunFinished EventType = "run.finished"

	// Simulator lifecycle: leased, prepared (slimmed, booted, app installed),
	// then released back to its prior state.
	EventSimLeased   EventType = "sim.leased"
	EventSimReady    EventType = "sim.ready"
	EventSimReleased EventType = "sim.released"

	// The agent's own progress.
	EventAgentStarted  EventType = "agent.started"
	EventAgentThinking EventType = "agent.thinking"
	EventStepStarted   EventType = "step.started"
	EventStepFinished  EventType = "step.finished"
	EventAgentFinished EventType = "agent.finished"

	// Anything that went wrong outside a step.
	EventError EventType = "error"
)

// RunStatus is the terminal state of a step, a simulator's run, or a whole run.
type RunStatus string

const (
	RunRunning RunStatus = "running"
	RunPassed  RunStatus = "passed"
	RunFailed  RunStatus = "failed"
	RunError   RunStatus = "error" // infrastructure failure, not a test failure
	RunSkipped RunStatus = "skipped"
)

// Event is one append-only record in a run's timeline. It is emitted as a
// single line of JSON (NDJSON) on stdout, which is how the orchestrator streams
// to the GUI and how a run is persisted to disk.
//
// The fields are a union across event types: only those relevant to a given
// Type are set. This keeps the stream a single decodable shape rather than
// forcing consumers to switch before they can parse.
type Event struct {
	Type EventType `json:"type"`
	// UnixMilli rather than time.Time: the Swift decoder sets no date strategy,
	// so dates cross the boundary as numbers.
	At    int64  `json:"at"`
	RunID string `json:"runId"`

	// Set for every sim-scoped and step-scoped event.
	UDID    string `json:"udid,omitempty"`
	SimName string `json:"simName,omitempty"`

	// Step-scoped.
	StepID     int    `json:"stepId,omitempty"`
	Tool       string `json:"tool,omitempty"`       // describe_ui, tap, type_text, …
	Summary    string `json:"summary,omitempty"`    // human-readable: `tap "Sign In"`
	Screenshot string `json:"screenshot,omitempty"` // path relative to the run directory
	DurationMS int64  `json:"durationMs,omitempty"`

	// Terminal state, on *.finished events.
	Status RunStatus `json:"status,omitempty"`
	// Detail carries an error message, or the agent's closing verdict.
	Detail string `json:"detail,omitempty"`

	// Set on run.started so a consumer attaching mid-run knows what it joined.
	Scenario string `json:"scenario,omitempty"`
	AppPath  string `json:"appPath,omitempty"`
	SimCount int    `json:"simCount,omitempty"`
}

// NewEvent stamps an event with the current time.
func NewEvent(runID string, t EventType) Event {
	return Event{Type: t, RunID: runID, At: time.Now().UnixMilli()}
}

// Time returns the event's timestamp.
func (e Event) Time() time.Time { return time.UnixMilli(e.At) }

// Step is one tool call an agent made, reconstructed from its start/finish pair.
type Step struct {
	ID         int       `json:"id"`
	Tool       string    `json:"tool"`
	Summary    string    `json:"summary"`
	Status     RunStatus `json:"status"`
	DurationMS int64     `json:"durationMs"`
	Screenshot string    `json:"screenshot,omitempty"`
	Detail     string    `json:"detail,omitempty"`
}

// SimRun is one simulator's slice of a run: its agent, its steps, its verdict.
type SimRun struct {
	UDID    string    `json:"udid"`
	SimName string    `json:"simName"`
	Status  RunStatus `json:"status"`
	Steps   []Step    `json:"steps"`
	Detail  string    `json:"detail,omitempty"`
}

// Run is the whole picture: a scenario executed across N simulators.
type Run struct {
	ID        string    `json:"id"`
	Scenario  string    `json:"scenario"`
	AppPath   string    `json:"appPath,omitempty"`
	Status    RunStatus `json:"status"`
	StartedAt int64     `json:"startedAt"`
	EndedAt   int64     `json:"endedAt,omitempty"`
	Sims      []*SimRun `json:"sims"`
}

// Apply folds one event into the run, which is how both the CLI and the GUI
// build current state from the stream. Unknown event types are ignored so an
// older consumer keeps working against a newer producer.
func (r *Run) Apply(e Event) {
	switch e.Type {
	case EventRunStarted:
		r.ID = e.RunID
		r.Scenario = e.Scenario
		r.AppPath = e.AppPath
		r.Status = RunRunning
		r.StartedAt = e.At
		return
	case EventRunFinished:
		r.Status = e.Status
		r.EndedAt = e.At
		return
	}

	if e.UDID == "" {
		return
	}
	sim := r.sim(e.UDID)
	if e.SimName != "" {
		sim.SimName = e.SimName
	}

	switch e.Type {
	case EventSimLeased, EventSimReady, EventAgentStarted:
		sim.Status = RunRunning
	case EventStepStarted:
		sim.Steps = append(sim.Steps, Step{
			ID:      e.StepID,
			Tool:    e.Tool,
			Summary: e.Summary,
			Status:  RunRunning,
		})
	case EventStepFinished:
		// Match by ID rather than assuming the last step: an agent may have
		// several tool calls in flight within one turn.
		for i := range sim.Steps {
			if sim.Steps[i].ID == e.StepID {
				sim.Steps[i].Status = e.Status
				sim.Steps[i].DurationMS = e.DurationMS
				sim.Steps[i].Screenshot = e.Screenshot
				sim.Steps[i].Detail = e.Detail
				return
			}
		}
	case EventAgentFinished:
		sim.Status = e.Status
		sim.Detail = e.Detail
	case EventError:
		sim.Status = RunError
		sim.Detail = e.Detail
	}
}

func (r *Run) sim(udid string) *SimRun {
	for _, s := range r.Sims {
		if s.UDID == udid {
			return s
		}
	}
	s := &SimRun{UDID: udid, Status: RunRunning, Steps: []Step{}}
	r.Sims = append(r.Sims, s)
	return s
}

// Verdict reduces the per-simulator outcomes to one status for the whole run.
// Any infrastructure error outranks a test failure, because a failed run that
// never really executed is not the same as one that ran and found a bug.
func (r *Run) Verdict() RunStatus {
	if len(r.Sims) == 0 {
		return RunError
	}
	verdict := RunPassed
	for _, s := range r.Sims {
		switch s.Status {
		case RunError:
			return RunError
		case RunFailed:
			verdict = RunFailed
		case RunRunning:
			if verdict == RunPassed {
				verdict = RunRunning
			}
		}
	}
	return verdict
}

// ReplayStep is one deterministic action recorded from an agent run. Replaying
// these re-runs the scenario with no model in the loop, which is what makes a
// run usable in CI.
//
// Only acting steps are recorded; reads (describe_ui, screenshot) are the
// agent's way of deciding what to do and carry no meaning on replay.
type ReplayStep struct {
	Tool    string `json:"tool"`
	Label   string `json:"label,omitempty"`
	ID      string `json:"id,omitempty"`
	Value   string `json:"value,omitempty"`
	Text    string `json:"text,omitempty"`
	Button  string `json:"button,omitempty"`
	X       int    `json:"x,omitempty"`
	Y       int    `json:"y,omitempty"`
	StartX  int    `json:"startX,omitempty"`
	StartY  int    `json:"startY,omitempty"`
	EndX    int    `json:"endX,omitempty"`
	EndY    int    `json:"endY,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// Replay is a recorded run, replayable against any simulator.
type Replay struct {
	RunID    string       `json:"runId"`
	Scenario string       `json:"scenario"`
	AppPath  string       `json:"appPath,omitempty"`
	Steps    []ReplayStep `json:"steps"`
}

// Target converts a recorded step back into a selector for the AXe layer.
func (s ReplayStep) Target() Target {
	return Target{
		Label:       s.Label,
		ID:          s.ID,
		Value:       s.Value,
		X:           s.X,
		Y:           s.Y,
		WaitTimeout: 10 * time.Second, // replay waits; the app may be slower than when recorded
	}
}

// DecodeEvent parses one NDJSON line.
func DecodeEvent(line []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(line, &e); err != nil {
		return Event{}, fmt.Errorf("decode event: %w", err)
	}
	if e.Type == "" {
		return Event{}, fmt.Errorf("decode event: missing type")
	}
	return e, nil
}
