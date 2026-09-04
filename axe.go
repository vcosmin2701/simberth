package simberth

// AXe (https://github.com/cameroncooke/AXe) is the only place simberth drives a
// simulator's UI, exactly as simctl.go is the only place it shells out to
// `xcrun`. Keeping every AXe invocation here means the tool can be swapped for
// idb or XCUITest without touching the orchestrator or the agent runner.
//
// AXe holds no daemon: every command takes --udid and runs as its own process,
// which is what makes one agent per simulator safe to run concurrently.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// AXeBinary is the AXe executable. SIMBERTH_AXE overrides it, mirroring the
// SIMSLIM_CLI override the macOS app already uses to point at a local build.
// AXe is a Homebrew dependency rather than a bundled one, so it is resolved
// from PATH and its absence is reported by `doctor`, not discovered mid-run.
func AXeBinary() string {
	if override := strings.TrimSpace(os.Getenv("SIMBERTH_AXE")); override != "" {
		return override
	}
	return "axe"
}

// UITimeout bounds a single AXe invocation. Reading a full accessibility tree
// on a busy machine takes a couple of seconds; anything past this is a wedged
// simulator, and the agent gets an error instead of hanging its whole run.
var UITimeout = 60 * time.Second

// Element is one actionable thing on screen: the distilled form of an
// accessibility node that an agent can reason about and act on.
//
// The raw tree is far too large to hand to a model — a stock iOS 26 home screen
// is ~450 KB of JSON (~113k tokens) across 297 nodes, of which only ~49 carry a
// label and 221 are anonymous layout groups. DescribeUI returns this shape
// instead, which is ~75x smaller and loses nothing an agent can act on.
type Element struct {
	Type  string `json:"type"`            // Button, TextField, Switch, …
	Label string `json:"label,omitempty"` // AXLabel — the preferred tap selector
	ID    string `json:"id,omitempty"`    // AXUniqueId (accessibilityIdentifier)
	Value string `json:"value,omitempty"` // AXValue: field contents, switch state
	X     int    `json:"x"`               // activation point, for when no selector matches
	Y     int    `json:"y"`
}

