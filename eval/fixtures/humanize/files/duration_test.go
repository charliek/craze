package humanize

import (
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{850 * time.Millisecond, "850ms"},
		{45 * time.Second, "45s"},
		{2*time.Minute + 3*time.Second, "2m 3s"},
		{5 * time.Minute, "5m"},
		{time.Hour + 5*time.Minute, "1h 5m"},
		{26 * time.Hour, "26h"},
		{-90 * time.Second, "-1m 30s"},
	}
	for _, c := range cases {
		if got := Duration(c.in); got != c.want {
			t.Errorf("Duration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
