package rtc

import (
	"os"
	"testing"
	"time"
)

// A fragment reaches the node when its last sample does; the earlier ones are older by the picture
// time between them, so a burst of five pictures is five instants and not one.
func TestDemuxerDatesEachSampleOfAFragmentApart(t *testing.T) {
	data, err := os.ReadFile("testdata/h264-slow.mp4")
	if err != nil {
		t.Fatal(err)
	}
	var (
		samples []Sample
		clock   = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
		arrived []time.Time
	)
	d := NewDemuxer(nil, func(s Sample) { samples = append(samples, s) })
	d.now = func() time.Time { return clock }

	// Feed box by box so each fragment gets its own arrival time, 500ms apart.
	for len(data) > 0 {
		size := int(uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3]))
		kind := string(data[4:8])
		before := len(samples)
		if kind == "mdat" {
			clock = clock.Add(500 * time.Millisecond)
			arrived = append(arrived, clock)
		}
		if _, err := d.Write(data[:size]); err != nil {
			t.Fatal(err)
		}
		if kind == "mdat" && len(samples) == before {
			t.Fatal("a data box produced no samples")
		}
		data = data[size:]
	}
	if len(samples) != 20 {
		t.Fatalf("%d samples, want 20", len(samples))
	}
	// 20 pictures in fragments of 5: the last of each fragment is dated at its arrival, the
	// first of it 400ms before.
	for i, s := range samples {
		fragment := i / 5
		want := arrived[fragment].Add(-time.Duration(4-i%5) * 100 * time.Millisecond)
		if !s.Wall.Equal(want) {
			t.Errorf("sample %d is dated %s, want %s", i, s.Wall.Format("05.000"), want.Format("05.000"))
		}
	}
}
