package rtc_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hyz-is/arandu-rtc"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func TestAPassOpensItsCameraAndNoOther(t *testing.T) {
	s, err := rtc.NewSigner(secret, 0)
	if err != nil {
		t.Fatal(err)
	}
	pass, expires := s.Issue("cam-1", "account-9")
	if d := time.Until(expires); d <= 50*time.Second || d > rtc.DefaultPassTTL {
		t.Errorf("a pass issued now expires in %s, want about %s", d, rtc.DefaultPassTTL)
	}
	claims, err := s.Verify(pass, "cam-1")
	if err != nil {
		t.Fatal(err)
	}
	if claims.Camera != "cam-1" || claims.Subject != "account-9" || !claims.Expires.Equal(expires) {
		t.Errorf("claims = %+v", claims)
	}
	if _, err := s.Verify(pass, "cam-2"); !errors.Is(err, rtc.ErrPass) {
		t.Errorf("another camera: error %v, want ErrPass", err)
	}
}

func TestAPassIsRefusedWhenItIsNotTheSignersOwn(t *testing.T) {
	a, _ := rtc.NewSigner(secret, 0)
	b, _ := rtc.NewSigner([]byte("another secret, of the same length."), 0)
	pass, _ := a.Issue("cam-1", "x")

	tampered := []string{
		"",
		"x",
		pass + "a",
		pass[:len(pass)-2],
		strings.Replace(pass, ".", ".A", 1),
		"." + pass,
		strings.Repeat(pass, 2),
	}
	for _, p := range tampered {
		if _, err := a.Verify(p, "cam-1"); !errors.Is(err, rtc.ErrPass) {
			t.Errorf("%q: error %v, want ErrPass", p, err)
		}
	}
	if _, err := b.Verify(pass, "cam-1"); !errors.Is(err, rtc.ErrPass) {
		t.Errorf("another secret: error %v, want ErrPass", err)
	}
	// The payload changed under the same tag.
	body, tag, _ := strings.Cut(pass, ".")
	forged, _ := a.Issue("cam-2", "x")
	forgedBody, _, _ := strings.Cut(forged, ".")
	if _, err := a.Verify(forgedBody+"."+tag, "cam-2"); !errors.Is(err, rtc.ErrPass) || body == forgedBody {
		t.Errorf("a payload with another pass's tag: error %v, want ErrPass", err)
	}
}

