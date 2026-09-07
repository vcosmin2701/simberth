package simberth

// The orchestrator owns a run: it leases simulators, prepares them (slim, boot,
// install the app), starts one agent per simulator, folds their events into a
// live Run, and puts every simulator back the way it found it.
//
// Leasing is the safety property that matters. simslim's rule is that a
// destructive command resolves an exact UDID first so a simctl alias like `all`
// can never fan out; the same reasoning applies here — an agent is handed one
// resolved device and can never reach another one.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// EventSink receives events as they happen. The CLI writes NDJSON to stdout;
// tests collect them in a slice.
type EventSink func(Event)

// RunOptions configures one run.
type RunOptions struct {
	Scenario string // the natural-language goal handed to every agent
	AppPath  string // .app bundle to install; optional (a scenario may drive a stock app)
	SimCount int    // how many simulators to run in parallel
	// Devices pins the run to specific UDIDs. When empty, the orchestrator
	// picks SimCount shutdown simulators itself.
	Devices []string
	// Slim applies the service profile before booting. This is the whole point
	// of running on simberth, but it costs a reconfigure+reboot, so a fast
	// iteration loop can turn it off.
	Slim    bool
	Profile Profile
	// KeepBooted leaves simulators running after the run, for debugging.
	KeepBooted bool
	// AgentCommand runs one agent. It receives the prepared simulator and must
	// emit events through the sink. Injected so the orchestrator can be tested
	// without spawning a real agent.
	AgentCommand AgentFunc
	// RunDir is where events, screenshots and the replay file are written.
	// Empty means a directory under the user's Application Support.
	RunDir string
}

// AgentFunc drives one simulator for the length of a run.
type AgentFunc func(ctx context.Context, sim LeasedSim, opts RunOptions, emit EventSink) error

// LeasedSim is one simulator reserved for the duration of a run.
type LeasedSim struct {
	Device   Device
	BundleID string // the app under test, when one was installed
	RunDir   string // this run's directory, for screenshots
}

// Orchestrator runs scenarios across a fleet of simulators.
type Orchestrator struct {
	mu     sync.Mutex
	run    Run
	sink   EventSink
	runDir string
}

// NewRunID makes a sortable, human-scannable run identifier.
func NewRunID() string {
	return time.Now().Format("20060102-150405")
}

// DefaultRunRoot is where runs are persisted when no directory is given.
func DefaultRunRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "simberth", "runs"), nil
}

// Execute runs the scenario end to end and returns the completed Run. The error
// is non-nil only for a failure to run at all; a run that executed and failed
// its scenario reports that through Run.Status.
func Execute(ctx context.Context, opts RunOptions, sink EventSink) (Run, error) {
	if opts.Scenario == "" {
		return Run{}, fmt.Errorf("a run needs a scenario")
	}
	if opts.AgentCommand == nil {
		return Run{}, fmt.Errorf("a run needs an agent command")
	}
	if opts.SimCount <= 0 {
		opts.SimCount = 1
	}

	runID := NewRunID()
	runDir, err := prepareRunDir(opts, runID)
	if err != nil {
		return Run{}, err
	}

	o := &Orchestrator{runDir: runDir}
	// Every event is both streamed and persisted, so a run can be reopened
	// later without the producer still being alive.
	events, err := os.Create(filepath.Join(runDir, "events.ndjson"))
	if err != nil {
		return Run{}, fmt.Errorf("create event log: %w", err)
	}
	defer events.Close()
	encoder := json.NewEncoder(events)

	o.sink = func(e Event) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.run.Apply(e)
		_ = encoder.Encode(e) // a full disk must not abort a run in progress
		if sink != nil {
			sink(e)
		}
	}

	start := NewEvent(runID, EventRunStarted)
	start.Scenario = opts.Scenario
	start.AppPath = opts.AppPath
	start.SimCount = opts.SimCount
	o.sink(start)

	leases, err := o.lease(ctx, opts, runID)
	if err != nil {
		fail := NewEvent(runID, EventRunFinished)
		fail.Status = RunError
		fail.Detail = err.Error()
		o.sink(fail)
		return o.snapshot(), err
	}

	// Agents run concurrently: that is the entire reason for slimming.
	var wg sync.WaitGroup
	for _, lease := range leases {
		wg.Add(1)
		go func(sim LeasedSim) {
			defer wg.Done()
			o.driveOne(ctx, sim, opts, runID)
		}(lease)
	}
	wg.Wait()

	o.release(context.WithoutCancel(ctx), leases, opts, runID)

	final := o.snapshot()
	done := NewEvent(runID, EventRunFinished)
	done.Status = final.Verdict()
	o.sink(done)

	result := o.snapshot()
	if err := writeRunSummary(runDir, result); err != nil {
		return result, err
	}
	return result, nil
}

