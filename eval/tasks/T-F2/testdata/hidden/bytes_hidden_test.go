package humanize_test

import (
	"math"
	"testing"

	"example.com/humanize"
)

func TestHiddenBytesBelowOneKiB(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{999, "999 B"},
		{1023, "1023 B"},
	} {
		if got := humanize.Bytes(c.in); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHiddenBytesBinaryUnits(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{1024, "1 KiB"},
		{1536, "1.5 KiB"},
		{1127, "1.1 KiB"},
		{10 * 1024, "10 KiB"},
		{1 << 20, "1 MiB"},
		{5*(1<<20) + 3*(1<<20)/10 + 1, "5.3 MiB"},
		{1 << 30, "1 GiB"},
		{1 << 40, "1 TiB"},
		{1 << 50, "1 PiB"},
		{1 << 60, "1 EiB"},
	} {
		if got := humanize.Bytes(c.in); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHiddenBytesRoundingUpChangesUnit(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{1048575, "1 MiB"},      // 1023.999 KiB rounds to 1024 KiB
		{1048525, "1 MiB"},      // 1023.9502 KiB rounds to 1024 KiB
		{1048524, "1023.9 KiB"}, // 1023.9492 KiB
		{math.MaxInt64, "8 EiB"},
	} {
		if got := humanize.Bytes(c.in); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHiddenBytesNegative(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{-1, "-1 B"},
		{-1536, "-1.5 KiB"},
		{-(1 << 30), "-1 GiB"},
		{math.MinInt64, "-8 EiB"}, // -n overflows int64: an implementation must not negate it naively
	} {
		if got := humanize.Bytes(c.in); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