func TestAPassExpires(t *testing.T) {
	s, _ := rtc.NewSigner(secret, time.Second)
	pass, _ := s.Issue("cam-1", "x")
	if _, err := s.Verify(pass, "cam-1"); err != nil {
		t.Fatalf("a fresh pass: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := s.Verify(pass, "cam-1"); !errors.Is(err, rtc.ErrPass) {
		t.Errorf("an old pass: error %v, want ErrPass", err)
	}
}

func TestASignerWantsASecretAndASaneLifetime(t *testing.T) {
	if _, err := rtc.NewSigner([]byte("short"), 0); err == nil {
		t.Error("a short secret was accepted")
	}
	if _, err := rtc.NewSigner(secret, -time.Second); err == nil {
		t.Error("a negative lifetime was accepted")
	}
	if _, err := rtc.NewSigner(secret, time.Hour); err == nil {
		t.Error("a pass of an hour was accepted: that is a session")
	}
}

func TestASnapshotPassOpensTheStillAndNotTheVideo(t *testing.T) {
	s, _ := rtc.NewSigner(secret, 0)
	snap, _ := s.IssueKind(rtc.KindSnapshot, "cam-1", "account-9")
	video, _ := s.Issue("cam-1", "account-9")

	claims, err := s.VerifyKind(snap, "cam-1", rtc.KindSnapshot)
	if err != nil || claims.Kind != rtc.KindSnapshot || claims.Subject != "account-9" {
		t.Fatalf("snapshot pass: %+v, %v", claims, err)
	}
	if _, err := s.VerifyKind(snap, "cam-2", rtc.KindSnapshot); !errors.Is(err, rtc.ErrPass) {
		t.Errorf("another camera: error %v, want ErrPass", err)
	}
	if _, err := s.Verify(snap, "cam-1"); !errors.Is(err, rtc.ErrPass) {
		t.Errorf("a snapshot pass opened the video: error %v, want ErrPass", err)
	}
	if _, err := s.VerifyKind(video, "cam-1", rtc.KindSnapshot); !errors.Is(err, rtc.ErrPass) {
		t.Errorf("a video pass opened the still: error %v, want ErrPass", err)
	}
	if c, err := s.Verify(video, "cam-1"); err != nil || c.Kind != "" {
		t.Errorf("video pass: %+v, %v", c, err)
	}
	// The kind is inside what the tag covers: it cannot be added to, or taken off, a pass.
	body, tag, _ := strings.Cut(snap, ".")
	vbody, _, _ := strings.Cut(video, ".")
	if _, err := s.Verify(vbody+"."+tag, "cam-1"); !errors.Is(err, rtc.ErrPass) || body == vbody {
		t.Errorf("a video payload with a snapshot tag: error %v, want ErrPass", err)
	}
}

func TestATilePassOpensTheTileAndNotTheVideoOrTheStill(t *testing.T) {
	s, _ := rtc.NewSigner(secret, 0)
	tile, _ := s.IssueKind(rtc.KindTile, "cam-1", "account-9")
	video, _ := s.Issue("cam-1", "account-9")
	snap, _ := s.IssueKind(rtc.KindSnapshot, "cam-1", "account-9")

	claims, err := s.VerifyKind(tile, "cam-1", rtc.KindTile)
	if err != nil || claims.Kind != rtc.KindTile || claims.Subject != "account-9" {
		t.Fatalf("tile pass: %+v, %v", claims, err)
	}
	if _, err := s.VerifyKind(tile, "cam-2", rtc.KindTile); !errors.Is(err, rtc.ErrPass) {
		t.Errorf("another camera: error %v, want ErrPass", err)
	}
	if _, err := s.Verify(tile, "cam-1"); !errors.Is(err, rtc.ErrPass) {
		t.Errorf("a tile pass opened the video: error %v, want ErrPass", err)
	}
	if _, err := s.VerifyKind(video, "cam-1", rtc.KindTile); !errors.Is(err, rtc.ErrPass) {
		t.Errorf("a video pass opened the tile: error %v, want ErrPass", err)
	}
	if _, err := s.VerifyKind(snap, "cam-1", rtc.KindTile); !errors.Is(err, rtc.ErrPass) {
		t.Errorf("a snapshot pass opened the tile: error %v, want ErrPass", err)
	}
	if _, err := s.VerifyKind(tile, "cam-1", rtc.KindSnapshot); !errors.Is(err, rtc.ErrPass) {
		t.Errorf("a tile pass opened the still: error %v, want ErrPass", err)
	}
}

// A node that predates kinds reads version 1 only and, finding no kind in its payload struct, would
// take a pass that carries one for the live video's. A pass with a kind is version 2 so that it
// does not.
func TestAPassWithAKindIsVersionTwoAndOneWithoutIsVersionOne(t *testing.T) {
	s, _ := rtc.NewSigner(secret, 0)
	version := func(pass string) (v int, kind string) {
		body, _, _ := strings.Cut(pass, ".")
		raw, err := base64.RawURLEncoding.DecodeString(body)
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			V int    `json:"v"`
			K string `json:"k"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		return p.V, p.K
	}
	video, _ := s.Issue("cam-1", "x")
	tile, _ := s.IssueKind(rtc.KindTile, "cam-1", "x")
	if v, k := version(video); v != 1 || k != "" {
		t.Errorf("video pass is version %d kind %q", v, k)
	}
	if v, k := version(tile); v != 2 || k != rtc.KindTile {
		t.Errorf("tile pass is version %d kind %q", v, k)
	}

	// Neither can be turned into the other: the tag covers the payload.
	forge := func(v int, k string) string {
		raw, _ := json.Marshal(map[string]any{"v": v, "c": "cam-1", "s": "x", "e": time.Now().Add(time.Minute).Unix(), "k": k})
		return base64.RawURLEncoding.EncodeToString(raw) + ".AAAA"
	}
	for _, p := range []string{forge(2, ""), forge(1, rtc.KindTile)} {
		if _, err := s.Verify(p, "cam-1"); !errors.Is(err, rtc.ErrPass) {
			t.Errorf("%s: error %v, want ErrPass", p, err)
		}
	}
}
