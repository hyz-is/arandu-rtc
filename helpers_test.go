package rtc_test

import (
	"encoding/binary"
	"os"
	"testing"

	"github.com/hyz-is/arandu-rtc"
)

// clip reads a fixture: a few seconds of a test pattern at 160x120 and 10 pictures a second, a
// keyframe every ten, muxed by ffmpeg with the flags the camera reader uses.
func clip(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// feed writes data to w in pieces of size bytes, the way a pipe delivers it.
func feed(t *testing.T, w interface{ Write([]byte) (int, error) }, data []byte, size int) {
	t.Helper()
	for len(data) > 0 {
		n := min(size, len(data))
		if _, err := w.Write(data[:n]); err != nil {
			t.Fatal(err)
		}
		data = data[n:]
	}
}

type parsed struct {
	tracks  []rtc.Track
	samples []rtc.Sample
}

func demux(t *testing.T, data []byte, size int) parsed {
	t.Helper()
	var p parsed
	d := rtc.NewDemuxer(func(tr rtc.Track) { p.tracks = append(p.tracks, tr) }, func(s rtc.Sample) { p.samples = append(p.samples, s) })
	feed(t, d, data, size)
	return p
}

// nalsOK says a sample is length-prefixed NAL units that add up to the whole sample.
func nalsOK(b []byte) bool {
	for len(b) > 0 {
		if len(b) < 4 {
			return false
		}
		n := int(binary.BigEndian.Uint32(b))
		if n <= 0 || 4+n > len(b) {
			return false
		}
		b = b[4+n:]
	}
	return true
}
