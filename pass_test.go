package rtc_test

import (
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
