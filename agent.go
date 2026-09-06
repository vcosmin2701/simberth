package simberth

// Spawning the agent runner. The Claude Agent SDK ships for TypeScript and
// Python only, so each agent is a Node child process rather than in-process Go.
// That turns out to be the right shape anyway: one process per simulator gives
// hard isolation, and a crashed agent takes down only its own simulator.
//
// The child speaks the same Event stream defined in run.go, one JSON object per
// line on stdout, so its output folds into the run exactly like events the
// orchestrator produces itself.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// NodeBinary is the Node runtime used for the agent. SIMBERTH_NODE overrides
// it, following the SIMSLIM_CLI precedent for a dependency resolved by PATH.
func NodeBinary() string {
	if override := strings.TrimSpace(os.Getenv("SIMBERTH_NODE")); override != "" {
		return override
	}
	return "node"
}

// AgentEntrypoint is the agent runner's main module. SIMBERTH_AGENT overrides
// it; otherwise it is resolved next to the executable (the app bundle case) or
// relative to the source tree (the development case).
func AgentEntrypoint() string {
	if override := strings.TrimSpace(os.Getenv("SIMBERTH_AGENT")); override != "" {
		return override
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "agent", "src", "index.mjs")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if wd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(wd, "agent", "src", "index.mjs")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "agent/src/index.mjs"
}

// agentConfig is the JSON argument handed to the runner.
type agentConfig struct {
	UDID     string `json:"udid"`
	SimName  string `json:"simName"`
	RunID    string `json:"runId"`
	Scenario string `json:"scenario"`
	RunDir   string `json:"runDir"`
	BundleID string `json:"bundleId,omitempty"`
	MaxTurns int    `json:"maxTurns,omitempty"`
	Model    string `json:"model,omitempty"`
}

// AgentSettings tunes the spawned agent.
type AgentSettings struct {
	MaxTurns int
	Model    string
}

// ClaudeAgent returns an AgentFunc that drives one simulator with a real Claude
// agent. It is injected into RunOptions so tests can substitute a fake.
func ClaudeAgent(settings AgentSettings) AgentFunc {
	return func(ctx context.Context, sim LeasedSim, opts RunOptions, emit EventSink) error {
		cfg := agentConfig{
			UDID:     sim.Device.UDID,
			SimName:  sim.Device.Name,
			RunID:    filepath.Base(sim.RunDir),
			Scenario: opts.Scenario,
			RunDir:   sim.RunDir,
			BundleID: sim.BundleID,
			MaxTurns: settings.MaxTurns,
			Model:    settings.Model,
		}
		payload, err := json.Marshal(cfg)
		if err != nil {
			return err
		}

		cmd := exec.CommandContext(ctx, NodeBinary(), AgentEntrypoint(), string(payload))
		cmd.Env = os.Environ()

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			if isMissingBinary(err) {
				return fmt.Errorf("node not found; install Node 18+ or set SIMBERTH_NODE: %w", err)
			}
			return fmt.Errorf("start agent: %w", err)
		}

		// The agent's stderr is diagnostics, not events. It is captured so a
		// crash can be explained rather than reported as a bare exit code.
		var diagnostics strings.Builder
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				diagnostics.WriteString(scanner.Text())
				diagnostics.WriteString("\n")
			}
		}()

		replaySteps := readAgentEvents(stdout, sim, emit)
		wg.Wait()

		if err := cmd.Wait(); err != nil {
			detail := strings.TrimSpace(diagnostics.String())
			if detail != "" {
				return fmt.Errorf("agent exited: %w: %s", err, lastLines(detail, 5))
			}
			return fmt.Errorf("agent exited: %w", err)
		}

		if len(replaySteps) > 0 {
			if err := writeReplay(sim, opts, replaySteps); err != nil {
				return err
			}
		}
		return nil
	}
}

// readAgentEvents folds the child's NDJSON into the run and picks out the
// replay record, which is carried on the stream rather than written by the
// child so the orchestrator stays the only writer of run files.
func readAgentEvents(stdout io.Reader, sim LeasedSim, emit EventSink) []ReplayStep {
	var replay []ReplayStep

	scanner := bufio.NewScanner(stdout)
	// The distilled screen is small, but an agent's reasoning text can exceed
	// bufio's default 64 KB line limit.
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		// The replay record shares the stream but is not a run event.
		var probe struct {
			Type  string       `json:"type"`
			Steps []ReplayStep `json:"steps"`
		}
		if err := json.Unmarshal(line, &probe); err == nil && probe.Type == "replay" {
			replay = probe.Steps
			continue
		}

		e, err := DecodeEvent(line)
		if err != nil {
			continue // a non-event line from the child is not fatal
		}
		// The child knows its own UDID, but trust the lease: an event can only
		// ever be attributed to the simulator this agent was given.
		e.UDID = sim.Device.UDID
		e.SimName = sim.Device.Name
		emit(e)
	}
	return replay
}

func writeReplay(sim LeasedSim, opts RunOptions, steps []ReplayStep) error {
	replay := Replay{
		RunID:    filepath.Base(sim.RunDir),
		Scenario: opts.Scenario,
		AppPath:  opts.AppPath,
		Steps:    steps,
	}
	data, err := json.MarshalIndent(replay, "", "  ")
	if err != nil {
		return err
	}
	// One replay file per simulator: agents explore differently, so their
	// recordings differ even within a single run.
	name := fmt.Sprintf("replay-%s.json", sim.Device.UDID)
	return os.WriteFile(filepath.Join(sim.RunDir, name), append(data, '\n'), 0o644)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "; ")
	}
	return strings.Join(lines[len(lines)-n:], "; ")
}

func isMissingBinary(err error) bool {
	var execErr *exec.Error
	var pathErr *fs.PathError
	return errors.As(err, &execErr) || errors.As(err, &pathErr)
}
