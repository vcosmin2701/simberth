package main

// The `ui` command group drives a simulator's interface through AXe. It exists
// so the simulator-control layer can be exercised — and debugged — on its own,
// before any agent is in the loop, and so a failing agent step can be replayed
// by hand from the command line.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	cli "github.com/urfave/cli/v3"

	"github.com/vcosmin2701/simberth"
)

func uiCommand(jsonFlag func() cli.Flag) *cli.Command {
	return &cli.Command{
		Name:  "ui",
		Usage: "inspect and drive a simulator's UI",
		Commands: []*cli.Command{
			{
				Name:      "describe",
				Usage:     "list the actionable elements on screen",
				ArgsUsage: "<udid>",
				Flags: []cli.Flag{
					jsonFlag(),
					&cli.BoolFlag{Name: "raw", Usage: "include the full accessibility tree (very large)"},
				},
				Action: cmdUIDescribe,
			},
			{
				Name:      "tap",
				Usage:     "tap an element by label/id, or a point with -x/-y",
				ArgsUsage: "<udid>",
				Flags: append(targetFlags(),
					jsonFlag(),
				),
				Action: cmdUITap,
			},
			{
				Name:      "type",
				Usage:     "type text into the focused field",
				ArgsUsage: "<udid> <text>",
				Flags:     []cli.Flag{jsonFlag()},
				Action:    cmdUIType,
			},
			{
				Name:      "swipe",
				Usage:     "swipe between two points",
				ArgsUsage: "<udid>",
				Flags: []cli.Flag{
					jsonFlag(),
					&cli.IntFlag{Name: "start-x", Required: true},
					&cli.IntFlag{Name: "start-y", Required: true},
					&cli.IntFlag{Name: "end-x", Required: true},
					&cli.IntFlag{Name: "end-y", Required: true},
					&cli.FloatFlag{Name: "duration", Usage: "swipe duration in seconds"},
				},
				Action: cmdUISwipe,
			},
			{
				Name:      "button",
				Usage:     "press a hardware button (apple-pay, home, lock, side-button, siri)",
				ArgsUsage: "<udid> <button>",
				Flags:     []cli.Flag{jsonFlag()},
				Action:    cmdUIButton,
			},
			{
				Name:      "screenshot",
				Usage:     "capture the display to a PNG",
				ArgsUsage: "<udid> <path>",
				Flags:     []cli.Flag{jsonFlag()},
				Action:    cmdUIScreenshot,
			},
		},
	}
}

// targetFlags are the element selectors shared by every acting command.
func targetFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "label", Usage: "match AXLabel (preferred: survives layout changes)"},
		&cli.StringFlag{Name: "id", Usage: "match AXUniqueId (accessibilityIdentifier)"},
		&cli.StringFlag{Name: "value", Usage: "match AXValue"},
		&cli.StringFlag{Name: "element-type", Usage: "narrow an ambiguous selector, e.g. Button"},
		&cli.IntFlag{Name: "x", Usage: "tap point X (used only without a selector)"},
		&cli.IntFlag{Name: "y", Usage: "tap point Y (used only without a selector)"},
		&cli.FloatFlag{Name: "wait-timeout", Usage: "seconds to poll for the element before failing"},
	}
}

func targetFromFlags(cmd *cli.Command) simberth.Target {
	return simberth.Target{
		Label:       cmd.String("label"),
		ID:          cmd.String("id"),
		Value:       cmd.String("value"),
		ElementType: cmd.String("element-type"),
		X:           cmd.Int("x"),
		Y:           cmd.Int("y"),
		WaitTimeout: time.Duration(cmd.Float("wait-timeout") * float64(time.Second)),
	}
}

func cmdUIDescribe(ctx context.Context, cmd *cli.Command) error {
	udid, err := oneUDID(cmd.Args().Slice())
	if err != nil {
		return err
	}
	screen, err := simberth.DescribeUI(ctx, udid, cmd.Bool("raw"))
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		return writeJSON(screen)
	}
	if len(screen.Elements) == 0 {
		fmt.Println("no actionable elements found")
		return nil
	}
	for _, e := range screen.Elements {
		fmt.Printf("%-16s %-40s %s\n", e.Type, describeSelector(e), e.Value)
	}
	return nil
}

// describeSelector shows how the element would be addressed, so what is printed
// is what can be pasted back into `ui tap`.
func describeSelector(e simberth.Element) string {
	switch {
	case e.Label != "":
		return "--label " + strconv.Quote(e.Label)
	case e.ID != "":
		return "--id " + strconv.Quote(e.ID)
	default:
		return fmt.Sprintf("-x %d -y %d", e.X, e.Y)
	}
}

func cmdUITap(ctx context.Context, cmd *cli.Command) error {
	udid, err := oneUDID(cmd.Args().Slice())
	if err != nil {
		return err
	}
	if err := simberth.Tap(ctx, udid, targetFromFlags(cmd)); err != nil {
		return err
	}
	return uiOK(cmd, "tap", udid)
}

func cmdUIType(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) != 2 {
		return fmt.Errorf("type takes a simulator UDID and the text to type")
	}
	if err := simberth.TypeText(ctx, args[0], args[1]); err != nil {
		return err
	}
	return uiOK(cmd, "type", args[0])
}

func cmdUISwipe(ctx context.Context, cmd *cli.Command) error {
	udid, err := oneUDID(cmd.Args().Slice())
	if err != nil {
		return err
	}
	duration := time.Duration(cmd.Float("duration") * float64(time.Second))
	err = simberth.Swipe(ctx, udid,
		cmd.Int("start-x"), cmd.Int("start-y"),
		cmd.Int("end-x"), cmd.Int("end-y"), duration)
	if err != nil {
		return err
	}
	return uiOK(cmd, "swipe", udid)
}

func cmdUIButton(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) != 2 {
		return fmt.Errorf("button takes a simulator UDID and a button name")
	}
	if err := simberth.Button(ctx, args[0], args[1]); err != nil {
		return err
	}
	return uiOK(cmd, "button", args[0])
}

func cmdUIScreenshot(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) != 2 {
		return fmt.Errorf("screenshot takes a simulator UDID and an output path")
	}
	if err := simberth.Screenshot(ctx, args[0], args[1]); err != nil {
		return err
	}
	if cmd.Bool("json") {
		return writeJSON(map[string]string{"action": "screenshot", "udid": args[0], "path": args[1]})
	}
	fmt.Printf("saved %s\n", args[1])
	return nil
}

func uiOK(cmd *cli.Command, action, udid string) error {
	if cmd.Bool("json") {
		return writeJSON(map[string]string{"action": action, "udid": udid})
	}
	fmt.Printf("%s: %s ok\n", udid, action)
	return nil
}