// Screen is one distilled snapshot of a simulator's UI.
type Screen struct {
	UDID     string    `json:"udid"`
	Elements []Element `json:"elements"`
	// Raw is the untouched AXe tree. Omitted from the agent-facing payload and
	// populated only when a caller explicitly asks, so a step can be debugged
	// after the fact without every turn paying for it.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// interactiveTypes are the accessibility types worth showing an agent. Groups,
// Images and other decoration are dropped: they are the bulk of the tree and
// nothing can be done to them.
var interactiveTypes = map[string]bool{
	"Button":           true,
	"TextField":        true,
	"SecureTextField":  true,
	"SearchField":      true,
	"Switch":           true,
	"Slider":           true,
	"Link":             true,
	"StaticText":       true,
	"Cell":             true,
	"PickerWheel":      true,
	"Stepper":          true,
	"SegmentedControl": true,
	"TabBar":           true,
	"NavigationBar":    true,
	"Alert":            true,
	"Sheet":            true,
}

// axNode mirrors the fields simberth reads from AXe's describe-ui output. The
// tree carries many more keys per node; anything not named here is ignored.
type axNode struct {
	Type     string   `json:"type"`
	AXLabel  *string  `json:"AXLabel"`
	AXUnique *string  `json:"AXUniqueId"`
	AXValue  *string  `json:"AXValue"`
	Frame    axFrame  `json:"frame"`
	Children []axNode `json:"children"`
}

type axFrame struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// DescribeUI reads the simulator's accessibility tree and distills it to the
// elements an agent can act on. Pass keepRaw to also return the full tree.
func DescribeUI(ctx context.Context, udid string, keepRaw bool) (Screen, error) {
	out, err := runAXe(ctx, "describe-ui", "--udid", udid)
	if err != nil {
		return Screen{}, err
	}

	roots, err := parseTree(out)
	if err != nil {
		return Screen{}, err
	}

	screen := Screen{UDID: udid, Elements: distill(roots)}
	if keepRaw {
		screen.Raw = json.RawMessage(out)
	}
	return screen, nil
}

// parseTree decodes AXe's describe-ui output, which is an array of root nodes
// (one per application layer on screen).
func parseTree(out []byte) ([]axNode, error) {
	var roots []axNode
	if err := json.Unmarshal(out, &roots); err != nil {
		return nil, fmt.Errorf("parse accessibility tree: %w", err)
	}
	return roots, nil
}

// distill walks the tree depth-first and keeps the nodes an agent can act on.
// Document order is preserved so the list reads roughly top-to-bottom, which is
// how a person would describe the screen.
func distill(roots []axNode) []Element {
	elements := []Element{}
	var walk func(n axNode)
	walk = func(n axNode) {
		label := deref(n.AXLabel)
		id := deref(n.AXUnique)
		// An element is only addressable if it has a selector; an unlabeled,
		// unidentified button cannot be described back to the agent anyway.
		if interactiveTypes[n.Type] && (label != "" || id != "") {
			elements = append(elements, Element{
				Type:  n.Type,
				Label: label,
				ID:    id,
				Value: deref(n.AXValue),
				X:     int(n.Frame.X + n.Frame.Width/2),
				Y:     int(n.Frame.Y + n.Frame.Height/2),
			})
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return elements
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// Target selects an element to act on. Prefer Label or ID: a selector survives
// layout changes, which is what lets a recorded run replay on a different
// device size. Coordinates are the fallback when nothing is addressable.
type Target struct {
	Label       string
	ID          string
	Value       string
	ElementType string        // narrows an ambiguous selector, e.g. "Button"
	X, Y        int           // used only when no selector is set
	WaitTimeout time.Duration // poll for the element before failing
}

// args renders the target as AXe flags. Coordinates win in AXe when both are
// given, so they are only emitted when no selector is present.
func (t Target) args() ([]string, error) {
	switch {
	case t.Label != "":
		return t.selectorArgs("--label", t.Label), nil
	case t.ID != "":
		return t.selectorArgs("--id", t.ID), nil
	case t.Value != "":
		return t.selectorArgs("--value", t.Value), nil
	case t.X != 0 || t.Y != 0:
		return []string{"-x", strconv.Itoa(t.X), "-y", strconv.Itoa(t.Y)}, nil
	default:
		return nil, fmt.Errorf("target needs a label, id, value, or coordinates")
	}
}

func (t Target) selectorArgs(flag, value string) []string {
	args := []string{flag, value}
	if t.ElementType != "" {
		args = append(args, "--element-type", t.ElementType)
	}
	if t.WaitTimeout > 0 {
		args = append(args, "--wait-timeout", strconv.FormatFloat(t.WaitTimeout.Seconds(), 'f', -1, 64))
	}
	return args
}

// Tap taps an element, or a point when the target carries no selector.
func Tap(ctx context.Context, udid string, t Target) error {
	args, err := t.args()
	if err != nil {
		return err
	}
	_, err = runAXe(ctx, append([]string{"tap", "--udid", udid}, args...)...)
	return err
}

// TypeText types into whatever currently has keyboard focus. Tap the field
// first; AXe has no combined focus-and-type command.
func TypeText(ctx context.Context, udid, text string) error {
	_, err := runAXe(ctx, "type", text, "--udid", udid)
	return err
}

// Swipe drags from one point to another, for scrolling and page gestures.
func Swipe(ctx context.Context, udid string, fromX, fromY, toX, toY int, duration time.Duration) error {
	args := []string{
		"swipe", "--udid", udid,
		"--start-x", strconv.Itoa(fromX), "--start-y", strconv.Itoa(fromY),
		"--end-x", strconv.Itoa(toX), "--end-y", strconv.Itoa(toY),
	}
	if duration > 0 {
		args = append(args, "--duration", strconv.FormatFloat(duration.Seconds(), 'f', -1, 64))
	}
	_, err := runAXe(ctx, args...)
	return err
}

// Button presses a hardware button: home, lock, side-button, siri, apple-pay.
func Button(ctx context.Context, udid, button string) error {
	_, err := runAXe(ctx, "button", button, "--udid", udid)
	return err
}

// Screenshot captures the display to a PNG. Every agent step takes one, so the
// run timeline can be reviewed after the fact.
func Screenshot(ctx context.Context, udid, path string) error {
	_, err := runAXe(ctx, "screenshot", "--udid", udid, "--output", path)
	return err
}

// runAXe executes one AXe command. AXe writes its payload to stdout and
// diagnostics to stderr, so a failure reports stderr, which is the part that
// says what actually went wrong.
func runAXe(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, UITimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, AXeBinary(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("axe %s timed out after %s", args[0], UITimeout)
		}
		// A missing binary is checked before stderr: it is a setup problem with
		// a specific fix, not a simulator problem, and the raw exec error alone
		// doesn't say how to resolve it. A bare command name that isn't on PATH
		// fails as *exec.Error; an absolute SIMBERTH_AXE path that doesn't exist
		// fails as *fs.PathError, so both are treated the same way.
		var execErr *exec.Error
		var pathErr *fs.PathError
		if errors.As(err, &execErr) || errors.As(err, &pathErr) {
			return nil, fmt.Errorf("axe not found at %q; install it with `brew install cameroncooke/axe/axe` or set SIMBERTH_AXE: %w", AXeBinary(), err)
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("axe %s: %s", args[0], detail)
		}
		return nil, fmt.Errorf("axe %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}
