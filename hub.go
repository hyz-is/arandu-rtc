package rtc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Defaults of Config.
const (
	DefaultViewersPerCamera = 4
	DefaultViewers          = 64
	DefaultQueue            = 120
	DefaultGOPBytes         = 8 << 20
	DefaultTranscodeIdle    = 12 * time.Second
	DefaultEndGrace         = 15 * time.Second
	DefaultTranscodeRetry   = 2 * time.Second
	// rawFeedDepth is how many fragments a transcoder may be behind before its feed is cut and
	// started again from a keyframe.
	rawFeedDepth = 512
)

var (
	// ErrFull is a viewer turned away because the camera, or the node, already has all it allows.
	ErrFull = errors.New("live: the limit of viewers is reached")
	// ErrBusy is a viewer turned away because the camera's video has to be converted for the
	// browser and the node is already converting as many as it may.
	ErrBusy = errors.New("live: the node is converting as many cameras as it may")
)

// Transcoder converts a camera's video to H.264, which every browser decodes, for the cameras whose
// own codec it may not (H.265 chiefly). It is the one thing here that costs a decoder and an
// encoder, so the Hub runs it only while somebody watches.
//
// Transcode reads a fragmented MP4 from in, as the camera's reader wrote it, and writes
// fragmented MP4 with an H.264 track to out, until ctx is cancelled, in ends or it fails. It must
// return when ctx is. The Hub runs one per camera being watched, never one per viewer, and never
// more than Config.MaxTranscodes at once.
type Transcoder interface {
	Transcode(ctx context.Context, camera, codec string, width, height int, in io.Reader, out io.Writer) error
}

