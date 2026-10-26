package rtc

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// The wire format of the socket, server to browser.
//
// Text messages are JSON objects with a "type":
//
//	{"type":"video","codec":"avc1.640028","width":1920,"height":1080,"timescale":90000,"description":"<base64 avcC or hvcC>"}
//	    the decoder's configuration, before the first sample and again whenever the camera's stream
//	    starts over. A browser configures a VideoDecoder with codec, size and description.
//	{"type":"signal","state":"waiting"|"starting"|"lost"|"busy"|"full"}
//	    the camera has no picture yet; has one that is being converted for the browser; had one
//	    and has lost it; or the node cannot convert it now ("busy"), or the camera has all the
//	    viewers it allows ("full"), after which the server closes the socket. Samples follow a new
//	    "video".
//	{"type":"ping"}
//	    keepalive for a proxy that closes an idle socket.
//
// Anything else is the application's, relayed from the feed it attached.
//
// Binary messages are one coded picture each:
//
//	byte  0      SampleMessage (1)
//	byte  1      flags: bit 0 is set on a keyframe, bit 1 when more of this picture follows
//	bytes 2..9   wall clock when the node had the picture, Unix milliseconds, big endian
//	bytes 10..17 presentation time from the stream's start, microseconds, big endian
//	bytes 18..   the picture in length-prefixed NAL units
//
// A message is never larger than ServerConfig.MaxMessage (128 KiB by default), because the
// socket usually crosses a proxy and a proxy is kinder to many small messages than to a keyframe
// of a megabyte. A picture that does not fit is sent in pieces, written one after the other with
// nothing between them: the first is the message above with flag bit 1 set, and each further
// piece is
//
//	byte  0      ContinuationMessage (2)
//	byte  1      flags: bit 1 while more follows, clear on the last piece
//	bytes 2..    the next bytes of the picture
//
// and the page joins the pieces until the flag is clear, then decodes the whole picture.
//
// The socket is never idle for longer than ServerConfig.PingInterval, even when the camera
// sends nothing: a proxy in front of the node closes a WebSocket that carries no data for about
// a hundred seconds, and a camera that lost its signal is exactly what makes a socket idle. The
// browser sends nothing the server acts on.
const (
	SampleMessage byte = 1
	// ContinuationMessage carries the next bytes of a picture that did not fit in one message.
	ContinuationMessage byte = 2
	// SampleHeader is the length of a binary message before the picture.
	SampleHeader = 18
	// ContinuationHeader is the length of a continuation message before the picture's bytes.
	ContinuationHeader = 2
	// KeyFlag is set in the flags byte of a keyframe.
	KeyFlag byte = 1
	// MoreFlag is set while more pieces of the same picture follow.
	MoreFlag byte = 2

	// StatusSessionEnded closes a socket that reached ServerConfig.MaxSession; a page asks for a
	// new pass and opens another.
	StatusSessionEnded websocket.StatusCode = 4001
)

// Defaults of ServerConfig.
const (
	DefaultMaxSession   = 15 * time.Minute
	DefaultPingInterval = 15 * time.Second
	DefaultWriteTimeout = 10 * time.Second
	// DefaultMaxMessage is the largest message the server writes.
	DefaultMaxMessage = 128 << 10
	// MinMaxMessage is the smallest MaxMessage accepted: a header and something to carry.
	MinMaxMessage   = SampleHeader + 16
	clientReadLimit = 4 << 10
)

// ServerConfig is what a Server takes.
type ServerConfig struct {
	Hub    *Hub
	Signer *Signer

	// Kind is the kind of pass this Server opens: empty for the live video's, or KindTile and
	// the like for a Server that serves a hub of its own. A pass of any other kind is 401, which
	// is how a node runs two Servers on one route and tells them apart by the pass.
	Kind string

	// Origins are the pages that may open the socket, as origins (https://alpr.sh) or hosts. A
	// page of any other origin is refused. Empty checks none, which is safe only because the pass
	// is what guards the socket, and the browser cannot attach one on its own.
	Origins []string

	// MaxSession is how long one socket lives. Zero takes DefaultMaxSession.
	MaxSession time.Duration
	// PingInterval is the keepalive period. Zero takes DefaultPingInterval. It has to stay well
	// under what a proxy in front of the node tolerates: Cloudflare closes a WebSocket with no data
	// for about 100 seconds.
	PingInterval time.Duration
	// MaxMessage is the largest message written to a browser, in bytes. Zero takes
	// DefaultMaxMessage; a picture larger than that is sent in pieces.
	MaxMessage int
	// WriteTimeout bounds one write. Zero takes DefaultWriteTimeout.
	WriteTimeout time.Duration

	// Sent, when set, is called with the size of every message written to a browser, for a node's
	// counter of what it sends. It runs on the socket's writer, so it must not wait.
	Sent func(bytes int)

	Logger *slog.Logger
}

