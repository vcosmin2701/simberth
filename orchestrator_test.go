package simberth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecuteRequiresScenarioAndAgent(t *testing.T) {
	if _, err := Execute(context.Background(), RunOptions{AgentCommand: noopAgent}, nil); err == nil {
		t.Error("a run with no scenario should be rejected")
	}
	if _, err := Execute(context.Background(), RunOptions{Scenario: "x"}, nil); err == nil {
		t.Error("a run with no agent should be rejected")
	}
}

func noopAgent(context.Context, LeasedSim, RunOptions, EventSink) error { return nil }

// TestRunPersistsAndReloads covers the promise that a finished run can be
// reopened from disk with no daemon running.
func TestRunPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	runID := "20260906-120000"

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	events := []Event{
		{Type: EventRunStarted, RunID: runID, At: 1, Scenario: "sign in"},
		{Type: EventSimLeased, RunID: runID, UDID: "A", SimName: "iPhone 17"},
		{Type: EventStepStarted, RunID: runID, UDID: "A", StepID: 1, Tool: "tap", Summary: `tap "Sign In"`},
		{Type: EventStepFinished, RunID: runID, UDID: "A", StepID: 1, Status: RunPassed, DurationMS: 40},
		{Type: EventAgentFinished, RunID: runID, UDID: "A", Status: RunPassed},
		{Type: EventRunFinished, RunID: runID, At: 5, Status: RunPassed},
	}
	for _, e := range events {
		data, err := marshalLine(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := log.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	log.Close()

	run, err := LoadRun(dir)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.ID != runID || run.Status != RunPassed || len(run.Sims) != 1 {
		t.Fatalf("reloaded run = %+v", run)
	}
	if len(run.Sims[0].Steps) != 1 || run.Sims[0].Steps[0].Summary != `tap "Sign In"` {
		t.Errorf("steps did not survive the round trip: %+v", run.Sims[0].Steps)
	}
}

// TestLoadRunToleratesTruncatedLog matters because a killed run leaves a
// partial final line, and that must not make the whole run unreadable.
func TestLoadRunToleratesTruncatedLog(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"run.started","runId":"r1","at":1,"scenario":"x"}
{"type":"sim.leased","runId":"r1","udid":"A","simName":"iPhone 17"}
{"type":"step.started","runId":"r1","udid":"A","stepI`
	if err := os.WriteFile(filepath.Join(dir, "events.ndjson"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	run, err := LoadRun(dir)
	if err != nil {
		t.Fatalf("a truncated log should still load: %v", err)
	}
	if run.ID != "r1" || len(run.Sims) != 1 {
		t.Errorf("complete lines were lost: %+v", run)
	}
}

// TestAgentsRunConcurrently is the property the whole project exists for: N
// simulators are driven at the same time, not one after another.
func TestAgentsRunConcurrently(t *testing.T) {
	const sims = 3
	var inFlight, peak int64

	agent := func(ctx context.Context, sim LeasedSim, opts RunOptions, emit EventSink) error {
		now := atomic.AddInt64(&inFlight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if now <= old || atomic.CompareAndSwapInt64(&peak, old, now) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)

		e := NewEvent("r", EventAgentFinished)
		e.UDID = sim.Device.UDID
		e.Status = RunPassed
		emit(e)
		return nil
	}

	orch := &Orchestrator{runDir: t.TempDir()}
	orch.sink = func(e Event) { orch.mu.Lock(); orch.run.Apply(e); orch.mu.Unlock() }

	var wg sync.WaitGroup
	for i := 0; i < sims; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sim := LeasedSim{Device: Device{UDID: string(rune('A' + i))}}
			orch.driveOne(context.Background(), sim, RunOptions{AgentCommand: agent}, "r")
		}(i)
	}
	wg.Wait()

	if peak < sims {
		t.Errorf("peak concurrency = %d, want %d: agents are running serially", peak, sims)
	}
}

// TestAgentErrorMarksSimulator makes sure a crashed agent can never leave its
// simulator stuck reporting "running" forever.
func TestAgentErrorMarksSimulator(t *testing.T) {
	orch := &Orchestrator{runDir: t.TempDir()}
	orch.sink = func(e Event) { orch.mu.Lock(); orch.run.Apply(e); orch.mu.Unlock() }

	failing := func(context.Context, LeasedSim, RunOptions, EventSink) error {
		return errFake
	}
	orch.driveOne(context.Background(), LeasedSim{Device: Device{UDID: "A"}},
		RunOptions{AgentCommand: failing}, "r")

	run := orch.snapshot()
	if len(run.Sims) != 1 || run.Sims[0].Status != RunError {
		t.Fatalf("sim status = %+v, want an error", run.Sims)
	}
	if run.Sims[0].Detail == "" {
		t.Error("the failure reason was not recorded")
	}
}

// TestAgentVerdictIsNotOverwritten: an agent that reported "failed" and then
// returned an error keeps its own verdict, since a real test failure is more
// informative than the wrapper's error.
func TestAgentVerdictIsNotOverwritten(t *testing.T) {
	orch := &Orchestrator{runDir: t.TempDir()}
	orch.sink = func(e Event) { orch.mu.Lock(); orch.run.Apply(e); orch.mu.Unlock() }

	agent := func(ctx context.Context, sim LeasedSim, opts RunOptions, emit EventSink) error {
		e := NewEvent("r", EventAgentFinished)
		e.UDID = sim.Device.UDID
		e.Status = RunFailed
		e.Detail = "login button never appeared"
		emit(e)
		return errFake
	}
	orch.driveOne(context.Background(), LeasedSim{Device: Device{UDID: "A"}},
		RunOptions{AgentCommand: agent}, "r")

	run := orch.snapshot()
	if run.Sims[0].Status != RunFailed {
		t.Errorf("status = %v, want the agent's own failed verdict", run.Sims[0].Status)
	}
	if run.Sims[0].Detail != "login button never appeared" {
		t.Errorf("detail = %q, want the agent's reason", run.Sims[0].Detail)
	}
}

// TestSnapshotIsADeepCopy: the GUI reads snapshots while agents are still
// writing, so a snapshot must not alias live state.
func TestSnapshotIsADeepCopy(t *testing.T) {
	orch := &Orchestrator{runDir: t.TempDir()}
	orch.sink = func(e Event) { orch.mu.Lock(); orch.run.Apply(e); orch.mu.Unlock() }
	orch.sink(Event{Type: EventStepStarted, UDID: "A", StepID: 1, Tool: "tap"})

	snap := orch.snapshot()
	orch.sink(Event{Type: EventStepFinished, UDID: "A", StepID: 1, Status: RunPassed})

	if snap.Sims[0].Steps[0].Status == RunPassed {
		t.Error("snapshot aliases live run state; later events mutated it")
	}
}

var errFake = fakeError("agent process exited 1")

type fakeError string

func (e fakeError) Error() string { return string(e) }

func marshalLine(e Event) ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// TestLeasePreparesConcurrently guards a regression that made setup dominate a
// run: preparing simulators one after another meant four devices spent ~160s
// booting serially before the first agent started, against ~65s of testing.
// Slimming and booting are mostly waiting on the simulator, so they overlap.
func TestLeasePreparesConcurrently(t *testing.T) {
	const sims = 4
	var inFlight, peak int64

	// prepareHook stands in for the real slim+boot+install, which needs real
	// simulators; the property under test is the scheduling, not the work.
	restore := prepareHook
	defer func() { prepareHook = restore }()
	prepareHook = func(d Device) {
		now := atomic.AddInt64(&inFlight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if now <= old || atomic.CompareAndSwapInt64(&peak, old, now) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
	}

	devices := make([]Device, sims)
	for i := range devices {
		devices[i] = Device{UDID: string(rune('A' + i)), Name: "sim", State: "Shutdown"}
	}

	orch := &Orchestrator{runDir: t.TempDir()}
	orch.sink = func(e Event) { orch.mu.Lock(); orch.run.Apply(e); orch.mu.Unlock() }

	leases, err := orch.leaseDevices(context.Background(), devices, RunOptions{}, "r")
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if len(leases) != sims {
		t.Fatalf("got %d leases, want %d", len(leases), sims)
	}
	if peak < sims {
		t.Errorf("peak concurrent preparations = %d, want %d: simulators are being prepared serially", peak, sims)
	}

	// Order must survive the concurrency so the fleet reads the same each run.
	for i, l := range leases {
		if l.Device.UDID != devices[i].UDID {
			t.Errorf("lease %d = %q, want %q: device order was not preserved", i, l.Device.UDID, devices[i].UDID)
		}
	}
}