// Config is what a Hub takes.
type Config struct {
	// ViewersPerCamera is how many browsers may watch one camera at once. Zero takes
	// DefaultViewersPerCamera. The camera is read once however many watch; this bounds what the
	// node sends.
	ViewersPerCamera int
	// Viewers is how many may watch on this node in all. Zero takes DefaultViewers.
	Viewers int
	// Queue is how many samples one viewer may be behind before it is skipped ahead to the next
	// keyframe. Zero takes DefaultQueue.
	Queue int
	// GOPBytes bounds the picture group kept so a viewer who arrives between keyframes starts at
	// once instead of waiting for the next one. A group larger than this is not kept, and a
	// viewer who arrives then waits for a keyframe. Zero takes DefaultGOPBytes.
	GOPBytes int

	// Transcoder converts a camera whose codec is not H.264. Nil leaves such a camera as it is,
	// and a browser that cannot decode it says so.
	Transcoder Transcoder
	// MaxTranscodes is how many cameras may be converted at once on this node. Zero is one.
	// A viewer of one more is told the node is busy (ErrBusy).
	MaxTranscodes int
	// TranscodeIdle is how long a camera keeps being converted after its last viewer left, so a
	// page that reloads does not pay to start it again. Zero takes DefaultTranscodeIdle.
	TranscodeIdle time.Duration
	// EndGrace is how long a camera whose stream ended keeps its viewers waiting before it tells
	// them there is no signal. A camera whose address was renewed, or whose reader reconnected,
	// opens a new source within seconds, and the viewers' sockets carry on through it. Zero takes
	// DefaultEndGrace.
	EndGrace time.Duration
	// TranscodeRetry is how long a transcoder that stopped by itself waits before it is started
	// again for viewers who are still there. Zero takes DefaultTranscodeRetry.
	TranscodeRetry time.Duration

	Logger *slog.Logger
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Hub takes each camera's compressed video once and hands it to whoever watches.
//
// The camera's reader opens a source with Open and writes the fragmented MP4 it produced; viewers
// Subscribe. Nothing in between waits: a viewer that cannot keep up is skipped ahead, and a
// camera nobody watches costs the parse of its container and nothing else.
//
// A camera whose video is not H.264 is converted, on demand, when a Transcoder is configured: the
// reader's copy is kept as it came, and fed to the transcoder from the moment the first viewer
// arrives until a few seconds after the last one leaves. Viewers then get the converted stream. An
// H.264 camera is handed on as it is, and never reaches a transcoder.
type Hub struct {
	cfg Config
	log *slog.Logger

	mu      sync.Mutex
	cameras map[string]*camera
	viewers int
	jobs    int
	closed  bool
	// sources counts the sources ever opened, so no source is mistaken for an older one of the
	// same camera that was forgotten in between.
	sources uint64
}

// stream is what a viewer is given: a track and the pictures since the last keyframe.
type stream struct {
	track *Track
	// gop is the samples since the last keyframe, the keyframe first.
	gop      []*Sample
	gopBytes int
	// gopBroken is a group too big to keep: nobody can start inside it.
	gopBroken bool
}

func (st *stream) reset() { *st = stream{} }

func (st *stream) add(sp *Sample, limit int) {
	switch {
	case sp.Key:
		st.gop, st.gopBytes, st.gopBroken = append(st.gop[:0:0], sp), len(sp.Data), false
	case !st.gopBroken && len(st.gop) > 0:
		st.gop = append(st.gop, sp)
		st.gopBytes += len(sp.Data)
	}
	if st.gopBytes > limit {
		st.gop, st.gopBytes, st.gopBroken = nil, 0, true
	}
}

// camera is one camera's side of the hub, alive while it has a source or a viewer.
type camera struct {
	id string
	// generation is the source that is heard: a source that was replaced stops being.
	generation uint64

	// native is what the camera's reader writes, derived what the transcoder writes.
	native, derived stream
	// transcode is set when the native track is one a browser may not decode and a Transcoder
	// exists: viewers are then given derived.
	transcode bool

	// The camera's container as it came, kept while it needs transcoding: the init segment, and the
	// fragments since the last keyframe.
	rawInit []byte
	rawGOP  [][]byte
	rawFeed *rawFeed

	job        *job
	idle       *time.Timer
	derivedGen uint64
	// ending is the grace a closed source leaves before the viewers are told the signal is gone.
	ending *time.Timer

	subs map[*Subscription]struct{}
}

// view is the stream viewers are given.
func (c *camera) view() *stream {
	if c.transcode {
		return &c.derived
	}
	return &c.native
}

// job is one conversion of one camera.
type job struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// rawFeed hands the camera's container to a transcoder: the init segment and the group of
// pictures in progress first, then every fragment as it arrives.
type rawFeed struct {
	ch     chan []byte
	closed bool
}

func (f *rawFeed) offer(b []byte) {
	if f.closed {
		return
	}
	select {
	case f.ch <- b:
	default:
		// The transcoder fell behind. Fragments cannot be skipped inside a group, so the feed ends
		// and the conversion starts again from the last keyframe.
		f.closed = true
		close(f.ch)
	}
}

func (f *rawFeed) close() {
	if !f.closed {
		f.closed = true
		close(f.ch)
	}
}

// NewHub returns a Hub.
func NewHub(cfg Config) *Hub {
	if cfg.ViewersPerCamera <= 0 {
		cfg.ViewersPerCamera = DefaultViewersPerCamera
	}
	if cfg.Viewers <= 0 {
		cfg.Viewers = DefaultViewers
	}
	if cfg.Queue <= 0 {
		cfg.Queue = DefaultQueue
	}
	if cfg.GOPBytes <= 0 {
		cfg.GOPBytes = DefaultGOPBytes
	}
	if cfg.MaxTranscodes <= 0 {
		cfg.MaxTranscodes = 1
	}
	if cfg.TranscodeIdle <= 0 {
		cfg.TranscodeIdle = DefaultTranscodeIdle
	}
	if cfg.TranscodeRetry <= 0 {
		cfg.TranscodeRetry = DefaultTranscodeRetry
	}
	if cfg.EndGrace <= 0 {
		cfg.EndGrace = DefaultEndGrace
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Hub{cfg: cfg, log: cfg.Logger, cameras: make(map[string]*camera)}
}

func (h *Hub) camera(id string) *camera {
	c := h.cameras[id]
	if c == nil {
		c = &camera{id: id, subs: make(map[*Subscription]struct{})}
		h.cameras[id] = c
	}
	return c
}

// forget drops a camera nothing refers to, so the map is bounded by live sources and viewers and
// not by the ids anyone has asked for.
func (h *Hub) forget(c *camera) {
	if c.native.track == nil && len(c.subs) == 0 && c.job == nil && h.cameras[c.id] == c {
		delete(h.cameras, c.id)
	}
}

// browserDecodes says whether a codec string is one every current browser decodes. H.264 is; H.265
// depends on the browser and the machine, so it is converted.
func browserDecodes(codec string) bool {
	return strings.HasPrefix(codec, "avc1.") || strings.HasPrefix(codec, "avc3.")
}

// source is one run of a camera's reader, or one run of its transcoder: the writer it writes the
// container to.
type source struct {
	hub        *Hub
	cam        string
	generation uint64
	derived    bool
	demux      *Demuxer
	failed     bool
	once       sync.Once

	// The fragment being read: what its first picture was, and the header that came before its data.
	fragSamples int
	fragKey     bool
	moof        []byte
	// ftyp is the file's first box, which comes before anybody knows what the stream is.
	ftyp []byte
}

// Open starts a source for a camera: the reader writes the fragmented MP4 it produced to the
// returned writer and closes it when the stream ends. It is what arandu-alpr's tap asks for, so
// the Hub can be handed to it as it is.
//
// A source opened while another is open for the same camera replaces it; the old one's writes
// are ignored from then on.
//
// Write never blocks and never fails the reader: a stream that is not what this package reads
// is logged once, and the writer swallows the rest. The reader has to keep draining the pipe
// whatever happens here, or the process behind it stalls, so the writer does not ask it to stop.
func (h *Hub) Open(cameraID string) io.WriteCloser {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.camera(cameraID)
	h.sources++
	c.generation = h.sources
	if c.ending != nil {
		c.ending.Stop()
		c.ending = nil
	}
	if c.native.track != nil {
		// The reader came back with a new stream, an address renewed or a reconnection: the viewers
		// keep their sockets and are given the new picture when it starts.
		h.endLocked(c, Event{Starting: true})
	}
	return h.newSource(cameraID, c.generation, false)
}

func (h *Hub) newSource(cameraID string, generation uint64, derived bool) *source {
	s := &source{hub: h, cam: cameraID, generation: generation, derived: derived}
	s.demux = NewDemuxer(s.onTrack, s.onSample)
	s.demux.now = h.cfg.Now
	s.demux.Boxes(s.onBox)
	return s
}

func (s *source) Write(p []byte) (int, error) {
	if s.failed {
		return len(p), nil
	}
	if _, err := s.demux.Write(p); err != nil {
		s.failed = true
		s.hub.log.Warn("live: the camera's video was not read", "camera", s.cam, "derived", s.derived, "error", err)
	}
	return len(p), nil
}

func (s *source) Close() error {
	s.once.Do(func() {
		h := s.hub
		h.mu.Lock()
		defer h.mu.Unlock()
		c := h.cameras[s.cam]
		if c == nil {
			return
		}
		if s.derived {
			if c.derivedGen == s.generation {
				c.derived.reset()
				h.derivedEndedLocked(c)
			}
			return
		}
		if c.generation != s.generation {
			return
		}
		// Not yet: a reader that reconnects opens the next source in a moment, and the viewers
		// should not see a "no signal" for it. Their picture stops, their sockets do not.
		generation := s.generation
		c.native.gop, c.native.gopBytes, c.rawGOP = nil, 0, nil
		for sub := range c.subs {
			sub.reset(Event{Starting: true}, h.cfg.Queue)
		}
		c.ending = time.AfterFunc(h.cfg.EndGrace, func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if c.generation != generation || c.ending == nil {
				return
			}
			c.ending = nil
			h.endLocked(c, Event{Ended: true})
			h.forget(c)
		})
	})
	return nil
}

// heard is the camera when this source is the one being listened to.
func (s *source) heard() *camera {
	c := s.hub.cameras[s.cam]
	if c == nil {
		return nil
	}
	if s.derived {
		if c.derivedGen != s.generation || !c.transcode {
			return nil
		}
		return c
	}
	if c.generation != s.generation {
		return nil
	}
	return c
}

func (s *source) onTrack(t Track) {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	c := s.heard()
	if c == nil {
		return
	}
	if s.derived {
		c.derived = stream{track: &t}
		for sub := range c.subs {
			sub.reset(Event{Track: &t}, h.cfg.Queue)
		}
		return
	}
	c.native = stream{track: &t}
	c.rawInit, c.rawGOP = nil, nil
	c.transcode = h.cfg.Transcoder != nil && !browserDecodes(t.Codec)
	if c.transcode {
		// Viewers wait for the converted picture; the conversion starts when the container's header
		// is in (onBox), if anybody is there to see it.
		c.derived.reset()
		for sub := range c.subs {
			sub.reset(Event{Starting: true}, h.cfg.Queue)
		}
		return
	}
	for sub := range c.subs {
		sub.reset(Event{Track: &t}, h.cfg.Queue)
	}
}

func (s *source) onSample(sample Sample) {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.fragSamples == 0 {
		s.fragKey = sample.Key
	}
	s.fragSamples++
	c := s.heard()
	if c == nil {
		return
	}
	sp := &sample
	if s.derived {
		if c.derived.track == nil {
			return
		}
		c.derived.add(sp, h.cfg.GOPBytes)
		for sub := range c.subs {
			sub.offer(sp)
		}
		return
	}
	if c.native.track == nil {
		return
	}
	c.native.add(sp, h.cfg.GOPBytes)
	if !c.transcode {
		for sub := range c.subs {
			sub.offer(sp)
		}
	}
}

// onBox keeps the camera's container as it came, for a transcoder, while the camera needs one.
func (s *source) onBox(kind string, box []byte) {
	if s.derived {
		return
	}
	if kind == "ftyp" {
		s.ftyp = append(s.ftyp[:0], box...)
		return
	}
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	c := s.heard()
	if c == nil || !c.transcode {
		s.fragSamples, s.moof = 0, nil
		return
	}
	switch kind {
	case "moov":
		c.rawInit = append(append([]byte(nil), s.ftyp...), box...)
		h.maybeStartLocked(c)
	case "moof":
		s.moof = append(s.moof[:0], box...)
		s.fragSamples = 0
	case "mdat":
		if s.moof == nil || len(c.rawInit) == 0 {
			return
		}
		fragment := make([]byte, 0, len(s.moof)+len(box))
		fragment = append(append(fragment, s.moof...), box...)
		s.moof = nil
		if s.fragKey {
			c.rawGOP = c.rawGOP[:0:0]
		}
		if len(c.rawGOP) == 0 && !s.fragKey {
			return // a group that did not begin here cannot be started from
		}
		c.rawGOP = append(c.rawGOP, fragment)
		if f := c.rawFeed; f != nil {
			f.offer(fragment)
		}
	}
}

// endLocked says the camera has no picture now.
func (h *Hub) endLocked(c *camera, told Event) {
	c.native.reset()
	c.derived.reset()
	c.rawInit, c.rawGOP = nil, nil
	c.transcode = false
	h.stopJobLocked(c)
	for sub := range c.subs {
		sub.reset(told, h.cfg.Queue)
	}
}

// State says what a viewer of the camera would see now: "live" when there is a picture to send,
// "starting" when the camera has one but it is being converted for the browser, and "waiting"
// when there is none.
func (h *Hub) State(cameraID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.cameras[cameraID]
	switch {
	case c == nil || c.native.track == nil:
		return "waiting"
	case c.view().track == nil:
		return "starting"
	}
	return "live"
}

// Live says whether there is a picture to send for this camera now.
func (h *Hub) Live(cameraID string) bool { return h.State(cameraID) == "live" }

// Viewers is how many are watching the camera.
func (h *Hub) Viewers(cameraID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.cameras[cameraID]; c != nil {
		return len(c.subs)
	}
	return 0
}

// Transcoding is how many cameras are being converted now.
func (h *Hub) Transcoding() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.jobs
}