// driveOne runs a single agent and makes sure its simulator always reaches a
// terminal state, even if the agent crashes or the run is cancelled.
func (o *Orchestrator) driveOne(ctx context.Context, sim LeasedSim, opts RunOptions, runID string) {
	started := NewEvent(runID, EventAgentStarted)
	started.UDID = sim.Device.UDID
	started.SimName = sim.Device.Name
	o.sink(started)

	err := opts.AgentCommand(ctx, sim, opts, o.sink)
	if err == nil {
		return
	}

	// The agent reports its own verdict on success. An error here means it
	// never got to, so the simulator would otherwise be stuck "running".
	if o.simStatus(sim.Device.UDID) == RunRunning {
		e := NewEvent(runID, EventAgentFinished)
		e.UDID = sim.Device.UDID
		e.SimName = sim.Device.Name
		e.Status = RunError
		e.Detail = err.Error()
		o.sink(e)
	}
}

// lease reserves simulators and prepares each one for its agent.
//
// Preparation runs concurrently. Slimming and booting a simulator takes tens of
// seconds and is mostly waiting on the simulator, so doing them one after
// another made setup dominate the run: four simulators spent ~160s booting
// serially before the first agent could start, against ~65s of actual testing.
func (o *Orchestrator) lease(ctx context.Context, opts RunOptions, runID string) ([]LeasedSim, error) {
	devices, err := o.pickDevices(ctx, opts)
	if err != nil {
		return nil, err
	}
	return o.leaseDevices(ctx, devices, opts, runID)
}

// leaseDevices prepares an already-chosen set of devices, concurrently.
func (o *Orchestrator) leaseDevices(ctx context.Context, devices []Device, opts RunOptions, runID string) ([]LeasedSim, error) {
	type result struct {
		sim LeasedSim
		err error
	}
	results := make([]result, len(devices))

	var wg sync.WaitGroup
	for i, d := range devices {
		leased := NewEvent(runID, EventSimLeased)
		leased.UDID = d.UDID
		leased.SimName = d.Name
		o.sink(leased)

		wg.Add(1)
		go func(i int, d Device) {
			defer wg.Done()
			sim, err := o.prepare(ctx, d, opts)
			results[i] = result{sim: sim, err: err}
			if err != nil {
				return
			}
			ready := NewEvent(runID, EventSimReady)
			ready.UDID = d.UDID
			ready.SimName = d.Name
			o.sink(ready)
		}(i, d)
	}
	wg.Wait()

	// Collect in the original order so the fleet reads the same way every run.
	leases := make([]LeasedSim, 0, len(devices))
	var failure error
	for i, r := range results {
		if r.err != nil {
			if failure == nil {
				failure = fmt.Errorf("prepare %s (%s): %w", devices[i].Name, devices[i].UDID, r.err)
			}
			continue
		}
		leases = append(leases, r.sim)
	}

	if failure != nil {
		// Release whatever did come up: a half-leased fleet would leave
		// simulators booted and slimmed with nothing driving them.
		o.release(context.WithoutCancel(ctx), leases, opts, runID)
		return nil, failure
	}
	return leases, nil
}

