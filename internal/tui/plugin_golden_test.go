package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pluginFixtureDirs is every plugin root the goldens run against, absolute.
// probe-plugin is this plan's own probe plugin, the same tree
// tests/cli/fixtures holds for the Python suite; alpha and beta exist only to
// ship one name between them, so the menu has a collision to qualify.
//
// The paths have to be absolute: the frame runner gives its session a temp
// workspace, and a relative --plugin-dir resolves against the workspace, not
// against the package directory testdata lives in.
func pluginFixtureDirs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, n := range []string{"probe-plugin", "alpha", "beta"} {
		abs, err := filepath.Abs(filepath.Join("testdata", "plugins", n))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, abs)
	}
	return out
}

// TestFrameGoldenPluginMenu is §3.5 through the real program and the real
// wire: the rows craze owns, their plugin labels, the collision pair that can
// only be reached qualified, the qualified-prefix bucket, and the accept that
// keeps the spelling the token asked for.
//
// Every case waits for zulu-tool, the last entry of the fake's catalog: the
// update lands after session/new replies, and until it has been applied every
// plugin row is still provisional (§3.2), so a golden that did not wait would
// be a race between two correct frames.
func TestFrameGoldenPluginMenu(t *testing.T) {
	dirs := pluginFixtureDirs(t)
	for _, tc := range []struct {
		name   string
		keys   string
		want   []string
		absent []string
	}{
		// The end of the list, where craze's own rows are: the fake's 24
		// advertised commands and the ten builtins come first, so five pages
		// down is where the four plugin rows are. probe-echo and probe-skill
		// are unique, so they are offered bare with their plugin as the label;
		// rescue is not, so both halves of it are qualified.
		{"plugin-open", slashCatalogLanded + strings.Repeat("<pgdn>", 5),
			[]string{
				"/probe-echo", "(probe-plugin)", "/probe-skill",
				"/alpha:rescue", "/beta:rescue", "38/38 ▲",
			},
			[]string{"/help", "▼"}},
		// The qualified-prefix bucket: neither displayed name starts with
		// "probe-plugin:", so the only thing matching the query is the
		// spelling craze resolved for them.
		{"plugin-filter-qualified", slashCatalogLanded + "probe-plugin:",
			[]string{"/probe-echo", "/probe-skill"},
			[]string{"/alpha:rescue", "/probe-plugin:probe-echo", "▲", "▼"}},
		// Tab on a token with a colon in it keeps the colon: the row means the
		// same entry either way, and the colon is the user saying which.
		{"plugin-accept-qualified", slashCatalogLanded + "probe-plugin:pr<tab>",
			[]string{"❯ /probe-plugin:probe-echo "},
			[]string{"1/1", "▲", "▼"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runFakeFramePlugins(t, "commands", 100, 30, "<wait:idle>"+tc.keys, dirs...)
			assertFrameGolden(t, tc.name, 100, 30, got, tc.want, tc.absent)
		})
	}
}

// TestFrameGoldenPluginProvisional is the window §3.2 exists for: until the
// agent's first available_commands_update has been applied, every row craze
// owns shows its qualified spelling, so a bare name accepted early cannot
// change meaning when the catalog lands.
//
// The `nocommands` script never advertises one, which is the only way to hold
// that window open for a frame: the real update arrives the instant session/new
// is answered. The token is mid-message so the builtins stay out and the four
// rows are the whole list.
func TestFrameGoldenPluginProvisional(t *testing.T) {
	got := runFakeFramePlugins(t, "nocommands", 100, 30, "<wait:idle>see /", pluginFixtureDirs(t)...)
	assertFrameGolden(t, "plugin-provisional", 100, 30, got,
		[]string{
			"/probe-plugin:probe-echo", "/probe-plugin:probe-skill",
			"/alpha:rescue", "/beta:rescue",
		},
		// The bare spellings are exactly what the provisional window withholds.
		[]string{"/probe-echo", "/probe-skill", "/help"})
}