// Subscribe adds a viewer. It answers ErrFull when the camera or the node has all it allows, and
// ErrBusy when the camera has to be converted and the node is converting as many as it may.
//
// The viewer's first events are the track, when the camera has one, and the picture group since
// its last keyframe, so decoding starts at once. A camera with no picture gives a viewer no
// events until it has one.
func (h *Hub) Subscribe(cameraID string) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrFull
	}
	c := h.camera(cameraID)
	if len(c.subs) >= h.cfg.ViewersPerCamera || h.viewers >= h.cfg.Viewers {
		h.forget(c)
		return nil, ErrFull
	}
	if c.transcode && c.job == nil && h.jobs >= h.cfg.MaxTranscodes {
		h.forget(c)
		return nil, ErrBusy
	}
	view := c.view()
	s := &Subscription{hub: h, cam: c, ch: make(chan Event, h.cfg.Queue+len(view.gop)+2)}
	if view.track != nil {
		t := *view.track
		s.ch <- Event{Track: &t}
		if len(view.gop) > 0 {
			for _, sample := range view.gop {
				s.ch <- Event{Sample: sample}
			}
		} else {
			s.wantKey = true
		}
	} else {
		s.wantKey = true
	}
	c.subs[s] = struct{}{}
	h.viewers++
	if c.idle != nil {
		c.idle.Stop()
		c.idle = nil
	}
	h.maybeStartLocked(c)
	return s, nil
}