// Server answers the browsers: it checks the pass, takes a place in the Hub and writes the
// camera's video to the socket.
type Server struct {
	hub        *Hub
	signer     *Signer
	kind       string
	origins    []string
	maxSession time.Duration
	ping       time.Duration
	write      time.Duration
	maxMessage int
	sent       func(int)
	log        *slog.Logger
}

// NewServer returns a Server.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Hub == nil || cfg.Signer == nil {
		return nil, errors.New("live: a Server needs a Hub and a Signer")
	}
	s := &Server{hub: cfg.Hub, signer: cfg.Signer, kind: cfg.Kind, maxSession: cfg.MaxSession, ping: cfg.PingInterval, write: cfg.WriteTimeout, log: cfg.Logger, maxMessage: cfg.MaxMessage, sent: cfg.Sent}
	for _, o := range cfg.Origins {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if strings.Contains(o, "://") {
			u, err := url.Parse(o)
			if err != nil || u.Host == "" {
				return nil, errors.New("live: " + o + " is not an origin")
			}
			o = u.Host
		}
		s.origins = append(s.origins, o)
	}
	if s.maxSession <= 0 {
		s.maxSession = DefaultMaxSession
	}
	if s.ping <= 0 {
		s.ping = DefaultPingInterval
	}
	if s.write <= 0 {
		s.write = DefaultWriteTimeout
	}
	if s.maxMessage == 0 {
		s.maxMessage = DefaultMaxMessage
	}
	if s.maxMessage < MinMaxMessage {
		return nil, fmt.Errorf("live: MaxMessage %d is below %d", s.maxMessage, MinMaxMessage)
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// Attach lets the application put its own text messages on a viewer's socket, such as the boxes
// drawn over the picture. It is called after the pass is accepted and a place is taken, with who
// the pass was issued to; the messages it returns are written as they come, and detach is called
// when the socket ends. A nil Attach adds nothing.
type Attach func(claims Claims) (messages <-chan []byte, detach func())

// Serve answers one browser asking to watch camera.
//
// A pass that is not valid for this camera is 401, before the upgrade: a refused page gets an
// ordinary status and not a socket that opens and closes. A camera with all its viewers, or one
// the node cannot convert now, is told in a message after the upgrade, because a browser cannot
// read the status of a handshake and the page has a sentence to say. The pass is read from the "pass" query parameter, because a browser cannot set a
// header on a WebSocket handshake.
func (s *Server) Serve(w http.ResponseWriter, r *http.Request, camera string, attach Attach) {
	var (
		claims Claims
		err    error
	)
	if s.kind == "" {
		claims, err = s.signer.Verify(r.URL.Query().Get("pass"), camera)
	} else {
		claims, err = s.signer.VerifyKind(r.URL.Query().Get("pass"), camera, s.kind)
	}
	if err != nil {
		refuse(w, http.StatusUnauthorized, "pass")
		return
	}
	sub, subErr := s.hub.Subscribe(camera)
	if subErr == nil {
		defer sub.Close()
	}

	// A socket is never cached or buffered by whatever sits in front of the node.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:     s.origins,
		InsecureSkipVerify: len(s.origins) == 0,
	})
	if err != nil {
		s.log.Debug("live: handshake failed", "camera", camera, "error", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(clientReadLimit)
	if subErr != nil {
		// Said in a message, not in a status: a browser cannot read the status of a refused
		// handshake, and the page owes the person a sentence about why there is no picture.
		state := "full"
		if errors.Is(subErr, ErrBusy) {
			state = "busy"
		}
		wctx, cancel := context.WithTimeout(r.Context(), s.write)
		defer cancel()
		_ = conn.Write(wctx, websocket.MessageText, signalMessage(state))
		_ = conn.Close(websocket.StatusTryAgainLater, state)
		return
	}

	var (
		messages <-chan []byte
		detach   func()
	)
	if attach != nil {
		messages, detach = attach(claims)
		if detach != nil {
			defer detach()
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.maxSession)
	defer cancel()
	go func() {
		// The reader is what notices the browser leave, and what answers its pings. It reads with
		// a context of its own: a library read that is cancelled closes the connection, and the
		// end of a session has to close it with its own status.
		defer cancel()
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				return
			}
		}
	}()

	err = s.pump(ctx, conn, sub, messages, camera)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		_ = conn.Close(StatusSessionEnded, "session ended")
	case err == nil:
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
}