// pickDevices resolves the run's simulators. Explicit UDIDs are resolved
// exactly; otherwise shutdown simulators are chosen so a run never commandeers
// a simulator someone is already using.
func (o *Orchestrator) pickDevices(ctx context.Context, opts RunOptions) ([]Device, error) {
	if len(opts.Devices) > 0 {
		devices := make([]Device, 0, len(opts.Devices))
		for _, udid := range opts.Devices {
			d, err := FindDevice(ctx, udid, "")
			if err != nil {
				return nil, err
			}
			devices = append(devices, d)
		}
		return devices, nil
	}

	all, err := ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	var available []Device
	for _, d := range all {
		if d.State == "Shutdown" {
			available = append(available, d)
		}
	}
	// Newest runtime first: a run should land on a modern OS by default.
	sort.SliceStable(available, func(i, j int) bool {
		return available[i].OSVersion > available[j].OSVersion
	})
	if len(available) < opts.SimCount {
		return nil, fmt.Errorf("need %d shutdown simulators, found %d; shut some down or pass --devices",
			opts.SimCount, len(available))
	}
	return available[:opts.SimCount], nil
}

// prepareHook is called at the start of prepare. It is nil in production and
// set by tests that need to exercise scheduling without real simulators.
var prepareHook func(Device)

// prepare slims, boots, and installs the app under test.
func (o *Orchestrator) prepare(ctx context.Context, d Device, opts RunOptions) (LeasedSim, error) {
	sim := LeasedSim{Device: d, RunDir: o.runDir}
	if prepareHook != nil {
		prepareHook(d)
		return sim, nil
	}

	if opts.Slim {
		if _, err := EnableSlim(ctx, d.Set, d.UDID, opts.Profile, nil); err != nil {
			return sim, fmt.Errorf("slim: %w", err)
		}
	}
	// EnableSlim boots as part of its work; booting again is a no-op that also
	// covers the un-slimmed path.
	if err := BootAndWait(ctx, d.Set, d.UDID); err != nil {
		return sim, fmt.Errorf("boot: %w", err)
	}

	if opts.AppPath != "" {
		if err := InstallApp(ctx, d.Set, d.UDID, opts.AppPath); err != nil {
			return sim, fmt.Errorf("install: %w", err)
		}
		bundleID, err := BundleIDForApp(ctx, opts.AppPath)
		if err != nil {
			return sim, err
		}
		sim.BundleID = bundleID
		if err := LaunchApp(ctx, d.Set, d.UDID, bundleID); err != nil {
			return sim, fmt.Errorf("launch %s: %w", bundleID, err)
		}
	}
	return sim, nil
}

// release returns every leased simulator to its prior state. It runs on an
// uncancelled context so a Ctrl-C still cleans up.
func (o *Orchestrator) release(ctx context.Context, leases []LeasedSim, opts RunOptions, runID string) {
	for _, sim := range leases {
		if !opts.KeepBooted {
			_ = Shutdown(ctx, sim.Device.Set, sim.Device.UDID)
		}
		e := NewEvent(runID, EventSimReleased)
		e.UDID = sim.Device.UDID
		e.SimName = sim.Device.Name
		o.sink(e)
	}
}

func (o *Orchestrator) simStatus(udid string) RunStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, s := range o.run.Sims {
		if s.UDID == udid {
			return s.Status
		}
	}
	return RunRunning
}

// snapshot returns a copy safe to read while agents are still emitting.
func (o *Orchestrator) snapshot() Run {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := o.run
	out.Sims = make([]*SimRun, len(o.run.Sims))
	for i, s := range o.run.Sims {
		clone := *s
		clone.Steps = append([]Step(nil), s.Steps...)
		out.Sims[i] = &clone
	}
	return out
}

func prepareRunDir(opts RunOptions, runID string) (string, error) {
	root := opts.RunDir
	if root == "" {
		defaultRoot, err := DefaultRunRoot()
		if err != nil {
			return "", err
		}
		root = filepath.Join(defaultRoot, runID)
	}
	if err := os.MkdirAll(filepath.Join(root, "screenshots"), 0o755); err != nil {
		return "", fmt.Errorf("create run directory: %w", err)
	}
	return root, nil
}

func writeRunSummary(dir string, run Run) error {
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "run.json"), append(data, '\n'), 0o644)
}

// LoadRun reconstructs a run by replaying its event log, so a finished run can
// be reopened without the process that produced it.
func LoadRun(dir string) (Run, error) {
	data, err := os.ReadFile(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		return Run{}, err
	}
	var run Run
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		e, err := DecodeEvent([]byte(line))
		if err != nil {
			continue // a truncated final line from a killed run is not fatal
		}
		run.Apply(e)
	}
	return run, nil
}
