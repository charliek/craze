package quota

import "testing"

func TestConfigureOverrides(t *testing.T) {
	if err := Configure(map[string]string{"burst": "3", "per_minute": "30"}); err != nil {
		t.Fatal(err)
	}
	if Defaults.Burst != 3 || Defaults.PerMinute != 30 {
		t.Fatalf("Defaults = %+v, want burst 3 and 30/min", Defaults)
	}
	if got := New().Limits(); got != Defaults {
		t.Fatalf("New().Limits() = %+v, want %+v", got, Defaults)
	}
}

func TestConfigureRejectsBadValues(t *testing.T) {
	for _, section := range []map[string]string{
		{"burst": "0"},
		{"burst": "many"},
		{"refill": "5"},
	} {
		before := Defaults
		if err := Configure(section); err == nil {
			t.Errorf("Configure(%v) succeeded, want an error", section)
		}
		if Defaults != before {
			t.Errorf("a failed Configure(%v) changed Defaults", section)
		}
	}
}
