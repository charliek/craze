package chatgptauth

import (
	"encoding/json"
	"testing"
	"time"
)

// TestTokenReplyExpiresIn: a reply's expires_in counts only as a lifetime of
// more than nothing and at most maxExpiresIn — a value past int64's range in
// nanoseconds (1e19 seconds) included, which a check after the conversion to
// time.Duration took for a negative lifetime and passed (CodeRabbit on #82).
func TestTokenReplyExpiresIn(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"3600":  time.Hour,
		"0.5":   500 * time.Millisecond,
		"86400": maxExpiresIn,
	} {
		if got, ok := (&tokenReply{ExpiresIn: json.Number(in)}).expiresIn(); !ok || got != want {
			t.Errorf("expires_in %s = %v, %v; want %v, true", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "0", "-1", "86400.5", "86401", "1e19", "-1e19", "1e400", "NaN", "Inf", "an hour"} {
		if got, ok := (&tokenReply{ExpiresIn: json.Number(in)}).expiresIn(); ok {
			t.Errorf("expires_in %q = %v, ok: want it refused", in, got)
		}
	}
}
