package rtc_test

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

// A camera's stream that has been read for days still has a picture time that grows: the time of a
// sample was once computed as ticks times a second before the division, which overflowed an int64
// of nanoseconds after 28.5 hours at 90 kHz, and the socket wrote the wrapped negative number.
func TestDemuxerKeepsPictureTimeGrowingAfterDaysOfStream(t *testing.T) {
	data := clip(t, "h264.mp4")
	plain := demux(t, data, 4096)

	// Every fragment's decode time starts 30 days in, as a stream does that nobody restarted.
	const days = 30
	patched := bytes.Clone(data)
	var scale uint32
	if i := bytes.Index(patched, []byte("mdhd")); i > 0 {
		scale = binary.BigEndian.Uint32(patched[i+16:]) // version 0
	}
	if scale == 0 {
		t.Fatal("the fixture has no timescale")
	}
	shift := uint64(days) * 24 * 3600 * uint64(scale)
	found := 0
	for from := 0; ; {
		i := bytes.Index(patched[from:], []byte("tfdt"))
		if i < 0 {
			break
		}
		at := from + i
		if patched[at+4] != 1 {
			t.Fatal("the fixture's tfdt is not 64-bit")
		}
		binary.BigEndian.PutUint64(patched[at+8:], binary.BigEndian.Uint64(patched[at+8:])+shift)
		found++
		from = at + 4
	}
	if found == 0 {
		t.Fatal("the fixture has no fragment")
	}
	late := demux(t, patched, 4096)
	if len(late.samples) != len(plain.samples) {
		t.Fatalf("%d samples, want %d", len(late.samples), len(plain.samples))
	}
	offset := time.Duration(days) * 24 * time.Hour
	for i, s := range late.samples {
		if s.PTS < offset {
			t.Fatalf("sample %d is at %s, before the %s the stream had already run", i, s.PTS, offset)
		}
		if got, want := s.PTS-offset, plain.samples[i].PTS; got != want {
			t.Errorf("sample %d is %s after the offset, want %s", i, got, want)
		}
	}
	for i := 1; i < len(late.samples); i++ {
		if late.samples[i].PTS < late.samples[i-1].PTS {
			t.Fatalf("picture time goes backwards at sample %d", i)
		}
	}
}
