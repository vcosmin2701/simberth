package simberth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNodeBinaryOverride(t *testing.T) {
	if got := NodeBinary(); got != "node" {
		t.Errorf("default = %q, want node", got)
	}
	t.Setenv("SIMBERTH_NODE", "/opt/node/bin/node")
	if got := NodeBinary(); got != "/opt/node/bin/node" {
		t.Errorf("override = %q", got)
	}
}

// TestAgentEntrypointPrefersExecutableDir is a regression test for a real
// deployment bug: the CLI resolved no entrypoint inside the app bundle, so
// every run from the app died with MODULE_NOT_FOUND. build-app.sh now copies
// the runner next to the CLI, and that location must win over the working
// directory — which in a launched app is "/" and has no agent at all.
func TestAgentEntrypointPrefersExecutableDir(t *testing.T) {
	t.Setenv("SIMBERTH_AGENT", "")

	exe, err := os.Executable()
	if err != nil {
		t.Skip("cannot resolve the test binary")
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	beside := filepath.Join(filepath.Dir(exe), "agent", "src")
	if err := os.MkdirAll(beside, 0o755); err != nil {
		t.Skipf("cannot write beside the test binary: %v", err)
	}
	defer os.RemoveAll(filepath.Join(filepath.Dir(exe), "agent"))

	entry := filepath.Join(beside, "index.mjs")
	if err := os.WriteFile(entry, []byte("// test\n"), 0o644); err != nil {
		t.Skipf("cannot write entrypoint: %v", err)
	}

	if got := AgentEntrypoint(); got != entry {
		t.Errorf("AgentEntrypoint() = %q, want the copy beside the executable (%q)", got, entry)
	}
}

func TestAgentEntrypointOverride(t *testing.T) {
	t.Setenv("SIMBERTH_AGENT", "/custom/agent.mjs")
	if got := AgentEntrypoint(); got != "/custom/agent.mjs" {
		t.Errorf("override = %q", got)
	}
}

// TestCurrentExecutableIsAbsolute backs the second half of the same bug: the
// agent shells back into this CLI, and inside the bundle nothing is on PATH, so
// a bare name would fail with ENOENT.
func TestCurrentExecutableIsAbsolute(t *testing.T) {
	got := currentExecutable()
	if !filepath.IsAbs(got) {
		t.Errorf("currentExecutable() = %q, want an absolute path so the child never depends on PATH", got)
	}
}

func TestLastLines(t *testing.T) {
	in := "one\ntwo\nthree\nfour"
	if got := lastLines(in, 2); got != "three; four" {
		t.Errorf("lastLines = %q, want %q", got, "three; four")
	}
	if got := lastLines("only", 5); got != "only" {
		t.Errorf("lastLines = %q, want %q", got, "only")
	}
}

// TestReadAgentEventsSeparatesReplay covers the stream carrying two kinds of
// line: run events, which fold into the run, and the single replay record.
func TestReadAgentEventsSeparatesReplay(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"step.started","stepId":1,"tool":"tap","summary":"tap \"Sign In\""}`,
		`{"type":"step.finished","stepId":1,"status":"passed","durationMs":40}`,
		`not json at all`,
		`{"type":"replay","steps":[{"tool":"tap","label":"Sign In"}]}`,
		`{"type":"agent.finished","status":"passed","detail":"done"}`,
	}, "\n")

	var events []Event
	sim := LeasedSim{Device: Device{UDID: "REAL-UDID", Name: "iPhone 17"}}
	replay := readAgentEvents(strings.NewReader(stream), sim, func(e Event) {
		events = append(events, e)
	})

	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (the replay line and the garbage line are not events)", len(events))
	}
	if len(replay) != 1 || replay[0].Label != "Sign In" {
		t.Errorf("replay = %+v, want the recorded tap", replay)
	}
	// Attribution comes from the lease, never from what the child claims.
	for _, e := range events {
		if e.UDID != "REAL-UDID" || e.SimName != "iPhone 17" {
			t.Errorf("event attributed to %q/%q, want the leased device", e.UDID, e.SimName)
		}
	}
}
