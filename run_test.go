package simberth

import (
	"encoding/json"
	"testing"
)

// TestApplyBuildsRunFromEventStream is the central guarantee of the event
// model: a run is fully reconstructible by folding its events in order, which
// is what lets the GUI attach mid-run and lets a finished run be reopened.
func TestApplyBuildsRunFromEventStream(t *testing.T) {
	events := []Event{
		{Type: EventRunStarted, RunID: "r1", At: 100, Scenario: "sign in", AppPath: "/tmp/Foo.app"},
		{Type: EventSimLeased, RunID: "r1", UDID: "A", SimName: "iPhone 17"},
		{Type: EventSimReady, RunID: "r1", UDID: "A"},
		{Type: EventStepStarted, RunID: "r1", UDID: "A", StepID: 1, Tool: "tap", Summary: `tap "Sign In"`},
		{Type: EventStepFinished, RunID: "r1", UDID: "A", StepID: 1, Status: RunPassed, DurationMS: 1200},
		{Type: EventAgentFinished, RunID: "r1", UDID: "A", Status: RunPassed, Detail: "home screen loaded"},
		{Type: EventRunFinished, RunID: "r1", At: 900, Status: RunPassed},
	}

	var run Run
	for _, e := range events {
		run.Apply(e)
	}

	if run.ID != "r1" || run.Scenario != "sign in" || run.AppPath != "/tmp/Foo.app" {
		t.Fatalf("run header not applied: %+v", run)
	}
	if run.Status != RunPassed || run.StartedAt != 100 || run.EndedAt != 900 {
		t.Errorf("run terminal state = %v (%d-%d), want passed 100-900", run.Status, run.StartedAt, run.EndedAt)
	}
	if len(run.Sims) != 1 {
		t.Fatalf("got %d sims, want 1", len(run.Sims))
	}
	sim := run.Sims[0]
	if sim.SimName != "iPhone 17" || sim.Status != RunPassed || sim.Detail != "home screen loaded" {
		t.Errorf("sim = %+v, want a passed iPhone 17", sim)
	}
	if len(sim.Steps) != 1 {
		t.Fatalf("got %d steps, want 1", len(sim.Steps))
	}
	if s := sim.Steps[0]; s.Status != RunPassed || s.DurationMS != 1200 || s.Summary != `tap "Sign In"` {
		t.Errorf("step = %+v, want the completed tap", s)
	}
}

// TestApplyMatchesStepsByID guards against assuming the finished step is the
// most recent one: an agent can have several tool calls in flight per turn.
func TestApplyMatchesStepsByID(t *testing.T) {
	var run Run
	for _, e := range []Event{
		{Type: EventStepStarted, UDID: "A", StepID: 1, Tool: "tap"},
		{Type: EventStepStarted, UDID: "A", StepID: 2, Tool: "screenshot"},
		// Finishes arrive out of order.
		{Type: EventStepFinished, UDID: "A", StepID: 2, Status: RunPassed, DurationMS: 50},
		{Type: EventStepFinished, UDID: "A", StepID: 1, Status: RunFailed, DurationMS: 900},
	} {
		run.Apply(e)
	}

	steps := run.Sims[0].Steps
	if steps[0].ID != 1 || steps[0].Status != RunFailed || steps[0].DurationMS != 900 {
		t.Errorf("step 1 = %+v, want the failed tap", steps[0])
	}
	if steps[1].ID != 2 || steps[1].Status != RunPassed || steps[1].DurationMS != 50 {
		t.Errorf("step 2 = %+v, want the passed screenshot", steps[1])
	}
}

// TestApplyIgnoresUnknownEvents keeps an older consumer working against a newer
// producer, which is the stated compatibility rule for the stream.
func TestApplyIgnoresUnknownEvents(t *testing.T) {
	var run Run
	run.Apply(Event{Type: EventRunStarted, RunID: "r1"})
	run.Apply(Event{Type: "some.future.event", RunID: "r1", UDID: "A"})

	if run.ID != "r1" {
		t.Fatal("known event was lost")
	}
	// The unknown event referenced a sim, so a sim is created, but nothing
	// about it should be corrupted.
	for _, s := range run.Sims {
		if s.Status != RunRunning {
			t.Errorf("unknown event changed sim state to %v", s.Status)
		}
	}
}

func TestVerdict(t *testing.T) {
	tests := []struct {
		name     string
		statuses []RunStatus
		want     RunStatus
	}{
		{"all passed", []RunStatus{RunPassed, RunPassed}, RunPassed},
		{"one failure fails the run", []RunStatus{RunPassed, RunFailed}, RunFailed},
		// An infrastructure error is not a test result, so it outranks a
		// failure: the run did not really execute.
		{"an error outranks a failure", []RunStatus{RunFailed, RunError}, RunError},
		{"still running", []RunStatus{RunPassed, RunRunning}, RunRunning},
		{"no sims is an error, not a pass", nil, RunError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var run Run
			for i, st := range tc.statuses {
				run.Sims = append(run.Sims, &SimRun{UDID: string(rune('A' + i)), Status: st})
			}
			if got := run.Verdict(); got != tc.want {
				t.Errorf("Verdict() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEventRoundTrip protects the wire contract the Swift app decodes.
func TestEventRoundTrip(t *testing.T) {
	original := Event{
		Type: EventStepFinished, RunID: "r1", At: 1700000000000,
		UDID: "ABC", SimName: "iPhone 17", StepID: 3, Tool: "tap",
		Summary: `tap "Sign In"`, Screenshot: "screenshots/3.png",
		DurationMS: 1200, Status: RunPassed,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded, err := DecodeEvent(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded != original {
		t.Errorf("round trip changed the event:\n got %+v\nwant %+v", decoded, original)
	}

	// The Swift side decodes camelCase keys with no key strategy, so the exact
	// spelling is part of the contract.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"type", "at", "runId", "udid", "simName", "stepId", "durationMs"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("JSON is missing key %q, which gui/*.swift decodes", key)
		}
	}
}

func TestDecodeEventRejectsGarbage(t *testing.T) {
	if _, err := DecodeEvent([]byte(`{"runId":"r1"}`)); err == nil {
		t.Error("an event with no type should be rejected")
	}
	if _, err := DecodeEvent([]byte(`not json`)); err == nil {
		t.Error("malformed JSON should be rejected")
	}
}
