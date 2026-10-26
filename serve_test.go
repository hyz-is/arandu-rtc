package rtc_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/hyz-is/arandu-rtc"
)

type rig struct {
	hub    *rtc.Hub
	signer *rtc.Signer
	server *httptest.Server
}

func newRig(t *testing.T, cfg rtc.ServerConfig, attach rtc.Attach, hubCfg ...rtc.Config) *rig {
	t.Helper()
	hc := rtc.Config{ViewersPerCamera: 2}
	if len(hubCfg) > 0 {
		hc = hubCfg[0]
	}
	hub := rtc.NewHub(hc)
	signer, err := rtc.NewSigner(secret, 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Hub, cfg.Signer = hub, signer
	srv, err := rtc.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live/{camera}", func(w http.ResponseWriter, r *http.Request) {
		srv.Serve(w, r, r.PathValue("camera"), attach)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(func() { ts.Close(); hub.Close() })
	return &rig{hub: hub, signer: signer, server: ts}
}

func (r *rig) url(camera, pass string) string {
	return "ws" + strings.TrimPrefix(r.server.URL, "http") + "/live/" + camera + "?pass=" + pass
}

func (r *rig) dial(t *testing.T, camera string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	pass, _ := r.signer.Issue(camera, "account-1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return websocket.Dial(ctx, r.url(camera, pass), &websocket.DialOptions{HTTPHeader: header})
}

func read(t *testing.T, c *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	kind, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return kind, b
}

func TestTheSocketWritesTheTrackThenEachPicture(t *testing.T) {
	r := newRig(t, rtc.ServerConfig{}, nil)
	src := r.hub.Open("cam")
	data := clip(t, "h264.mp4")
	feed(t, src, data[:len(data)/3], 1000)

	conn, _, err := r.dial(t, "cam", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	kind, b := read(t, conn)
	if kind != websocket.MessageText {
		t.Fatalf("the first message is %v, want the track as text", kind)
	}
	var track struct {
		Type, Codec, Description string
		Width, Height            int
		Timescale                uint32
	}
	if err := json.Unmarshal(b, &track); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	if track.Type != "video" || !strings.HasPrefix(track.Codec, "avc1.") || track.Width != 160 || track.Height != 120 || track.Timescale == 0 {
		t.Fatalf("track = %s", b)
	}
	if d, err := base64.StdEncoding.DecodeString(track.Description); err != nil || len(d) < 7 || d[0] != 1 {
		t.Fatalf("description %q is not a base64 avcC record", track.Description)
	}

	// The picture group in progress arrives first, from a keyframe, then the live ones.
	feed(t, src, data[len(data)/3:], 1000)
	var keys, pictures int
	var lastPTS int64 = -1
	for pictures < 30 {
		kind, b := read(t, conn)
		if kind != websocket.MessageBinary {
			t.Fatalf("message %d is %v, want a picture", pictures, kind)
		}
		if b[0] != rtc.SampleMessage || len(b) <= rtc.SampleHeader || !nalsOK(b[rtc.SampleHeader:]) {
			t.Fatalf("message %d is not a picture: % x", pictures, b[:min(len(b), 24)])
		}
		if pictures == 0 && b[1]&rtc.KeyFlag == 0 {
			t.Fatal("the first picture is not a keyframe")
		}
		if b[1]&rtc.KeyFlag != 0 {
			keys++
		}
		wall := int64(binary.BigEndian.Uint64(b[2:]))
		pts := int64(binary.BigEndian.Uint64(b[10:]))
		if d := time.Since(time.UnixMilli(wall)); d < -time.Second || d > 5*time.Second {
			t.Fatalf("picture %d is dated %s ago", pictures, d)
		}
		if pts < lastPTS {
			t.Fatalf("picture %d goes back in time: %d after %d", pictures, pts, lastPTS)
		}
		lastPTS = pts
		pictures++
		if pictures > 30 {
			break
		}
	}
	if keys < 3 {
		t.Errorf("%d keyframes in 30 pictures of ten-picture groups", keys)
	}
}

func TestTheSocketSaysWaitingThenLostThenStartsOver(t *testing.T) {
	r := newRig(t, rtc.ServerConfig{}, nil, rtc.Config{ViewersPerCamera: 2, EndGrace: 20 * time.Millisecond})
	conn, _, err := r.dial(t, "cam", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	if _, b := read(t, conn); string(b) != `{"type":"signal","state":"waiting"}` {
		t.Fatalf("first message %s, want waiting", b)
	}
	src := r.hub.Open("cam")
	data := clip(t, "h264.mp4")
	feed(t, src, data[:len(data)/2], 2000)
	if _, b := read(t, conn); !strings.Contains(string(b), `"type":"video"`) {
		t.Fatalf("after the camera came up: %s", b)
	}
	_ = src.Close()
	for {
		kind, b := read(t, conn)
		if kind == websocket.MessageText {
			if string(b) == `{"type":"signal","state":"starting"}` {
				continue // the stream stopped; the camera may be back in a moment
			}
			if string(b) != `{"type":"signal","state":"lost"}` {
				t.Fatalf("after the camera went: %s", b)
			}
			break
		}
	}
}

func TestAPassForAnotherCameraOrNoPassIsRefusedBeforeTheUpgrade(t *testing.T) {
	r := newRig(t, rtc.ServerConfig{}, nil)
	other, _ := r.signer.Issue("cam-2", "x")
	for name, url := range map[string]string{
		"no pass":        r.url("cam-1", ""),
		"another camera": r.url("cam-1", other),
		"garbage":        r.url("cam-1", "a.b"),
	} {
		_, resp, err := websocket.Dial(context.Background(), url, nil)
		if err == nil {
			t.Errorf("%s: the socket opened", name)
			continue
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: answered %v, want 401", name, resp)
		}
	}
	if r.hub.Viewers("cam-1") != 0 {
		t.Error("a refused viewer took a place")
	}
}

// fullSignal is what a viewer of a camera with all its viewers is told: in a message, after the
// upgrade, because a browser cannot read the status of a refused handshake.
func fullSignal(t *testing.T, r *rig, camera string, header http.Header) bool {
	t.Helper()
	c, _, err := r.dial(t, camera, header)
	if err != nil {
		t.Fatalf("a viewer who is turned away still gets a socket to be told on: %v", err)
	}
	defer c.CloseNow()
	_, b := read(t, c)
	if string(b) != `{"type":"signal","state":"full"}` {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err = c.Read(ctx)
	return websocket.CloseStatus(err) == websocket.StatusTryAgainLater
}

func TestACameraWithAllItsViewersSaysSoAndAFreedPlaceIsReused(t *testing.T) {
	r := newRig(t, rtc.ServerConfig{}, nil)
	a, _, err := r.dial(t, "cam", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := r.dial(t, "cam", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.CloseNow()
	if !fullSignal(t, r, "cam", nil) {
		t.Fatal("a third viewer was not told the camera is full and closed")
	}
	if r.hub.Viewers("cam") != 2 {
		t.Fatalf("a viewer who was turned away holds a place: %d", r.hub.Viewers("cam"))
	}

	_ = a.Close(websocket.StatusNormalClosure, "")
	until(t, "the place is freed", func() bool { return r.hub.Viewers("cam") <= 1 })
	c, _, err := r.dial(t, "cam", nil)
	if err != nil {
		t.Fatalf("after one left: %v", err)
	}
	c.CloseNow()
}

func TestOnlyThePagesNamedMayOpenTheSocket(t *testing.T) {
	r := newRig(t, rtc.ServerConfig{Origins: []string{"https://alpr.sh"}}, nil)
	good, _, err := r.dial(t, "cam", http.Header{"Origin": {"https://alpr.sh"}})
	if err != nil {
		t.Fatalf("the named page: %v", err)
	}
	good.CloseNow()
	_, resp, err := r.dial(t, "cam", http.Header{"Origin": {"https://evil.example"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("another page: err=%v resp=%v, want 403", err, resp)
	}
}

func TestTheApplicationsMessagesRideTheSameSocketAndDetachAtTheEnd(t *testing.T) {
	feedc := make(chan []byte, 4)
	detached := make(chan rtc.Claims, 1)
	var attached rtc.Claims
	r := newRig(t, rtc.ServerConfig{}, func(c rtc.Claims) (<-chan []byte, func()) {
		attached = c
		return feedc, func() { detached <- c }
	})
	conn, _, err := r.dial(t, "cam", nil)
	if err != nil {
		t.Fatal(err)
	}
	read(t, conn) // waiting
	feedc <- []byte(`{"type":"overlay","items":[]}`)
	if kind, b := read(t, conn); kind != websocket.MessageText || string(b) != `{"type":"overlay","items":[]}` {
		t.Fatalf("got %v %s", kind, b)
	}
	if attached.Camera != "cam" || attached.Subject != "account-1" {
		t.Errorf("the application was told %+v", attached)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	select {
	case <-detached:
	case <-time.After(2 * time.Second):
		t.Fatal("the application was not told the viewer left")
	}
}

func TestASessionEndsOnItsOwnAndSaysSo(t *testing.T) {
	r := newRig(t, rtc.ServerConfig{MaxSession: 300 * time.Millisecond}, nil)
	conn, _, err := r.dial(t, "cam", nil)
	if err != nil {
		t.Fatal(err)
	}
	read(t, conn) // waiting
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != rtc.StatusSessionEnded {
		t.Fatalf("closed with %v, want status %d", err, rtc.StatusSessionEnded)
	}
	deadline := time.Now().Add(2 * time.Second)
	for r.hub.Viewers("cam") > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.hub.Viewers("cam") != 0 {
		t.Error("the viewer kept its place after the session ended")
	}
}

func TestAKeepaliveGoesOutOnAnIdleSocket(t *testing.T) {
	r := newRig(t, rtc.ServerConfig{PingInterval: 50 * time.Millisecond}, nil)
	conn, _, err := r.dial(t, "cam", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	read(t, conn) // waiting
	if _, b := read(t, conn); string(b) != `{"type":"ping"}` {
		t.Fatalf("got %s, want the keepalive", b)
	}
}

func TestAServerWantsAHubAndASigner(t *testing.T) {
	if _, err := rtc.NewServer(rtc.ServerConfig{}); err == nil {
		t.Error("a Server with neither was built")
	}
}

func TestAPictureLargerThanAMessageIsSentInPiecesThatJoinBack(t *testing.T) {
	const max = 512
	r := newRig(t, rtc.ServerConfig{MaxMessage: max}, nil)
	src := r.hub.Open("cam")
	data := clip(t, "h264.mp4")
	feed(t, src, data[:len(data)/3], 1000)

	conn, _, err := r.dial(t, "cam", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	read(t, conn) // the track

	var got [][]byte
	var current []byte
	more := false
	for len(got) < 6 {
		kind, b := read(t, conn)
		if kind != websocket.MessageBinary {
			t.Fatalf("a text message in the middle of the pictures: %s", b)
		}
		if len(b) > max {
			t.Fatalf("a message of %d bytes, the limit is %d", len(b), max)
		}
		switch b[0] {
		case rtc.SampleMessage:
			if more {
				t.Fatal("a new picture began before the last one ended")
			}
			current = append([]byte(nil), b[rtc.SampleHeader:]...)
			more = b[1]&rtc.MoreFlag != 0
		case rtc.ContinuationMessage:
			if !more {
				t.Fatal("a continuation with nothing to continue")
			}
			current = append(current, b[rtc.ContinuationHeader:]...)
			more = b[1]&rtc.MoreFlag != 0
		default:
			t.Fatalf("message type %d", b[0])
		}
		if !more {
			got = append(got, current)
		}
	}
	pieces := 0
	for i, sample := range got {
		if !nalsOK(sample) {
			t.Fatalf("picture %d joined back is not length-prefixed NAL units", i)
		}
		if len(sample) > max {
			pieces++
		}
	}
	if pieces == 0 {
		t.Error("no picture was larger than a message, so nothing was split: the fixture's keyframe is smaller than the limit")
	}
}

func TestAServerRefusesALimitTooSmallToCarryAPicture(t *testing.T) {
	hub := rtc.NewHub(rtc.Config{})
	signer, _ := rtc.NewSigner(secret, 0)
	if _, err := rtc.NewServer(rtc.ServerConfig{Hub: hub, Signer: signer, MaxMessage: 10}); err == nil {
		t.Error("a MaxMessage with no room for the header was accepted")
	}
}

func TestTheAnswersAreNeverCachedOrBufferedByWhatSitsInFront(t *testing.T) {
	r := newRig(t, rtc.ServerConfig{}, nil)
	conn, resp, err := r.dial(t, "cam", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("the handshake answered with %v", resp.Header)
	}
	_, resp, err = websocket.Dial(context.Background(), r.url("cam", "x.y"), nil)
	if err == nil || resp == nil || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("a refusal was cacheable: %v", resp)
	}
}

// A camera that loses its signal is what makes a socket idle, and an idle socket is what a proxy in
// front of the node closes. The keepalive does not depend on the camera.
func TestTheKeepaliveContinuesWhenTheCameraStopsSendingPictures(t *testing.T) {
	r := newRig(t, rtc.ServerConfig{PingInterval: 60 * time.Millisecond}, nil)
	src := r.hub.Open("cam")
	data := clip(t, "h264.mp4")
	feed(t, src, data[:len(data)/4], 1000)
	conn, _, err := r.dial(t, "cam", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	// Drain what the camera sent, then the camera goes quiet, source still open.
	deadline := time.After(3 * time.Second)
	pings := 0
	for pings < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d keepalives from a camera that went quiet", pings)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, b, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if string(b) == `{"type":"ping"}` {
			pings++
		}
	}
}

// The limits count viewers of a camera and of the node, never addresses: behind a proxy every
// viewer arrives from the proxy's, and a header naming the client is the client's to write.
func TestTheLimitsDoNotDependOnTheClientAddress(t *testing.T) {
	r := newRig(t, rtc.ServerConfig{}, nil)
	var open []*websocket.Conn
	defer func() {
		for _, c := range open {
			c.CloseNow()
		}
	}()
	for i, ip := range []string{"203.0.113.1", "203.0.113.2"} {
		c, _, err := r.dial(t, "cam", http.Header{"CF-Connecting-IP": {ip}, "X-Forwarded-For": {ip}})
		if err != nil {
			t.Fatalf("viewer %d: %v", i, err)
		}
		open = append(open, c)
	}
	if !fullSignal(t, r, "cam", http.Header{"CF-Connecting-IP": {"198.51.100.9"}, "X-Forwarded-For": {"198.51.100.9"}}) {
		t.Fatal("a third viewer with a new address was let in")
	}
}

// A node runs two Servers on /live/{camera}: the live video's, and the tile's on a hub of its own.
// Each takes only the pass of its kind.
func TestAServerOfAKindTakesOnlyThePassOfThatKind(t *testing.T) {
	tile := newRig(t, rtc.ServerConfig{Kind: rtc.KindTile}, nil)
	video := newRig(t, rtc.ServerConfig{}, nil)

	dial := func(r *rig, kind string) error {
		pass, _ := r.signer.IssueKind(kind, "cam", "account-1")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, resp, err := websocket.Dial(ctx, r.url("cam", pass), nil)
		if err == nil {
			_ = c.CloseNow()
		} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("kind %q refused with %v, want 401", kind, resp)
		}
		return err
	}
	if err := dial(tile, rtc.KindTile); err != nil {
		t.Errorf("the tile server refused a tile pass: %v", err)
	}
	if err := dial(tile, ""); err == nil {
		t.Error("the tile server took a live video pass")
	}
	if err := dial(tile, rtc.KindSnapshot); err == nil {
		t.Error("the tile server took a snapshot pass")
	}
	if err := dial(video, ""); err != nil {
		t.Errorf("the video server refused a video pass: %v", err)
	}
	if err := dial(video, rtc.KindTile); err == nil {
		t.Error("the video server took a tile pass")
	}
}

func TestTheServerCountsWhatItSends(t *testing.T) {
	var mu sync.Mutex
	var total, messages int
	r := newRig(t, rtc.ServerConfig{Sent: func(n int) { mu.Lock(); total += n; messages++; mu.Unlock() }}, nil)
	src := r.hub.Open("cam")
	data := clip(t, "h264.mp4")
	feed(t, src, data, 1000)

	conn, _, err := r.dial(t, "cam", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var received int
	for i := 0; i < 5; i++ { // the track, then the group in progress
		_, b := read(t, conn)
		received += len(b)
	}
	// The count follows the write by an instant, and the browser may have read the message first.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := messages >= 5 && total >= received
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if messages < 5 || total < received {
		t.Errorf("counted %d bytes in %d messages, but the browser read %d bytes in 5", total, messages, received)
	}
}