// Close ends every viewer and every conversion, and refuses new viewers.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for _, c := range h.cameras {
		h.stopJobLocked(c)
		for s := range c.subs {
			s.closeLocked()
		}
	}
}

// maybeStartLocked starts the conversion of a camera that needs one and has a viewer, when it is
// not already running and the node has room. Without room the viewers are told.
func (h *Hub) maybeStartLocked(c *camera) {
	if !c.transcode || c.job != nil || len(c.subs) == 0 || len(c.rawInit) == 0 || c.native.track == nil || h.closed {
		return
	}
	if h.jobs >= h.cfg.MaxTranscodes {
		for sub := range c.subs {
			sub.reset(Event{Busy: true}, h.cfg.Queue)
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{cancel: cancel, done: make(chan struct{})}
	c.job = j
	h.jobs++
	track := *c.native.track
	go h.run(ctx, c, j, track)
}

// stopJobLocked ends a camera's conversion and what it produced.
func (h *Hub) stopJobLocked(c *camera) {
	if c.idle != nil {
		c.idle.Stop()
		c.idle = nil
	}
	if c.job != nil {
		c.job.cancel()
		c.job = nil
		h.jobs--
	}
	if c.rawFeed != nil {
		c.rawFeed.close()
		c.rawFeed = nil
	}
	c.derived.reset()
	c.derivedGen = 0
}

// derivedEndedLocked is what happens when the converted stream ends by itself: the viewers wait
// for it to start again.
func (h *Hub) derivedEndedLocked(c *camera) {
	if !c.transcode {
		return
	}
	for sub := range c.subs {
		sub.reset(Event{Starting: true}, h.cfg.Queue)
	}
}

// run converts one camera until it is cancelled, starting the transcoder again when it stops by
// itself while viewers are still there.
func (h *Hub) run(ctx context.Context, c *camera, j *job, track Track) {
	defer close(j.done)
	for ctx.Err() == nil {
		h.mu.Lock()
		if c.job != j {
			h.mu.Unlock()
			return
		}
		feed := &rawFeed{ch: make(chan []byte, rawFeedDepth)}
		// The init segment and the group in progress first: the transcoder starts on a keyframe.
		feed.ch <- append([]byte(nil), c.rawInit...)
		for _, f := range c.rawGOP {
			feed.ch <- f
		}
		c.rawFeed = feed
		h.sources++
		out := h.newSource(c.id, h.sources, true)
		c.derivedGen = out.generation
		c.derived.reset()
		h.mu.Unlock()

		pr, pw := io.Pipe()
		go func() {
			for {
				select {
				case b, ok := <-feed.ch:
					if !ok {
						_ = pw.Close()
						return
					}
					if _, err := pw.Write(b); err != nil {
						return
					}
				case <-ctx.Done():
					_ = pw.CloseWithError(ctx.Err())
					return
				}
			}
		}()
		err := h.cfg.Transcoder.Transcode(ctx, c.id, track.Codec, track.Width, track.Height, pr, out)
		_ = pr.Close()
		_ = out.Close()
		h.mu.Lock()
		if c.rawFeed == feed {
			feed.close()
			c.rawFeed = nil
		}
		h.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		h.log.Warn("live: the conversion stopped", "camera", c.id, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.cfg.TranscodeRetry):
		}
	}
}

// Event is one thing a viewer is told: a track to configure a decoder with, a sample to decode,
// that the camera has no picture any more, that the picture is being prepared, or that the node
// cannot prepare it.
type Event struct {
	Track    *Track
	Sample   *Sample
	Ended    bool
	Starting bool
	Busy     bool
}

// Subscription is one viewer's place in the hub.
type Subscription struct {
	hub *Hub
	cam *camera
	ch  chan Event

	// The fields below are guarded by hub.mu.
	wantKey bool
	dropped uint64
	done    bool
}

// Events delivers what the camera sends this viewer. The channel closes when the viewer is
// closed or the hub is.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Dropped is how many times this viewer fell behind and was skipped ahead to a keyframe.
func (s *Subscription) Dropped() uint64 {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	return s.dropped
}

// Close removes the viewer. It is safe to call twice.
func (s *Subscription) Close() {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	s.closeLocked()
}

func (s *Subscription) closeLocked() {
	if s.done {
		return
	}
	s.done = true
	h := s.hub
	c := s.cam
	delete(c.subs, s)
	h.viewers--
	if len(c.subs) == 0 && c.job != nil && c.idle == nil && !h.closed {
		// The last viewer left: keep converting a little in case the page comes back.
		j := c.job
		c.idle = time.AfterFunc(h.cfg.TranscodeIdle, func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if c.job == j && len(c.subs) == 0 {
				h.stopJobLocked(c)
				h.forget(c)
			}
		})
	}
	h.forget(c)
	close(s.ch)
}

// offer hands the viewer a sample without waiting. A viewer that is full is emptied and waits for
// the next keyframe, because a decoder given a picture group with holes in it shows garbage.
func (s *Subscription) offer(sample *Sample) {
	if s.done {
		return
	}
	if s.wantKey && !sample.Key {
		return
	}
	select {
	case s.ch <- Event{Sample: sample}:
		if sample.Key {
			s.wantKey = false
		}
		return
	default:
	}
	// Full. The consumer may take from the channel while this empties it; both are safe.
	s.drain()
	s.dropped++
	s.wantKey = true
	if sample.Key {
		s.ch <- Event{Sample: sample}
		s.wantKey = false
	}
}

func (s *Subscription) drain() {
	for drained := false; !drained; {
		select {
		case <-s.ch:
		default:
			drained = true
		}
	}
}

// reset replaces what the viewer was waiting to be told with one event, and waits for a keyframe.
func (s *Subscription) reset(e Event, queue int) {
	if s.done {
		return
	}
	s.drain()
	s.wantKey = true
	select {
	case s.ch <- e:
	default:
	}
}