// pump owns the socket's write side.
func (s *Server) pump(ctx context.Context, conn *websocket.Conn, sub *Subscription, messages <-chan []byte, camera string) error {
	ping := time.NewTicker(s.ping)
	defer ping.Stop()

	if state := s.hub.State(camera); state != "live" {
		if err := s.text(ctx, conn, signalMessage(state)); err != nil {
			return err
		}
	}
	events := sub.Events()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ping.C:
			if err := s.text(ctx, conn, []byte(`{"type":"ping"}`)); err != nil {
				return err
			}
		case msg, ok := <-messages:
			if !ok {
				messages = nil
				continue
			}
			if err := s.text(ctx, conn, msg); err != nil {
				return err
			}
		case e, ok := <-events:
			if !ok {
				return nil
			}
			var err error
			switch {
			case e.Track != nil:
				err = s.text(ctx, conn, trackMessage(*e.Track))
			case e.Ended:
				err = s.text(ctx, conn, signalMessage("lost"))
			case e.Starting:
				err = s.text(ctx, conn, signalMessage("starting"))
			case e.Busy:
				err = s.text(ctx, conn, signalMessage("busy"))
				if err == nil {
					return nil
				}
			case e.Sample != nil:
				for _, piece := range sampleMessages(*e.Sample, s.maxMessage) {
					if err = s.binary(ctx, conn, piece); err != nil {
						break
					}
				}
			}
			if err != nil {
				return err
			}
		}
	}
}

func (s *Server) text(ctx context.Context, conn *websocket.Conn, msg []byte) error {
	ctx, cancel := context.WithTimeout(ctx, s.write)
	defer cancel()
	err := conn.Write(ctx, websocket.MessageText, msg)
	if err == nil && s.sent != nil {
		s.sent(len(msg))
	}
	return err
}

func (s *Server) binary(ctx context.Context, conn *websocket.Conn, msg []byte) error {
	ctx, cancel := context.WithTimeout(ctx, s.write)
	defer cancel()
	err := conn.Write(ctx, websocket.MessageBinary, msg)
	if err == nil && s.sent != nil {
		s.sent(len(msg))
	}
	return err
}

func trackMessage(t Track) []byte {
	b, _ := json.Marshal(struct {
		Type        string `json:"type"`
		Codec       string `json:"codec"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		Timescale   uint32 `json:"timescale"`
		Description string `json:"description"`
	}{"video", t.Codec, t.Width, t.Height, t.Timescale, base64.StdEncoding.EncodeToString(t.Description)})
	return b
}

func signalMessage(state string) []byte {
	return []byte(`{"type":"signal","state":"` + state + `"}`)
}

// sampleMessages lays a sample out as the wire format says, in messages of at most max bytes.
func sampleMessages(s Sample, max int) [][]byte {
	flags := byte(0)
	if s.Key {
		flags = KeyFlag
	}
	first := min(len(s.Data), max-SampleHeader)
	pieces := make([][]byte, 0, 1+len(s.Data)/max)
	head := make([]byte, SampleHeader+first)
	head[0] = SampleMessage
	head[1] = flags
	if first < len(s.Data) {
		head[1] |= MoreFlag
	}
	binary.BigEndian.PutUint64(head[2:], uint64(s.Wall.UnixMilli()))
	binary.BigEndian.PutUint64(head[10:], uint64(s.PTS.Microseconds()))
	copy(head[SampleHeader:], s.Data[:first])
	pieces = append(pieces, head)
	for rest := s.Data[first:]; len(rest) > 0; {
		n := min(len(rest), max-ContinuationHeader)
		piece := make([]byte, ContinuationHeader+n)
		piece[0] = ContinuationMessage
		if n < len(rest) {
			piece[1] = MoreFlag
		}
		copy(piece[ContinuationHeader:], rest[:n])
		pieces = append(pieces, piece)
		rest = rest[n:]
	}
	return pieces
}

func refuse(w http.ResponseWriter, status int, why string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": why})
}
