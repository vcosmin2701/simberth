package simberth

import (
	"strings"
	"testing"
	"time"
)

// sampleTree mirrors the shape AXe's describe-ui actually emits: an Application
// root, deeply nested anonymous Groups, and a handful of labelled controls.
const sampleTree = `[
  {
    "AXLabel": " ",
    "type": "Application",
    "children": [
      {
        "type": "Group",
        "AXLabel": null,
        "AXUniqueId": null,
        "frame": {"x": 0, "y": 0, "width": 402, "height": 874},
        "children": [
          {
            "type": "Button",
            "AXLabel": "Sign In",
            "AXUniqueId": "signin-button",
            "AXValue": null,
            "frame": {"x": 100, "y": 200, "width": 100, "height": 40},
            "children": []
          },
          {
            "type": "TextField",
            "AXLabel": null,
            "AXUniqueId": "email-field",
            "AXValue": "user@example.com",
            "frame": {"x": 20, "y": 100, "width": 360, "height": 44},
            "children": []
          },
          {
            "type": "Image",
            "AXLabel": "Decorative logo",
            "AXUniqueId": null,
            "frame": {"x": 0, "y": 0, "width": 402, "height": 80},
            "children": []
          },
          {
            "type": "Button",
            "AXLabel": null,
            "AXUniqueId": null,
            "frame": {"x": 0, "y": 0, "width": 10, "height": 10},
            "children": []
          }
        ]
      }
    ]
  }
]`

// TestDistillKeepsOnlyAddressableControls is the core guarantee of the AXe
// layer: the agent-facing payload keeps every actionable element and drops the
// decoration and anonymous layout nodes that make up the bulk of the tree.
func TestDistillKeepsOnlyAddressableControls(t *testing.T) {
	elements := distillJSON(t, sampleTree)

	if len(elements) != 2 {
		t.Fatalf("got %d elements, want 2 (Sign In button + email field): %+v", len(elements), elements)
	}

	// Document order is preserved so the list reads top-to-bottom.
	if elements[0].Label != "Sign In" || elements[0].ID != "signin-button" {
		t.Errorf("first element = %+v, want the Sign In button", elements[0])
	}
	if elements[1].ID != "email-field" || elements[1].Value != "user@example.com" {
		t.Errorf("second element = %+v, want the email field with its value", elements[1])
	}

	for _, e := range elements {
		if e.Type == "Image" {
			t.Errorf("decoration leaked into the agent payload: %+v", e)
		}
		if e.Label == "" && e.ID == "" {
			t.Errorf("unaddressable element leaked into the agent payload: %+v", e)
		}
	}
}

// TestDistillComputesActivationCenter checks the coordinate fallback used when
// no selector matches.
func TestDistillComputesActivationCenter(t *testing.T) {
	elements := distillJSON(t, sampleTree)

	// Sign In: x=100 w=100 -> 150, y=200 h=40 -> 220.
	if elements[0].X != 150 || elements[0].Y != 220 {
		t.Errorf("center = (%d,%d), want (150,220)", elements[0].X, elements[0].Y)
	}
}

// TestDistillEmptyTree guards the JSON contract: an empty result must encode as
// [] rather than null, since the Swift side decodes a non-optional array.
func TestDistillEmptyTree(t *testing.T) {
	elements := distill(nil)
	if elements == nil {
		t.Fatal("distill(nil) returned a nil slice; it must be empty, not null, for the JSON contract")
	}
	if len(elements) != 0 {
		t.Fatalf("got %d elements, want 0", len(elements))
	}
}

func TestTargetArgs(t *testing.T) {
	tests := []struct {
		name    string
		target  Target
		want    []string
		wantErr bool
	}{
		{
			name:   "label is preferred and carries its qualifiers",
			target: Target{Label: "Sign In", ElementType: "Button", WaitTimeout: 5 * time.Second},
			want:   []string{"--label", "Sign In", "--element-type", "Button", "--wait-timeout", "5"},
		},
		{
			name:   "id when no label",
			target: Target{ID: "signin-button"},
			want:   []string{"--id", "signin-button"},
		},
		{
			// AXe ignores a selector when coordinates are present, so the two
			// must never be emitted together.
			name:   "label wins over coordinates",
			target: Target{Label: "Sign In", X: 10, Y: 20},
			want:   []string{"--label", "Sign In"},
		},
		{
			name:   "coordinates only when nothing else is set",
			target: Target{X: 10, Y: 20},
			want:   []string{"-x", "10", "-y", "20"},
		},
		{
			name:    "an empty target is a caller bug, not a silent no-op",
			target:  Target{},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.target.args()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got args %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("args = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAXeBinaryOverride(t *testing.T) {
	if got := AXeBinary(); got != "axe" {
		t.Errorf("default binary = %q, want %q", got, "axe")
	}
	t.Setenv("SIMBERTH_AXE", "/opt/custom/axe")
	if got := AXeBinary(); got != "/opt/custom/axe" {
		t.Errorf("override = %q, want /opt/custom/axe", got)
	}
}

func distillJSON(t *testing.T, raw string) []Element {
	t.Helper()
	roots, err := parseTree([]byte(raw))
	if err != nil {
		t.Fatalf("parse sample tree: %v", err)
	}
	return distill(roots)
}
