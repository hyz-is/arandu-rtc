package rtc

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// A pass is how a page that already knows who the viewer is lets the browser open one camera on a
// node it does not trust the page's session with.
//
// The application decides who may watch which camera, with its own Policy, and then calls Issue.
// The node, which shares nothing with that application but a secret, calls Verify. Nothing is
// looked up and nothing is stored: the pass says which camera, for whom, until when, and the
// HMAC says the application wrote it.
//
// It is short on purpose. It is checked once, when the socket opens, so it only has to outlive
// the round trip from the page to the browser to the node; a stolen one is good for one camera for
// about a minute. It is not a session: the socket the pass opens is closed by the server after
// ServerConfig.MaxSession, and the page asks for a new pass to continue.

const (
	// MinSecret is the shortest secret a Signer takes: the HMAC key is the whole defence.
	MinSecret = 32

	// DefaultPassTTL is how long a pass opens a socket.
	DefaultPassTTL = 60 * time.Second

	// MaxPassTTL is the longest a Signer will issue for: a pass that lasts is a session.
	MaxPassTTL = 10 * time.Minute

	// passVersion is the pass of the live video, which carries no kind. A pass with a kind is
	// passKindVersion: a node that predates kinds reads only version 1, so it refuses a still
	// picture's or a tile's pass instead of taking it for the live video's.
	passVersion     = 1
	passKindVersion = 2
	passDomain      = "arandu-rtc/pass/v1\n"
)

// KindSnapshot marks a pass that opens a camera's still picture and nothing else. A pass without a
// kind is the live video's, and Verify refuses a snapshot pass, as VerifyKind refuses a video one:
// the still picture is fetched by a page that may do it for dozens of cameras at once, and a pass
// for it must not be good for a socket that carries the camera's whole stream.
const KindSnapshot = "snapshot"

// KindTile marks a pass that opens a camera's small picture: the node serves a sub-stream, or the
// main picture reduced, at most 640 by 360 at 6 pictures a second, from a hub of its own. A page that
// shows a grid of cameras holds one for each, and it must not open the socket of the camera's whole
// stream, as the live video's pass must not open the tile.
const KindTile = "tile"

// reserved says a kind has a check of its own: Verify, the live video's, refuses it.
func reserved(kind string) bool { return kind == KindSnapshot || kind == KindTile }

// ErrPass is every reason a pass is refused. The caller gets one answer on purpose: which part was
// wrong is a hint to somebody guessing.
var ErrPass = errors.New("live: the pass is not valid")

// Claims is what a verified pass says.
type Claims struct {
	// Camera is the only camera the pass opens.
	Camera string
	// Subject is who the application issued it to, such as an account id. The node does not
	// interpret it; it is there for the node's log.
	Subject string
	// Expires is the last instant the pass opens a socket.
	Expires time.Time
	// Kind is what the pass opens: empty for the live video, or a mark such as KindSnapshot.
	Kind string
}

// Signer issues and verifies passes with one shared secret.
type Signer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewSigner returns a Signer for secret. A zero ttl takes DefaultPassTTL.
func NewSigner(secret []byte, ttl time.Duration) (*Signer, error) {
	if len(secret) < MinSecret {
		return nil, fmt.Errorf("live: the pass secret has %d bytes, and %d is the least", len(secret), MinSecret)
	}
	if ttl == 0 {
		ttl = DefaultPassTTL
	}
	if ttl < 0 || ttl > MaxPassTTL {
		return nil, fmt.Errorf("live: a pass lasting %s is not between 0 and %s", ttl, MaxPassTTL)
	}
	return &Signer{secret: append([]byte(nil), secret...), ttl: ttl, now: time.Now}, nil
}

// TTL is how long an issued pass is good for.
func (s *Signer) TTL() time.Duration { return s.ttl }

type passPayload struct {
	V int    `json:"v"`
	C string `json:"c"`
	S string `json:"s"`
	E int64  `json:"e"`
	// K is the kind, left out of a live video pass so that a pass written before kinds existed
	// and one written now are the same bytes.
	K string `json:"k,omitempty"`
}

// Issue writes a pass for camera, for subject, and says when it stops working.
func (s *Signer) Issue(camera, subject string) (string, time.Time) {
	return s.IssueKind("", camera, subject)
}

// IssueKind writes a pass of a kind, such as KindSnapshot or KindTile. An empty kind is Issue's.
func (s *Signer) IssueKind(kind, camera, subject string) (string, time.Time) {
	expires := s.now().Add(s.ttl).Truncate(time.Second)
	version := passVersion
	if kind != "" {
		version = passKindVersion
	}
	body, _ := json.Marshal(passPayload{V: version, C: camera, S: subject, E: expires.Unix(), K: kind})
	encoded := base64.RawURLEncoding.EncodeToString(body)
	return encoded + "." + base64.RawURLEncoding.EncodeToString(s.mac(encoded)), expires
}

// Verify reads a pass and checks it was issued by a Signer with this secret, for this camera,
// and has not expired. Every failure is ErrPass. It is the live video's check: a pass of kind
// KindSnapshot or KindTile is refused, and any other mark is the application's to define and is
// accepted.
func (s *Signer) Verify(pass, camera string) (Claims, error) {
	claims, err := s.verify(pass, camera)
	if err != nil || reserved(claims.Kind) {
		return Claims{}, ErrPass
	}
	return claims, nil
}

// VerifyKind is Verify for a pass of exactly this kind: a pass of any other kind, the live video's
// included, is refused.
func (s *Signer) VerifyKind(pass, camera, kind string) (Claims, error) {
	claims, err := s.verify(pass, camera)
	if err != nil || claims.Kind != kind {
		return Claims{}, ErrPass
	}
	return claims, nil
}

func (s *Signer) verify(pass, camera string) (Claims, error) {
	encoded, tag, found := cutLast(pass, '.')
	if !found {
		return Claims{}, ErrPass
	}
	given, err := base64.RawURLEncoding.DecodeString(tag)
	if err != nil || !hmac.Equal(given, s.mac(encoded)) {
		return Claims{}, ErrPass
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Claims{}, ErrPass
	}
	var p passPayload
	if err := json.Unmarshal(body, &p); err != nil || p.C == "" || p.C != camera {
		return Claims{}, ErrPass
	}
	// A pass without a kind is version 1 and a pass with one is version 2, never the other way.
	if (p.V != passVersion || p.K != "") && (p.V != passKindVersion || p.K == "") {
		return Claims{}, ErrPass
	}
	expires := time.Unix(p.E, 0)
	if !s.now().Before(expires) {
		return Claims{}, ErrPass
	}
	return Claims{Camera: p.C, Subject: p.S, Expires: expires, Kind: p.K}, nil
}

func (s *Signer) mac(encoded string) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(passDomain))
	m.Write([]byte(encoded))
	return m.Sum(nil)
}

func cutLast(s string, sep byte) (before, after string, found bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
