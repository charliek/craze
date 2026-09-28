package quota

import (
	"fmt"
	"strconv"
)

// Limits are the rate limits a Bucket enforces.
type Limits struct {
	PerMinute int // tokens added back per minute
	Burst     int // the most tokens a bucket holds
}

// Defaults are the limits New uses when no option overrides them. Configure
// changes them for the whole process, once, at start-up.
var Defaults = Limits{PerMinute: 60, Burst: 10}

// Configure applies the "quota" section of the service's configuration to
// Defaults. Unknown keys are an error; values must be positive integers.
func Configure(section map[string]string) error {
	next := Defaults
	for k, v := range section {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("quota: %s must be a positive integer, got %q", k, v)
		}
		switch k {
		case "per_minute":
			next.PerMinute = n
		case "burst":
			next.Burst = n
		default:
			return fmt.Errorf("quota: unknown key %q", k)
		}
	}
	Defaults = next
	return nil
}
