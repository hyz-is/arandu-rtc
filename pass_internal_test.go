package rtc

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// signed is a pass the Signer would never write, with a valid tag: the version and the kind are
// what is under test, not the HMAC.
func signed(s *Signer, version int, kind string) string {
	body, _ := json.Marshal(passPayload{V: version, C: "cam", S: "x", E: time.Now().Add(time.Minute).Unix(), K: kind})
	encoded := base64.RawURLEncoding.EncodeToString(body)
	return encoded + "." + base64.RawURLEncoding.EncodeToString(s.mac(encoded))
}

func TestAKindAndAVersionGoTogether(t *testing.T) {
	s, _ := NewSigner([]byte("0123456789abcdef0123456789abcdef"), 0)
	for _, tc := range []struct {
		version int
		kind    string
		ok      bool
	}{
		{1, "", true},
		{2, KindTile, true},
		{2, KindSnapshot, true},
		{1, KindTile, false}, // a kind under the version a node without kinds reads
		{2, "", false},       // a kindless pass is version 1
		{3, KindTile, false}, // a version nobody writes
		{0, "", false},
	} {
		_, err := s.VerifyKind(signed(s, tc.version, tc.kind), "cam", tc.kind)
		if (err == nil) != tc.ok {
			t.Errorf("version %d kind %q: error %v, want ok=%v", tc.version, tc.kind, err, tc.ok)
		}
		if !tc.ok && !errors.Is(err, ErrPass) {
			t.Errorf("version %d kind %q: error %v is not ErrPass", tc.version, tc.kind, err)
		}
	}
}
