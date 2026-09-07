package main

// `simberth mirror` streams live simulator screens. The macOS app runs this for
// the simulators in a run so the Runs view can show the phones themselves
// rather than only a list of steps.

import (
	"context"
	"fmt"
	"os"
	"sync"

	cli "github.com/urfave/cli/v3"

	"github.com/vcosmin2701/simberth"
)

func mirrorCommand(jsonFlag func() cli.Flag) *cli.Command {
	return &cli.Command{
		Name:      "mirror",
		Usage:     "stream live simulator screens as JPEG frames",
		ArgsUsage: "<udid>...",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "fps", Usage: "frames per second (1-30)", Value: 5},
			&cli.FloatFlag{Name: "scale", Usage: "fraction of native resolution (0.1-1.0)", Value: 0.4},
			&cli.IntFlag{Name: "quality", Usage: "JPEG quality (1-100)", Value: 60},
			&cli.StringFlag{Name: "out", Usage: "write the newest frame per simulator to this directory instead of stdout"},
		},
		Action: cmdMirror,
	}
}

func cmdMirror(ctx context.Context, cmd *cli.Command) error {
	udids := cmd.Args().Slice()
	if len(udids) == 0 {
		// With no arguments, mirror whatever is booted — the common case when
		// checking on a fleet by hand.
		devices, err := simberth.ListDevices(ctx)
		if err != nil {
			return err
		}
		for _, d := range devices {
			if d.State == "Booted" {
				udids = append(udids, d.UDID)
			}
		}
		if len(udids) == 0 {
			return fmt.Errorf("no booted simulators to mirror")
		}
	}

	opts := simberth.MirrorOptions{
		FPS:     cmd.Int("fps"),
		Scale:   cmd.Float("scale"),
		Quality: cmd.Int("quality"),
	}

	// --out writes each simulator's newest frame to a file, which is the
	// simplest way to eyeball a fleet without a consumer for the stream.
	if dir := cmd.String("out"); dir != "" {
		return mirrorToDirectory(ctx, udids, opts, dir)
	}

	var mu sync.Mutex
	return simberth.MirrorFleet(ctx, udids, opts, os.Stdout, &mu)
}

func mirrorToDirectory(ctx context.Context, udids []string, opts simberth.MirrorOptions, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	var wg sync.WaitGroup
	for _, udid := range udids {
		wg.Add(1)
		go func(udid string) {
			defer wg.Done()
			path := fmt.Sprintf("%s/%s.jpg", dir, udid)
			_ = simberth.MirrorScreen(ctx, udid, opts, func(f simberth.Frame) {
				// Written via a temporary file and renamed, so a reader never
				// catches a half-written frame.
				tmp := path + ".tmp"
				if err := os.WriteFile(tmp, f.JPEG, 0o644); err == nil {
					_ = os.Rename(tmp, path)
				}
			})
		}(udid)
	}
	wg.Wait()
	return nil
}
