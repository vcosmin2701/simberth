package main

// `simberth run` executes a scenario across a fleet of simulators, and
// `simberth replay` re-runs a recorded one with no model in the loop.
//
// Following the house rule: machine-readable JSON goes to stdout, human
// progress goes to stderr, so a consumer reading a combined stream still gets
// clean NDJSON.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cli "github.com/urfave/cli/v3"

	"github.com/vcosmin2701/simberth"
)

func runCommand(jsonFlag func() cli.Flag) *cli.Command {
	return &cli.Command{
		Name:      "run",
		Usage:     "drive simulators with agents against a scenario",
		ArgsUsage: "",
		Flags: []cli.Flag{
			jsonFlag(),
			&cli.StringFlag{Name: "scenario", Usage: "what the agent should verify, in plain English", Required: true},
			&cli.StringFlag{Name: "app", Usage: "path to a simulator .app bundle to install and launch"},
			&cli.IntFlag{Name: "sims", Usage: "how many simulators to run in parallel", Value: 1},
			&cli.StringFlag{Name: "devices", Usage: "comma-separated UDIDs to use instead of picking automatically"},
			&cli.BoolFlag{Name: "slim", Usage: "slim each simulator before booting it", Value: true},
			&cli.StringFlag{Name: "profile", Usage: "JSON slim profile (mutually exclusive with --except/--keep)"},
			&cli.StringFlag{Name: "except", Usage: "comma-separated categories to leave enabled when slimming"},
			&cli.StringFlag{Name: "keep", Usage: "comma-separated launchd labels to keep running"},
			&cli.BoolFlag{Name: "keep-booted", Usage: "leave simulators booted after the run, for debugging"},
			&cli.IntFlag{Name: "max-turns", Usage: "cap the agent's tool-use turns", Value: 40},
			&cli.StringFlag{Name: "model", Usage: "Claude model for the agents"},
			&cli.StringFlag{Name: "run-dir", Usage: "where to write events, screenshots and replays"},
		},
		Action: cmdRun,
	}
}

func cmdRun(ctx context.Context, cmd *cli.Command) error {
	jsonOutput := cmd.Bool("json")

	profile, err := simberth.BuildProfile(
		cmd.String("profile"), cmd.String("except"), cmd.String("keep"))
	if err != nil {
		return err
	}

	opts := simberth.RunOptions{
		Scenario:   cmd.String("scenario"),
		AppPath:    cmd.String("app"),
		SimCount:   cmd.Int("sims"),
		Devices:    simberth.SplitList(cmd.String("devices")),
		Slim:       cmd.Bool("slim"),
		Profile:    profile,
		KeepBooted: cmd.Bool("keep-booted"),
		RunDir:     cmd.String("run-dir"),
		AgentCommand: simberth.ClaudeAgent(simberth.AgentSettings{
			MaxTurns: cmd.Int("max-turns"),
			Model:    cmd.String("model"),
		}),
	}
	if opts.AppPath != "" {
		if opts.AppPath, err = filepath.Abs(opts.AppPath); err != nil {
			return err
		}
	}

	// Under --json the event stream itself is the output, so nothing else may
	// touch stdout. Otherwise events are rendered for a person on stderr.
	stdout := bufio.NewWriter(os.Stdout)
	defer stdout.Flush()
	encoder := json.NewEncoder(stdout)
	// A step's summary rides on step.started; the matching finish carries only
	// the outcome, so the renderer remembers it to print one complete line.
	summaries := map[string]string{}
	var mu sync.Mutex
	sink := func(e simberth.Event) {
		if jsonOutput {
			_ = encoder.Encode(e)
			// Flushed per event: a consumer showing a run live must see each
			// step as it happens, not when the buffer happens to fill.
			_ = stdout.Flush()
			return
		}
		mu.Lock()
		key := fmt.Sprintf("%s/%d", e.UDID, e.StepID)
		switch e.Type {
		case simberth.EventStepStarted:
			summaries[key] = e.Summary
		case simberth.EventStepFinished:
			if e.Summary == "" {
				e.Summary = summaries[key]
			}
			delete(summaries, key)
		}
		mu.Unlock()

		if line := humanEvent(e); line != "" {
			fmt.Fprintln(os.Stderr, line)
		}
	}

	run, err := simberth.Execute(ctx, opts, sink)
	if err != nil {
		return err
	}

	if !jsonOutput {
		fmt.Fprintln(os.Stderr)
		printRunSummary(run)
	}
	// A failed scenario is a real result, but the process must exit non-zero so
	// CI notices. os.Exit skips deferred calls, so the final events -- including
	// run.finished -- are flushed first; otherwise a consumer never learns the
	// run ended and shows it as still running.
	if run.Status != simberth.RunPassed {
		_ = stdout.Flush()
		os.Exit(1)
	}
	return nil
}

// humanEvent renders an event as a progress line, or "" for events that are
// only interesting to a machine.
func humanEvent(e simberth.Event) string {
	sim := e.SimName
	if sim == "" {
		sim = e.UDID
	}
	switch e.Type {
	case simberth.EventRunStarted:
		return fmt.Sprintf("run %s: %s", e.RunID, e.Scenario)
	case simberth.EventSimLeased:
		return fmt.Sprintf("  %s: preparing", sim)
	case simberth.EventSimReady:
		return fmt.Sprintf("  %s: ready", sim)
	case simberth.EventStepFinished:
		mark := "ok"
		if e.Status != simberth.RunPassed {
			mark = "FAILED"
		}
		return fmt.Sprintf("  %s: %s (%s, %dms)", sim, e.Summary, mark, e.DurationMS)
	case simberth.EventAgentFinished:
		return fmt.Sprintf("  %s: %s — %s", sim, strings.ToUpper(string(e.Status)), e.Detail)
	case simberth.EventError:
		return fmt.Sprintf("  %s: error: %s", sim, e.Detail)
	}
	return ""
}

