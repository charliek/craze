// Package humanize formats numbers for people: durations today, more to come.
package humanize

import (
	"fmt"
	"time"
)

// Duration formats d with its two largest units, e.g. "1h 5m", "2m 3s", "45s",
// "850ms". A negative duration gets a leading minus.
func Duration(d time.Duration) string {
	if d < 0 {
		return "-" + Duration(-d)
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		s := int((d - time.Duration(m)*time.Minute).Seconds())
		return twoUnits(m, "m", s, "s")
	default:
		h := int(d.Hours())
		m := int((d - time.Duration(h)*time.Hour).Minutes())
		return twoUnits(h, "h", m, "m")
	}
}

func twoUnits(a int, au string, b int, bu string) string {
	if b == 0 {
		return fmt.Sprintf("%d%s", a, au)
	}
	return fmt.Sprintf("%d%s %d%s", a, au, b, bu)
}