// TestFrameGoldenPluginSent is the send half of §3.3 in the transcript: the
// draft as the user wrote it, and under it the one dim line craze adds saying
// what it expanded into the prompt — by the qualified spelling, which always
// resolves, and the kind. The body is deliberately nowhere on the frame.
//
// The fake holds the turn so the frame is the moment after the prompt reached
// the wire. That is not only a smaller frame: the block craze sent quotes the
// plugin file's absolute path, which is a temp directory here and a checkout
// path in CI, so an echoed block could never be a golden. The block itself is
// asserted below, and pinned byte for byte by internal/agent's own goldens.
//
// Frozen, because the turn is still running: the spinner and the elapsed
// counters would otherwise make the frame a race against the build.
func TestFrameGoldenPluginSent(t *testing.T) {
	// The `hang` script advertises one command, and waiting for its row is
	// waiting for the catalog: a bare /probe-echo only resolves once the
	// provisional window has closed. Three backspaces leave the bare "/".
	const keys = "<wait:idle>/res<wait:text:Agent-advertised command><backspace><backspace><backspace>" +
		"probe-echo banana<enter><wait:text:⤷ probe-plugin:probe-echo (command)>"
	got := runFakeFrameOpts(t, "hang", 100, 30, keys, fakeFrameOpts{
		force:      true,
		freeze:     true,
		pluginDirs: pluginFixtureDirs(t),
		timeout:    20 * time.Second,
	})
	assertFrameGolden(t, "plugin-sent", 100, 30, got,
		[]string{"❯ /probe-echo banana", "⤷ probe-plugin:probe-echo (command)"},
		// The expanded body must not reach the transcript.
		[]string{"PROBE-COMMAND-EXPANDED"})
}

// TestFramePluginBlockReachesTheAgent is the other half, without a golden: the
// fake joins the text blocks of one session/prompt with a newline, so its echo
// is the draft, a line break, and then the block craze appended. The path in
// that block is machine-specific, so this asserts on what it says rather than
// on the cells it occupies.
func TestFramePluginBlockReachesTheAgent(t *testing.T) {
	got := runFakeFramePlugins(t, "commands", 100, 30,
		"<wait:idle>"+slashCatalogLanded+"<backspace>/probe-echo banana<enter>"+
			"<wait:text:PROBE-COMMAND-EXPANDED args=[banana]><wait:idle>", pluginFixtureDirs(t)...)
	// The transcript wraps the echo as prose, so the assertions are fragments
	// short enough to survive the wrap rather than whole lines of the block.
	assertFrameGolden(t, "", 100, 30, got, []string{
		"❯ /probe-echo banana",
		"⤷ probe-plugin:probe-echo (command)",
		"echo: /probe-echo banana",
		`The user invoked /probe-echo banana`,
		`args="banana">`,
		"PROBE-COMMAND-EXPANDED args=[banana]",
		"</command>",
	}, nil)
}

// TestFrameGoldenHelpPlugins is the help dialog's half of §3.5: plugin rows
// list under "this session's commands" with everything else the session
// offers, labelled by their plugin and with no heading or hint of their own.
// The heading itself is twenty-eight rows above the last of them, so the page
// that holds craze's own rows cannot also hold it; help-bottom-100x30 is the
// golden that pins the heading.
func TestFrameGoldenHelpPlugins(t *testing.T) {
	// Clear the token the catalog wait left behind, open help, and page to the
	// bottom, where the rows craze owns are. PgDn clamps at the end, so the
	// count only has to be enough.
	keys := "<wait:idle>" + slashCatalogLanded + "<backspace>/help<enter>" + strings.Repeat("<pgdn>", 12)
	got := runFakeFramePlugins(t, "commands", 100, 30, keys, pluginFixtureDirs(t)...)
	assertFrameGolden(t, "help-plugins", 100, 30, got,
		[]string{"/probe-echo", "(probe-plugin)", "/probe-skill", "/alpha:rescue", "/beta:rescue"},
		nil)
}
