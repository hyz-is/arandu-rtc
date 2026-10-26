package rtc_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyz-is/arandu-rtc"
)

// fakeTranscoder stands in for ffmpeg: it reads what the hub feeds it, and answers with an H.264
// fixture, as a converter would, until it is cancelled.
type fakeTranscoder struct {
	t *testing.T

	mu      sync.Mutex
	started int
	running int
	stopped int
	codecs  []string
	sizes   [][2]int
	read    map[string]*bytes.Buffer
	fail    bool
}

func newFake(t *testing.T) *fakeTranscoder {
	return &fakeTranscoder{t: t, read: map[string]*bytes.Buffer{}}
}

func (f *fakeTranscoder) Transcode(ctx context.Context, camera, codec string, width, height int, in io.Reader, out io.Writer) error {
	f.mu.Lock()
	f.started++
	f.running++
	f.codecs = append(f.codecs, codec)
	f.sizes = append(f.sizes, [2]int{width, height})
	buf := &bytes.Buffer{}
	f.read[camera] = buf
	fail := f.fail
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running--
		f.stopped++
		f.mu.Unlock()
	}()
	if fail {
		return errors.New("the encoder is gone")
	}
	go func() {
		chunk := make([]byte, 32<<10)
		for {
			n, err := in.Read(chunk)
			f.mu.Lock()
			buf.Write(chunk[:n])
			f.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	h264 := clip(f.t, "h264.mp4")
	for len(h264) > 0 {
		n := min(1400, len(h264))
		_, _ = out.Write(h264[:n])
		h264 = h264[n:]
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeTranscoder) counts() (started, running, stopped int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.running, f.stopped
}

func (f *fakeTranscoder) got(camera string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b := f.read[camera]; b != nil {
		return append([]byte(nil), b.Bytes()...)
	}
	return nil
}

func hevc(t *testing.T, h *rtc.Hub, camera string) io.WriteCloser {
	src := h.Open(camera)
	feed(t, src, clip(t, "h265.mp4"), 1500)
	return src
}

func TestAnH264CameraNeverReachesTheTranscoder(t *testing.T) {
	f := newFake(t)
	h := rtc.NewHub(rtc.Config{Transcoder: f, MaxTranscodes: 2})
	v, _ := h.Subscribe("cam")
	feed(t, h.Open("cam"), clip(t, "h264.mp4"), 1500)
	got := pending(v)
	if len(got) != 31 || got[0].Track == nil || !strings.HasPrefix(got[0].Track.Codec, "avc1.") {
		t.Fatalf("an H.264 camera was not handed on as it is: %d events", len(got))
	}
	if started, _, _ := f.counts(); started != 0 || h.Transcoding() != 0 {
		t.Fatalf("an H.264 camera started %d conversions", started)
	}
}

func TestAnH265CameraIsConvertedOnlyWhileSomebodyWatches(t *testing.T) {
	f := newFake(t)
	h := rtc.NewHub(rtc.Config{Transcoder: f, MaxTranscodes: 2, TranscodeIdle: 150 * time.Millisecond})
	src := hevc(t, h, "cam")
	if started, _, _ := f.counts(); started != 0 {
		t.Fatalf("a camera nobody watches is being converted (%d)", started)
	}
	if h.State("cam") != "starting" {
		t.Fatalf("a camera that needs converting says %q before anybody asks", h.State("cam"))
	}

	v, err := h.Subscribe("cam")
	if err != nil {
		t.Fatal(err)
	}
	until(t, "the conversion starts for the first viewer", func() bool { s, _, _ := f.counts(); return s == 1 })
	// The viewer is given the converted picture, an H.264 track, and not the camera's own.
	var track *rtc.Track
	until(t, "the viewer is given the converted track", func() bool {
		for _, e := range pending(v) {
			if e.Track != nil {
				track = e.Track
			}
		}
		return track != nil
	})
	if !strings.HasPrefix(track.Codec, "avc1.") {
		t.Fatalf("the viewer was given %q", track.Codec)
	}
	f.mu.Lock()
	codec, size := f.codecs[0], f.sizes[0]
	f.mu.Unlock()
	if !strings.HasPrefix(codec, "hvc1.") || size != [2]int{160, 120} {
		t.Errorf("the transcoder was told %q %v: it needs the camera's own codec and size", codec, size)
	}
	// What it is fed is the camera's container as it came: the header first, then the pictures.
	until(t, "the transcoder is fed", func() bool { return len(f.got("cam")) > 1000 })
	fed := f.got("cam")
	if !bytes.Equal(fed[4:8], []byte("ftyp")) {
		t.Fatalf("the transcoder was fed %q first, not the container's header", fed[4:8])
	}
	if p := demux(t, fed, 4096); len(p.tracks) != 1 || len(p.samples) == 0 || !p.samples[0].Key {
		t.Fatalf("what the transcoder was fed is not a stream it can start: tracks=%d samples=%d", len(p.tracks), len(p.samples))
	}

	// A second viewer shares the conversion.
	w, err := h.Subscribe("cam")
	if err != nil {
		t.Fatal(err)
	}
	if started, _, _ := f.counts(); started != 1 || h.Transcoding() != 1 {
		t.Fatalf("a second viewer started another conversion: %d", started)
	}

	// Both leave; the conversion outlives them by the idle time, and a page that comes back keeps it.
	v.Close()
	w.Close()
	time.Sleep(60 * time.Millisecond)
	back, _ := h.Subscribe("cam")
	time.Sleep(250 * time.Millisecond)
	if started, running, _ := f.counts(); started != 1 || running != 1 {
		t.Fatalf("a viewer who came back inside the idle time restarted the conversion: started=%d running=%d", started, running)
	}
	back.Close()
	until(t, "the conversion stops after the idle time", func() bool { _, r, _ := f.counts(); return r == 0 })
	if h.Transcoding() != 0 {
		t.Fatalf("the slot of a stopped conversion is not free: %d", h.Transcoding())
	}
	_ = src.Close()
}

func TestTheNodeConvertsAsManyCamerasAsItMayAndSaysBusyToTheNext(t *testing.T) {
	f := newFake(t)
	h := rtc.NewHub(rtc.Config{Transcoder: f, MaxTranscodes: 1, TranscodeIdle: 100 * time.Millisecond})
	hevc(t, h, "one")
	hevc(t, h, "two")
	a, err := h.Subscribe("one")
	if err != nil {
		t.Fatal(err)
	}
	until(t, "the first camera is converted", func() bool { _, r, _ := f.counts(); return r == 1 })
	if _, err := h.Subscribe("two"); !errors.Is(err, rtc.ErrBusy) {
		t.Fatalf("a viewer of a second converted camera: %v, want ErrBusy", err)
	}
	// Another viewer of the camera being converted costs nothing more.
	b, err := h.Subscribe("one")
	if err != nil {
		t.Fatalf("a second viewer of the converted camera: %v", err)
	}
	a.Close()
	b.Close()
	until(t, "the slot is freed", func() bool { return h.Transcoding() == 0 })
	c, err := h.Subscribe("two")
	if err != nil {
		t.Fatalf("after the slot was freed: %v", err)
	}
	c.Close()
}

func TestAViewerWhoWaitsForAConversionIsToldWhenTheNodeCannotDoIt(t *testing.T) {
	f := newFake(t)
	h := rtc.NewHub(rtc.Config{Transcoder: f, MaxTranscodes: 1, TranscodeIdle: time.Minute})
	hevc(t, h, "one")
	a, _ := h.Subscribe("one")
	until(t, "the first is converted", func() bool { _, r, _ := f.counts(); return r == 1 })
	defer a.Close()
	// "two" is not known to need converting when its viewer arrives, so it gets in; the camera
	// then turns out to be H.265 and the node is full.
	v, err := h.Subscribe("two")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	hevc(t, h, "two")
	until(t, "the viewer is told the node is busy", func() bool {
		for _, e := range pending(v) {
			if e.Busy {
				return true
			}
		}
		return false
	})
}

func TestAConversionThatStopsByItselfIsStartedAgainForTheViewersWhoRemain(t *testing.T) {
	f := newFake(t)
	f.fail = true
	h := rtc.NewHub(rtc.Config{Transcoder: f, TranscodeRetry: 30 * time.Millisecond})
	hevc(t, h, "cam")
	v, _ := h.Subscribe("cam")
	defer v.Close()
	until(t, "it is tried again", func() bool { s, _, _ := f.counts(); return s >= 3 })
	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
	until(t, "the viewer is given the converted picture", func() bool {
		for _, e := range pending(v) {
			if e.Track != nil {
				return true
			}
		}
		return false
	})
}

// A camera whose address was renewed opens a new source: the conversion follows it, and the viewer
// keeps its place.
func TestARenewedSourceIsConvertedAgainWithoutLosingTheViewer(t *testing.T) {
	f := newFake(t)
	h := rtc.NewHub(rtc.Config{Transcoder: f, EndGrace: 300 * time.Millisecond})
	src := hevc(t, h, "cam")
	v, _ := h.Subscribe("cam")
	defer v.Close()
	until(t, "converted", func() bool { s, _, _ := f.counts(); return s == 1 })
	_ = src.Close()
	hevc(t, h, "cam")
	until(t, "converted again", func() bool { s, _, _ := f.counts(); return s == 2 })
	if h.Viewers("cam") != 1 {
		t.Fatal("the viewer lost its place")
	}
}

func TestTheSocketSaysStartingThenGivesTheConvertedPictureAndSaysBusyWhenFull(t *testing.T) {
	f := newFake(t)
	r := newRig(t, rtc.ServerConfig{}, nil, rtc.Config{Transcoder: f, MaxTranscodes: 1, ViewersPerCamera: 2, TranscodeIdle: time.Minute})
	hevc(t, r.hub, "one")
	hevc(t, r.hub, "two")

	one, _, err := r.dial(t, "one", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer one.CloseNow()
	// "starting" while the camera is being converted, unless the conversion won the race with the
	// socket; either way the track the viewer gets is the converted one, never the camera's own.
	_, b := read(t, one)
	if string(b) == `{"type":"signal","state":"starting"}` {
		_, b = read(t, one)
	}
	if !strings.Contains(string(b), `"codec":"avc1.`) {
		t.Fatalf("the viewer was sent %s, want the converted track", b)
	}

	two, _, err := r.dial(t, "two", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer two.CloseNow()
	if _, b := read(t, two); string(b) != `{"type":"signal","state":"busy"}` {
		t.Fatalf("a camera the node cannot convert: %s, want busy", b)
	}
}

// timedConverter is a converter that takes its time: it answers the H.264 fixture only when told,
// as a transcoder that finishes a picture long after the camera sent it.
type timedConverter struct {
	release chan struct{}
	started chan struct{}
}

func (c *timedConverter) Transcode(ctx context.Context, camera, codec string, width, height int, in io.Reader, out io.Writer) error {
	close(c.started)
	go func() { _, _ = io.Copy(io.Discard, in) }()
	select {
	case <-c.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	h264, _ := os.ReadFile("testdata/h264.mp4")
	for len(h264) > 0 {
		n := min(1400, len(h264))
		_, _ = out.Write(h264[:n])
		h264 = h264[n:]
	}
	<-ctx.Done()
	return ctx.Err()
}

func boxesOf(data []byte) [][]byte {
	var out [][]byte
	for len(data) >= 8 {
		n := int(data[0])<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3])
		out = append(out, data[:n])
		data = data[n:]
	}
	return out
}

// What the viewer is shown is dated when the camera sent it, not when the converter finished it:
// the boxes the engine found on a frame carry the time of that frame, and a picture dated by the
// converter's clock would be shown with the boxes of a moment later.
func TestAConvertedPictureIsDatedWhenTheCameraSentIt(t *testing.T) {
	var mu sync.Mutex
	clock := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }

	conv := &timedConverter{release: make(chan struct{}), started: make(chan struct{})}
	h := rtc.NewHub(rtc.Config{Transcoder: conv, Now: now})
	v, err := h.Subscribe("cam")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	src := h.Open("cam")

	// The camera's pictures arrive in fragments a tenth of a second apart; what the hub should
	// remember of each is when its fragment arrived, less the picture time between it and the
	// fragment's last picture.
	start := now()
	native := map[time.Duration]time.Time{}
	var first time.Duration
	var haveFirst bool
	var inFragment []rtc.Sample
	shadow := rtc.NewDemuxer(nil, func(s rtc.Sample) { inFragment = append(inFragment, s) })
	for _, box := range boxesOf(clip(t, "h265.mp4")) {
		if string(box[4:8]) == "moof" {
			advance(100 * time.Millisecond)
		}
		inFragment = nil
		_, _ = shadow.Write(box)
		_, _ = src.Write(box)
		if len(inFragment) > 0 {
			last := inFragment[len(inFragment)-1].PTS
			if !haveFirst {
				first, haveFirst = inFragment[0].PTS, true
			}
			for _, sm := range inFragment {
				native[sm.PTS] = now().Add(-(last - sm.PTS))
			}
		}
	}
	<-conv.started
	// The converter finishes the first picture seven tenths of a second after the camera sent the last.
	advance(700 * time.Millisecond)
	close(conv.release)

	var converted []rtc.Sample
	until(t, "the converted pictures arrive", func() bool {
		for _, e := range pending(v) {
			if e.Sample != nil {
				converted = append(converted, *e.Sample)
			}
		}
		return len(converted) >= 20
	})
	for i := 0; i < 20; i++ {
		// The converter's pictures come in the order they were shown (the fixture has no reordering);
		// the camera's picture at the same time of the programme is the one to compare with.
		// The converter counts from its first picture, which is the camera's first keyframe.
		want, ok := native[first+converted[i].PTS]
		if !ok {
			t.Fatalf("converted picture %d at %s has no picture of the camera at that time", i, converted[i].PTS)
		}
		if d := converted[i].Wall.Sub(want); d < -time.Millisecond || d > time.Millisecond {
			t.Errorf("converted picture %d (at %s) is dated %s from the camera's own, %s after the start: want the same instant",
				i, converted[i].PTS, d, converted[i].Wall.Sub(start))
		}
	}
}

func TestAHubThatSetsConvertConvertsAnH264PictureThatIsTooLarge(t *testing.T) {
	f := newFake(t)
	var asked []string
	var mu sync.Mutex
	h := rtc.NewHub(rtc.Config{
		Transcoder: f, MaxTranscodes: 1, TranscodeIdle: 150 * time.Millisecond,
		// The fixture is 160x120 H.264; "too large" here is anything over 100 pixels wide.
		Convert: func(camera string, tr rtc.Track) bool {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, camera)
			return tr.Width > 100
		},
	})
	v, err := h.Subscribe("cam")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	feed(t, h.Open("cam"), clip(t, "h264.mp4"), 1500)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if started, _, _ := f.counts(); started == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if started, _, _ := f.counts(); started != 1 {
		t.Fatalf("an H.264 picture the hub is told to convert started %d conversions", started)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "cam" {
		t.Errorf("Convert was asked %v, want once for cam", asked)
	}
}

func TestAHubThatSetsConvertLeavesAnH265PictureItSaysFits(t *testing.T) {
	f := newFake(t)
	h := rtc.NewHub(rtc.Config{Transcoder: f, Convert: func(string, rtc.Track) bool { return false }})
	v, _ := h.Subscribe("cam")
	defer v.Close()
	hevc(t, h, "cam")
	time.Sleep(100 * time.Millisecond)
	if started, _, _ := f.counts(); started != 0 {
		t.Errorf("a picture Convert said fits started %d conversions", started)
	}
	if got := h.State("cam"); got != "live" {
		t.Errorf("state %q, want live: the picture is handed on as it is", got)
	}
}
