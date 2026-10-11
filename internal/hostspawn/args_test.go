package hostspawn

import (
	"slices"
	"strings"
	"testing"
)

// TestArgsPassTheStartSettingsOnlyWhenSet: --effort and --fast/--no-fast
// (plan 032 §3.11) reach craze serve's command line only when the spawner set
// them — "" and nil leave the host the provider's own defaults — and an effort
// that looks like a flag stays a value. internal/cli's
// TestSpawnArgvCarriesEverySessionFlag parses the result with serve's own
// flag set.
func TestArgsPassTheStartSettingsOnlyWhenSet(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name     string
		spec     Spec
		want     []string
		dontWant []string
	}{
		{"neither", Spec{}, nil, []string{"--effort=", "--fast", "--no-fast"}},
		{"effort and fast", Spec{Effort: "-high", Fast: &on}, []string{"--effort=-high", "--fast"}, []string{"--no-fast"}},
		{"no-fast", Spec{Fast: &off}, []string{"--no-fast"}, []string{"--effort=", "--fast"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := Args(tc.spec)
			for _, a := range tc.want {
				if !slices.Contains(argv, a) {
					t.Fatalf("%q lacks %s", argv, a)
				}
			}
			for _, a := range tc.dontWant {
				if slices.ContainsFunc(argv, func(x string) bool { return x == a || strings.HasSuffix(a, "=") && strings.HasPrefix(x, a) }) {
					t.Fatalf("%q passes %s", argv, a)
				}
			}
		})
	}
}

// TestArgsPassTheRequestOnlyWhenSet: a hub's create (plan 032 §3.10) hands
// its host --request-id and --request-hash; every other spawn passes neither.
func TestArgsPassTheRequestOnlyWhenSet(t *testing.T) {
	argv := Args(Spec{HostID: "0123456789ab", Log: "/l", RequestID: "-r1", RequestHash: "abc"})
	for _, a := range []string{"--request-id=-r1", "--request-hash=abc"} {
		if !slices.Contains(argv, a) {
			t.Fatalf("%q lacks %s", argv, a)
		}
	}
	for _, a := range Args(Spec{HostID: "0123456789ab", Log: "/l"}) {
		if strings.HasPrefix(a, "--request-") {
			t.Fatalf("a spawn with no request passes %s", a)
		}
	}
}

// TestArgsPassTheHubPIDOnlyWhenSet (plan 035 P3): a hub's create hands its
// host --hub-pid, its own pid, for the macOS login-session hint to name;
// every other spawn — a TUI's launch, the list's — passes none.
func TestArgsPassTheHubPIDOnlyWhenSet(t *testing.T) {
	if argv := Args(Spec{HostID: "0123456789ab", Log: "/l", HubPID: 4242}); !slices.Contains(argv, "--hub-pid=4242") {
		t.Fatalf("%q lacks --hub-pid=4242", argv)
	}
	for _, a := range Args(Spec{HostID: "0123456789ab", Log: "/l"}) {
		if strings.HasPrefix(a, "--hub-pid") {
			t.Fatalf("a spawn with no hub passes %s", a)
		}
	}
}

// TestArgsPassUnattendedOnlyWhenSet (plan 041 §3.2): craze new --wait's and
// craze send's spawns hand their host --unattended; every other spawn — a
// TUI's launch, the list's, the hub's create — passes none.
func TestArgsPassUnattendedOnlyWhenSet(t *testing.T) {
	if argv := Args(Spec{HostID: "0123456789ab", Log: "/l", Unattended: true}); !slices.Contains(argv, "--unattended") {
		t.Fatalf("%q lacks --unattended", argv)
	}
	if argv := Args(Spec{HostID: "0123456789ab", Log: "/l", NoHostStatus: true}); slices.Contains(argv, "--unattended") {
		t.Fatalf("an attended spawn passes --unattended: %q", argv)
	}
}

// TestArgsPassOutliveSpawnerOnlyWhenSet (plan 041 C3 review r2, 1): craze
// send's reload hands its host --outlive-spawner; every other spawn passes
// none.
func TestArgsPassOutliveSpawnerOnlyWhenSet(t *testing.T) {
	if argv := Args(Spec{HostID: "0123456789ab", Log: "/l", OutliveSpawner: true}); !slices.Contains(argv, "--outlive-spawner") {
		t.Fatalf("%q lacks --outlive-spawner", argv)
	}
	if argv := Args(Spec{HostID: "0123456789ab", Log: "/l", Unattended: true}); slices.Contains(argv, "--outlive-spawner") {
		t.Fatalf("a spawn not for a reload passes --outlive-spawner: %q", argv)
	}
}

// TestArgsPassTheTitleOnlyWhenSet (plan 043 D8): a titled create's spawn —
// the hub's, craze new --wait's — hands its host --title, spelled
// --flag=value so a title that looks like a flag stays a value; every other
// spawn passes none.
func TestArgsPassTheTitleOnlyWhenSet(t *testing.T) {
	if argv := Args(Spec{HostID: "0123456789ab", Log: "/l", Title: "-fix the build"}); !slices.Contains(argv, "--title=-fix the build") {
		t.Fatalf("%q lacks --title=-fix the build", argv)
	}
	for _, a := range Args(Spec{HostID: "0123456789ab", Log: "/l", RequestID: "r", RequestHash: "h"}) {
		if strings.HasPrefix(a, "--title") {
			t.Fatalf("an untitled spawn passes %s", a)
		}
	}
}
