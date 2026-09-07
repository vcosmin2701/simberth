package simberth

import (
	"bufio"
	"bytes"
	"strconv"
	"testing"
)

// jpegBytes is a minimal but structurally valid JPEG: SOI, a SOF0 declaring
// 100x200, then EOI.
func jpegBytes() []byte {
	return []byte{
		0xFF, 0xD8, // SOI
		0xFF, 0xC0, 0x00, 0x11, 0x08, // SOF0, length 17, precision 8
		0x00, 0xC8, // height 200
		0x00, 0x64, // width 100
		0x03, 0x01, 0x11, 0x00, 0x02, 0x11, 0x01, 0x03, 0x11, 0x01,
		0xFF, 0xD9, // EOI
	}
}

// TestParseMJPEGReadsByLength is the reason frames are read by Content-Length
// rather than by scanning for the boundary string: a JPEG payload can contain
// those bytes itself, and boundary-scanning would split a frame in half.
func TestParseMJPEGReadsByLength(t *testing.T) {
	payload := jpegBytes()
	// Embed the boundary marker inside the payload to prove it is not treated
	// as a delimiter.
	hostile := append(append([]byte{}, payload[:2]...), []byte("--mjpegstream")...)
	hostile = append(hostile, payload[2:]...)

	var stream bytes.Buffer
	stream.WriteString("HTTP/1.1 200 OK\r\nContent-Type: multipart/x-mixed-replace; boundary=--mjpegstream\r\n\r\n")
	for _, frame := range [][]byte{payload, hostile, payload} {
		stream.WriteString("--mjpegstream\r\nContent-Type: image/jpeg\r\n")
		stream.WriteString("Content-Length: " + strconv.Itoa(len(frame)) + "\r\n\r\n")
		stream.Write(frame)
		stream.WriteString("\r\n")
	}

	var got [][]byte
	if err := parseMJPEG(&stream, "UDID-1", func(f Frame) {
		if f.UDID != "UDID-1" {
			t.Errorf("frame attributed to %q", f.UDID)
		}
		got = append(got, f.JPEG)
	}); err != nil {
		t.Fatalf("parseMJPEG: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d frames, want 3", len(got))
	}
	if !bytes.Equal(got[1], hostile) {
		t.Error("a frame containing the boundary string was truncated")
	}
}

// TestParseMJPEGStopsCleanlyOnTruncation: a mirror is killed by cancelling its
// context, which cuts the stream mid-frame. That is normal shutdown, not an error.
func TestParseMJPEGStopsCleanlyOnTruncation(t *testing.T) {
	payload := jpegBytes()
	var stream bytes.Buffer
	stream.WriteString("--mjpegstream\r\nContent-Length: " + strconv.Itoa(len(payload)) + "\r\n\r\n")
	stream.Write(payload)
	stream.WriteString("--mjpegstream\r\nContent-Length: 999999\r\n\r\n")
	stream.Write([]byte{0xFF, 0xD8, 0x01}) // truncated

	count := 0
	if err := parseMJPEG(&stream, "U", func(Frame) { count++ }); err != nil {
		t.Fatalf("a truncated stream should end cleanly, got %v", err)
	}
	if count != 1 {
		t.Errorf("got %d complete frames, want 1", count)
	}
}

// TestFrameRoundTrip covers simberth's own framing, which the Swift side parses.
func TestFrameRoundTrip(t *testing.T) {
	frames := []Frame{
		{UDID: "AAA", JPEG: jpegBytes()},
		{UDID: "BBB", JPEG: append(jpegBytes(), []byte("--simberth-frame")...)},
	}

	var buf bytes.Buffer
	for _, f := range frames {
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}

	reader := bufio.NewReader(&buf)
	for i, want := range frames {
		got, err := ReadFrame(reader)
		if err != nil {
			t.Fatalf("ReadFrame %d: %v", i, err)
		}
		if got.UDID != want.UDID {
			t.Errorf("frame %d udid = %q, want %q", i, got.UDID, want.UDID)
		}
		if !bytes.Equal(got.JPEG, want.JPEG) {
			t.Errorf("frame %d payload changed in transit", i)
		}
	}
}

func TestJPEGDimensions(t *testing.T) {
	w, h, err := JPEGDimensions(jpegBytes())
	if err != nil {
		t.Fatalf("JPEGDimensions: %v", err)
	}
	if w != 100 || h != 200 {
		t.Errorf("got %dx%d, want 100x200", w, h)
	}
	if _, _, err := JPEGDimensions([]byte("not a jpeg")); err == nil {
		t.Error("non-JPEG input should be rejected")
	}
}

// TestMirrorOptionsNormalize keeps a bad flag from reaching AXe, which would
// reject the whole stream rather than clamp.
func TestMirrorOptionsNormalize(t *testing.T) {
	got := MirrorOptions{FPS: 999, Scale: 5, Quality: -1}.normalized()
	want := DefaultMirrorOptions()
	if got != want {
		t.Errorf("normalized = %+v, want the defaults %+v", got, want)
	}

	valid := MirrorOptions{FPS: 12, Scale: 0.8, Quality: 90}
	if valid.normalized() != valid {
		t.Error("valid options should be left alone")
	}
}
