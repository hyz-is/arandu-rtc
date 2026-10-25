package rtc_test

import (
	"testing"
	"time"

	"github.com/hyz-is/arandu-rtc"
)

// next waits for the viewer's next event.
func next(t *testing.T, s *rtc.Subscription) rtc.Event {
	t.Helper()
	select {
	case e, ok := <-s.Events():
		if !ok {
			t.Fatal("the viewer was closed")
		}
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event within 2s")
	}
	return rtc.Event{}
}

// pending is what the viewer has waiting, without waiting for more.
func pending(s *rtc.Subscription) []rtc.Event {
	var out []rtc.Event
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestEveryViewerGetsTheTrackAndTheSamplesOfOneRead(t *testing.T) {
	h := rtc.NewHub(rtc.Config{})
	a, err := h.Subscribe("cam")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := h.Subscribe("cam")
	if h.Live("cam") || h.Viewers("cam") != 2 {
		t.Fatalf("before any video: live=%v viewers=%d", h.Live("cam"), h.Viewers("cam"))
	}

	src := h.Open("cam")
	feed(t, src, clip(t, "h264.mp4"), 1500)
	if !h.Live("cam") {
		t.Fatal("a camera with a source writing is not live")
	}
	for _, s := range []*rtc.Subscription{a, b} {
		got := pending(s)
		if len(got) != 31 || got[0].Track == nil {
			t.Fatalf("a viewer has %d events starting with %+v, want the track and 30 samples", len(got), got[0])
		}
		for i, e := range got[1:] {
			if e.Sample == nil || e.Sample.Key != (i%10 == 0) {
				t.Fatalf("event %d is %+v", i+1, e)
			}
		}
	}
	// The same sample object reaches both: a viewer is a pointer, not a copy of the picture.
	if x, y := pending(a), pending(b); len(x) != 0 || len(y) != 0 {
		t.Fatal("events were left over")
	}
}

func TestAViewerWhoArrivesMidGroupStartsAtOnceOnAKeyframe(t *testing.T) {
	h := rtc.NewHub(rtc.Config{})
	src := h.Open("cam")
	data := clip(t, "h264.mp4")
	// Write until the stream is in the middle of its second group of pictures.
	var seen int
	probe := rtc.NewDemuxer(nil, func(rtc.Sample) { seen++ })
	cut := 0
	for cut < len(data) && seen < 14 {
		n := min(100, len(data)-cut)
		_, _ = probe.Write(data[cut : cut+n])
		cut += n
	}
	feed(t, src, data[:cut], 1000)

	late, err := h.Subscribe("cam")
	if err != nil {
		t.Fatal(err)
	}
	got := pending(late)
	if len(got) < 3 || got[0].Track == nil || got[1].Sample == nil || !got[1].Sample.Key {
		t.Fatalf("a viewer arriving mid-group has %d events, the first two %+v %+v", len(got), got[0], got[1])
	}
	if len(got) > 11 {
		t.Errorf("%d events: more than a group of ten pictures plus the track", len(got))
	}
	// And from there it follows the live picture.
	feed(t, src, data[cut:], 1000)
	if len(pending(late)) == 0 {
		t.Error("the viewer got nothing after it arrived")
	}
}

func TestAViewerWhoFallsBehindIsSkippedAheadToAKeyframe(t *testing.T) {
	h := rtc.NewHub(rtc.Config{Queue: 5})
	slow, _ := h.Subscribe("cam")
	fast, _ := h.Subscribe("cam")
	src := h.Open("cam")

	// The fast viewer takes what it has after every piece, as a socket does; the slow one takes
	// nothing.
	var fastSamples int
	data := clip(t, "h264.mp4")
	for len(data) > 0 {
		n := min(1500, len(data))
		feed(t, src, data[:n], n)
		data = data[n:]
		for _, e := range pending(fast) {
			if e.Sample != nil {
				fastSamples++
			}
		}
	}

	if slow.Dropped() == 0 {
		t.Fatal("a viewer that took nothing was never skipped ahead")
	}
	got := pending(slow)
	var samples []*rtc.Sample
	for _, e := range got {
		if e.Sample != nil {
			samples = append(samples, e.Sample)
		}
	}
	// Whatever is left starts where a decoder can: the rest was dropped, and the viewer waits for
	// the next keyframe instead of being handed pictures that refer to ones it never got.
	if len(samples) > 0 && !samples[0].Key {
		t.Fatalf("after skipping, the viewer's samples start with %+v: a decoder cannot start there", samples[0])
	}
	for i := 1; i < len(samples); i++ {
		if !samples[i].Key && samples[i].PTS != samples[i-1].PTS+100*time.Millisecond {
			t.Fatalf("a hole inside the viewer's group at %d", i)
		}
	}
	if fastSamples != 30 {
		t.Errorf("the viewer that kept up got %d samples, want 30: a slow one must not cost the others", fastSamples)
	}
}

func TestTheHubLimitsViewersPerCameraAndInAll(t *testing.T) {
	h := rtc.NewHub(rtc.Config{ViewersPerCamera: 2, Viewers: 3})
	a, _ := h.Subscribe("one")
	if _, err := h.Subscribe("one"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe("one"); err != rtc.ErrFull {
		t.Fatalf("a third viewer of one camera: %v, want ErrFull", err)
	}
	if _, err := h.Subscribe("two"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe("three"); err != rtc.ErrFull {
		t.Fatalf("a fourth viewer on the node: %v, want ErrFull", err)
	}
	a.Close()
	a.Close() // twice is safe
	if _, err := h.Subscribe("three"); err != nil {
		t.Fatalf("after one left: %v", err)
	}
}

// until waits for a condition that a timer makes true.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestACameraThatLosesItsStreamTellsItsViewersAndStartsOver(t *testing.T) {
	h := rtc.NewHub(rtc.Config{EndGrace: 20 * time.Millisecond})
	v, _ := h.Subscribe("cam")
	data := clip(t, "h264.mp4")

	src := h.Open("cam")
	feed(t, src, data[:len(data)/2], 1000)
	pending(v)
	_ = src.Close()
	until(t, "a camera whose source closed stops being live", func() bool { return !h.Live("cam") })
	if got := pending(v); len(got) != 1 || !got[0].Ended {
		t.Fatalf("after the source closed the viewer has %+v, want one Ended", got)
	}

	// The reader connects again: a new header, then pictures from a keyframe.
	src = h.Open("cam")
	feed(t, src, data, 1000)
	got := pending(v)
	if len(got) == 0 || got[0].Track == nil || got[1].Sample == nil || !got[1].Sample.Key {
		t.Fatalf("after the restart the viewer has %d events starting %+v", len(got), got[0])
	}
}

func TestANewSourceReplacesTheOldOne(t *testing.T) {
	h := rtc.NewHub(rtc.Config{})
	v, _ := h.Subscribe("cam")
	data := clip(t, "h264.mp4")
	old := h.Open("cam")
	fresh := h.Open("cam")

	feed(t, old, data, 1000) // ignored
	if h.Live("cam") || len(pending(v)) != 0 {
		t.Fatal("a replaced source was heard")
	}
	_ = old.Close() // does not take the new one down
	feed(t, fresh, data, 1000)
	if !h.Live("cam") {
		t.Fatal("the new source was not heard")
	}
	_ = old.Close()
	if !h.Live("cam") {
		t.Fatal("closing the old source again took the new one down")
	}
}

func TestASourceThatWritesGarbageIsSwallowedAndNeverFails(t *testing.T) {
	h := rtc.NewHub(rtc.Config{})
	src := h.Open("cam")
	for i := 0; i < 3; i++ {
		n, err := src.Write([]byte("not a movie, just bytes going down a pipe"))
		if err != nil || n == 0 {
			t.Fatalf("write %d: n=%d err=%v: the reader has to be able to keep draining", i, n, err)
		}
	}
	if h.Live("cam") {
		t.Error("garbage made a camera live")
	}
}

func TestACameraNobodyWatchesLeavesNothingBehind(t *testing.T) {
	h := rtc.NewHub(rtc.Config{EndGrace: 10 * time.Millisecond})
	src := h.Open("cam")
	feed(t, src, clip(t, "h264.mp4"), 4000)
	_ = src.Close()
	until(t, "the camera is forgotten", func() bool { return !h.Live("cam") })
	if h.Viewers("cam") != 0 {
		t.Fatal("state left behind")
	}
	// A viewer of a camera that was never opened does not grow the hub either.
	v, _ := h.Subscribe("ghost")
	v.Close()
	if _, err := h.Subscribe("ghost"); err != nil {
		t.Fatal(err)
	}
}

func TestClosingTheHubEndsEveryViewerAndRefusesNewOnes(t *testing.T) {
	h := rtc.NewHub(rtc.Config{})
	v, _ := h.Subscribe("cam")
	h.Close()
	select {
	case _, ok := <-v.Events():
		if ok {
			t.Fatal("an event after close")
		}
	case <-time.After(time.Second):
		t.Fatal("the viewer's channel stayed open")
	}
	if _, err := h.Subscribe("cam"); err != rtc.ErrFull {
		t.Fatalf("subscribing to a closed hub: %v", err)
	}
	v.Close() // after the hub did
}

// A camera whose address was renewed, or whose reader reconnected, opens a new source within
// seconds. Its viewers' sockets carry on: they are told the picture is starting, never that the
// signal is gone, and they are given the new picture when it begins.
func TestAReconnectInsideTheGraceIsNeverAnnouncedAsNoSignal(t *testing.T) {
	h := rtc.NewHub(rtc.Config{EndGrace: 300 * time.Millisecond})
	v, _ := h.Subscribe("cam")
	data := clip(t, "h264.mp4")
	src := h.Open("cam")
	feed(t, src, data[:len(data)/2], 1000)
	pending(v)
	_ = src.Close()

	// A viewer who arrives while the reader is reconnecting waits for a keyframe, and is not
	// handed pictures the camera sent before it stopped.
	late, err := h.Subscribe("cam")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range pending(late) {
		if e.Sample != nil {
			t.Fatalf("a viewer arriving during the reconnection got an old picture: %+v", e.Sample)
		}
	}

	src = h.Open("cam")
	feed(t, src, data, 1000)
	for _, s := range []*rtc.Subscription{v, late} {
		got := pending(s)
		var sawTrack, sawEnded, sawKey bool
		for _, e := range got {
			sawEnded = sawEnded || e.Ended
			sawTrack = sawTrack || e.Track != nil
			sawKey = sawKey || (e.Sample != nil && e.Sample.Key)
		}
		if sawEnded || !sawTrack || !sawKey {
			t.Fatalf("a reconnection told a viewer ended=%v track=%v keyframe=%v", sawEnded, sawTrack, sawKey)
		}
	}
	time.Sleep(400 * time.Millisecond)
	if !h.Live("cam") {
		t.Fatal("the grace of the old source took the new one down")
	}
}