func printRunSummary(run simberth.Run) {
	fmt.Fprintf(os.Stderr, "%s: %s\n", run.ID, strings.ToUpper(string(run.Status)))
	for _, sim := range run.Sims {
		passed := 0
		for _, s := range sim.Steps {
			if s.Status == simberth.RunPassed {
				passed++
			}
		}
		fmt.Fprintf(os.Stderr, "  %-24s %-7s %d/%d steps  %s\n",
			sim.SimName, strings.ToUpper(string(sim.Status)), passed, len(sim.Steps), sim.Detail)
	}
}

func replayCommand(jsonFlag func() cli.Flag) *cli.Command {
	return &cli.Command{
		Name:      "replay",
		Usage:     "re-run a recorded run deterministically, with no model",
		ArgsUsage: "<replay.json>",
		Flags: []cli.Flag{
			jsonFlag(),
			&cli.StringFlag{Name: "device", Usage: "UDID to replay on (default: the first booted simulator)"},
			&cli.BoolFlag{Name: "keep-booted", Usage: "leave the simulator booted afterwards"},
		},
		Action: cmdReplay,
	}
}

func cmdReplay(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) != 1 {
		return fmt.Errorf("replay takes one replay file")
	}

	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var replay simberth.Replay
	if err := json.Unmarshal(data, &replay); err != nil {
		return fmt.Errorf("parse %s: %w", args[0], err)
	}
	if len(replay.Steps) == 0 {
		return fmt.Errorf("%s records no steps", args[0])
	}

	device, err := resolveReplayDevice(ctx, cmd.String("device"))
	if err != nil {
		return err
	}

	if err := simberth.BootAndWait(ctx, device.Set, device.UDID); err != nil {
		return err
	}
	if !cmd.Bool("keep-booted") {
		defer func() { _ = simberth.Shutdown(context.WithoutCancel(ctx), device.Set, device.UDID) }()
	}

	if replay.AppPath != "" {
		if err := simberth.InstallApp(ctx, device.Set, device.UDID, replay.AppPath); err != nil {
			return err
		}
		bundleID, err := simberth.BundleIDForApp(ctx, replay.AppPath)
		if err != nil {
			return err
		}
		if err := simberth.LaunchApp(ctx, device.Set, device.UDID, bundleID); err != nil {
			return err
		}
	}

	results := make([]replayResult, 0, len(replay.Steps))
	failed := false
	for i, step := range replay.Steps {
		started := time.Now()
		err := applyReplayStep(ctx, device.UDID, step)
		result := replayResult{
			Index:      i + 1,
			Tool:       step.Tool,
			Summary:    step.Summary,
			DurationMS: time.Since(started).Milliseconds(),
			Status:     string(simberth.RunPassed),
		}
		if err != nil {
			result.Status = string(simberth.RunFailed)
			result.Detail = err.Error()
			failed = true
		}
		results = append(results, result)

		if !cmd.Bool("json") {
			fmt.Fprintf(os.Stderr, "  %2d. %-40s %s\n", result.Index, result.Summary, result.Status)
		}
		if err != nil {
			break // a replay is a fixed sequence; once it diverges the rest is meaningless
		}
	}

	if cmd.Bool("json") {
		if err := writeJSON(map[string]any{
			"runId": replay.RunID,
			"steps": results,
			"status": map[bool]string{
				true: string(simberth.RunFailed), false: string(simberth.RunPassed),
			}[failed],
		}); err != nil {
			return err
		}
	}
	if failed {
		os.Exit(1)
	}
	return nil
}

type replayResult struct {
	Index      int    `json:"index"`
	Tool       string `json:"tool"`
	Summary    string `json:"summary,omitempty"`
	Status     string `json:"status"`
	DurationMS int64  `json:"durationMs"`
	Detail     string `json:"detail,omitempty"`
}

func applyReplayStep(ctx context.Context, udid string, step simberth.ReplayStep) error {
	switch step.Tool {
	case "tap":
		return simberth.Tap(ctx, udid, step.Target())
	case "type_text":
		return simberth.TypeText(ctx, udid, step.Text)
	case "swipe":
		return simberth.Swipe(ctx, udid, step.StartX, step.StartY, step.EndX, step.EndY, 0)
	case "press_button":
		return simberth.Button(ctx, udid, step.Button)
	default:
		return fmt.Errorf("unknown replay step %q", step.Tool)
	}
}

func resolveReplayDevice(ctx context.Context, udid string) (simberth.Device, error) {
	if udid != "" {
		return simberth.FindDevice(ctx, udid, "")
	}
	devices, err := simberth.ListDevices(ctx)
	if err != nil {
		return simberth.Device{}, err
	}
	for _, d := range devices {
		if d.State == "Booted" {
			return d, nil
		}
	}
	return simberth.Device{}, fmt.Errorf("no booted simulator; boot one or pass --device")
}
