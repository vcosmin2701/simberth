package simberth

// Live screen mirroring. A run is far easier to follow when you can watch the
// phones rather than read a step list, so the Runs view shows one live screen
// per simulator and the step timeline beside it.
//
// AXe streams MJPEG (multipart/x-mixed-replace) at a configurable frame rate,
// scale and quality. Mirroring runs alongside the agents and is deliberately
// cheap: a fleet of twelve at full size and full rate would cost more than the
// testing it is meant to show.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// MirrorOptions tunes one simulator's video stream.
type MirrorOptions struct {
	FPS     int     // 1-30
	Scale   float64 // 0.1-1.0 of native resolution
	Quality int     // JPEG quality, 1-100
}

// DefaultMirrorOptions are tuned for a grid of simulators rather than a single
// large view: small, frequent frames that stay readable when tiled.
func DefaultMirrorOptions() MirrorOptions {
	return MirrorOptions{FPS: 5, Scale: 0.4, Quality: 60}
}

func (o MirrorOptions) normalized() MirrorOptions {
	if o.FPS < 1 || o.FPS > 30 {
		o.FPS = 5
	}
	if o.Scale < 0.1 || o.Scale > 1.0 {
		o.Scale = 0.4
	}
	if o.Quality < 1 || o.Quality > 100 {
		o.Quality = 60
	}
	return o
}

// Frame is one captured screen.
type Frame struct {
	UDID string
	JPEG []byte
}

// MirrorScreen streams a simulator's display, calling onFrame for each frame
// until the context is cancelled or the stream fails. It blocks, so callers run
// it in a goroutine.
//
// Frames are dropped rather than queued when a consumer is slow: a stale frame
// is worthless, and buffering them would grow without bound.
func MirrorScreen(ctx context.Context, udid string, opts MirrorOptions, onFrame func(Frame)) error {
	opts = opts.normalized()

	cmd := exec.CommandContext(ctx, AXeBinary(),
		"stream-video", "--udid", udid,
		"--format", "mjpeg",
		"--fps", strconv.Itoa(opts.FPS),
		"--scale", strconv.FormatFloat(opts.Scale, 'f', -1, 64),
		"--quality", strconv.Itoa(opts.Quality),
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// AXe writes its banner to stderr; discarding it keeps the stream clean.
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		if isMissingBinary(err) {
			return fmt.Errorf("axe not found; install it with `brew install cameroncooke/axe/axe` or set SIMBERTH_AXE: %w", err)
		}
		return fmt.Errorf("start mirror: %w", err)
	}

	parseErr := parseMJPEG(stdout, udid, onFrame)

	// A cancelled context is the normal way a mirror ends, not a failure.
	_ = cmd.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return parseErr
}

// parseMJPEG reads a multipart/x-mixed-replace stream and yields each JPEG.
//
// Every part carries a Content-Length, so frames are read by length rather than
// by scanning for boundaries — which would otherwise risk matching bytes inside
// the JPEG payload itself.
func parseMJPEG(r io.Reader, udid string, onFrame func(Frame)) error {
	reader := bufio.NewReaderSize(r, 128*1024)

	for {
		length, err := readPartHeader(reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if length <= 0 {
			continue
		}

		frame := make([]byte, length)
		if _, err := io.ReadFull(reader, frame); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		// Guard against a desynchronised stream handing us non-JPEG bytes.
		if len(frame) > 2 && frame[0] == 0xFF && frame[1] == 0xD8 {
			onFrame(Frame{UDID: udid, JPEG: frame})
		}
	}
}

// readPartHeader consumes headers up to the blank line and returns the part's
// Content-Length.
func readPartHeader(r *bufio.Reader) (int, error) {
	length := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return 0, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			// Blank line ends the headers; a length of 0 means this was the
			// stream's own HTTP preamble, so the caller loops again.
			return length, nil
		}
		if name, value, ok := strings.Cut(trimmed, ":"); ok {
			if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
				length, _ = strconv.Atoi(strings.TrimSpace(value))
			}
		}
	}
}

// FrameBoundary is the marker separating frames when simberth re-emits a
// mirror stream to its own consumers. Each frame is preceded by this line and a
// byte count, which keeps the format trivial to parse from Swift.
const FrameBoundary = "--simberth-frame"

// WriteFrame emits one frame in simberth's own length-prefixed framing.
func WriteFrame(w io.Writer, f Frame) error {
	header := fmt.Sprintf("%s\r\nudid: %s\r\nlength: %d\r\n\r\n", FrameBoundary, f.UDID, len(f.JPEG))
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}
	_, err := w.Write(f.JPEG)
	return err
}

// ReadFrame reads one frame written by WriteFrame.
func ReadFrame(r *bufio.Reader) (Frame, error) {
	var frame Frame
	length := 0

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return frame, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		if trimmed == FrameBoundary {
			continue
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "udid":
			frame.UDID = strings.TrimSpace(value)
		case "length":
			length, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}

	if length <= 0 {
		return frame, fmt.Errorf("frame has no length")
	}
	frame.JPEG = make([]byte, length)
	if _, err := io.ReadFull(r, frame.JPEG); err != nil {
		return frame, err
	}
	return frame, nil
}

// MirrorFleet mirrors several simulators at once onto a single writer, which is
// how the app receives every screen in a run over one pipe. Writes are
// serialized so frames never interleave mid-payload.
func MirrorFleet(ctx context.Context, udids []string, opts MirrorOptions, w io.Writer, lock interface {
	Lock()
	Unlock()
}) error {
	if len(udids) == 0 {
		return fmt.Errorf("no simulators to mirror")
	}

	errs := make(chan error, len(udids))
	for _, udid := range udids {
		go func(udid string) {
			errs <- MirrorScreen(ctx, udid, opts, func(f Frame) {
				lock.Lock()
				defer lock.Unlock()
				_ = WriteFrame(w, f)
			})
		}(udid)
	}

	// The fleet mirror lives as long as its context; a single simulator's
	// stream ending must not tear down the others.
	var firstErr error
	for range udids {
		if err := <-errs; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// JPEGDimensions reads a JPEG's pixel size from its SOF marker, for laying out
// a grid without decoding the image.
func JPEGDimensions(data []byte) (width, height int, err error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 0, 0, fmt.Errorf("not a JPEG")
	}
	for i := 2; i+9 < len(data); {
		if data[i] != 0xFF {
			i++
			continue
		}
		marker := data[i+1]
		// SOF0-SOF3 and SOF5-SOF15 carry the frame dimensions.
		if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			height = int(data[i+5])<<8 | int(data[i+6])
			width = int(data[i+7])<<8 | int(data[i+8])
			return width, height, nil
		}
		if i+3 >= len(data) {
			break
		}
		i += 2 + (int(data[i+2])<<8 | int(data[i+3]))
	}
	return 0, 0, fmt.Errorf("no SOF marker")
}
