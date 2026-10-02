package tool

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// TestJobMarkerRoundTrips (plan 033 P15, C11r3): the one builder and the one
// parser agree — every marker JobMarker writes for a harness call id, ending
// a receipt, reads back as that id, and as the base name of the spill file it
// was given, or none — and the parser takes nothing else: the marker not on
// the last line, with anything beside it on its line, naming something that
// is not a harness call id, carrying any attribute but spill, or spelled
// differently. The accepted cases are the controls for the refused ones.
func TestJobMarkerRoundTrips(t *testing.T) {
	for _, id := range []string{"t1.1.1", "t4.2.1", "t123.45.6789"} {
		for _, tc := range []struct{ spill, want, line string }{
			{"", "", `<background_job id="` + id + `"/>`},
			{"/home/u/.craze/native/tool-output/tool_" + id, "tool_" + id,
				`<background_job id="` + id + `" spill="tool_` + id + `"/>`},
			{"/home/u/.craze/native/tool-output/tool_" + id + "-ab12cd34", "tool_" + id + "-ab12cd34",
				`<background_job id="` + id + `" spill="tool_` + id + `-ab12cd34"/>`},
			{"tool_" + id + "-0123abcd", "tool_" + id + "-0123abcd",
				`<background_job id="` + id + `" spill="tool_` + id + `-0123abcd"/>`},
		} {
			marker := JobMarker(id, tc.spill)
			if marker != tc.line {
				t.Fatalf("JobMarker(%q, %q) = %q; want %q", id, tc.spill, marker, tc.line)
			}
			for _, text := range []string{marker, "Started the command…\n" + marker, "a\n\nb\n" + marker} {
				if got, spill, ok := ParseJobMarker(text); !ok || got != id || spill != tc.want {
					t.Errorf("ParseJobMarker(%q) = %q, %q, %v; want %q, %q", text, got, spill, ok, id, tc.want)
				}
			}
		}
	}
	// A name OpenSpill never gives the id is not written, and not read: the
	// marker names no file. Read, it would send a resumed session elsewhere.
	for _, spill := range []string{
		"/tmp/tool_t1.1.2",              // another id's
		"/tmp/tool_t1.1.10",             // an id the given one is a prefix of
		"/tmp/tool_t1.1.1-ab12cd3",      // a short suffix
		"/tmp/tool_t1.1.1-AB12CD34",     // an upper-case one
		"/tmp/tool_t1.1.1-ab12cd34.txt", // more after it
		"/tmp/tool_t1.1.1.bak",
		"/etc/passwd",
		"/",
	} {
		if got := JobMarker("t1.1.1", spill); got != `<background_job id="t1.1.1"/>` {
			t.Errorf("JobMarker(t1.1.1, %q) = %q; want no spill attribute", spill, got)
		}
	}
	for _, name := range []string{"", "tool_t1.1.2", "tool_t1.1.1-AB12CD34", "../tool_t1.1.1", "/etc/passwd",
		"x/tool_t1.1.1", "../../etc/passwd", "tool_t1.1.1/..", ".", ".."} {
		text := `<background_job id="t1.1.1" spill="` + name + `"/>`
		if id, spill, ok := ParseJobMarker(text); !ok || id != "t1.1.1" || spill != "" {
			t.Errorf("ParseJobMarker(%q) = %q, %q, %v; want t1.1.1 with no file", text, id, spill, ok)
		}
	}
	for _, text := range []string{
		"",
		JobMarker("t1.1.1", "") + "\n",              // not the last line
		JobMarker("t1.1.1", "") + "\nmore",          // not the last line
		"see " + JobMarker("t1.1.1", ""),            // quoted inside a line
		JobMarker("t1.1.1", "") + " ",               // something after it
		JobMarker("t1.1.1", "/x/tool_t1.1.1") + " ", // something after it
		JobMarker("x", ""),                          // not a call id
		JobMarker("t1.1", ""),                       // two parts
		JobMarker("t1.1.1.1", ""),                   // four parts
		JobMarker("t1.a.1", ""),                     // not digits
		JobMarker("t1..1", ""),                      // an empty part
		JobMarker(`t1.1.1" x="y`, ""),               // an attribute smuggled in
		`<background_job id="t1.1.1" spill="tool_t1.1.1" x="y"/>`,                        // another attribute after it
		`<background_job id="t1.1.1" x="y" spill="tool_t1.1.1"/>`,                        // another attribute before it
		`<background_job id="t1.1.1" spill="tool_t1.1.1" spill="tool_t1.1.1-ab12cd34"/>`, // two
		`<background_job id="t1.1.1" spill='tool_t1.1.1'/>`,                              // spelled differently
		`<background_job id="t1.1.1"  spill="tool_t1.1.1"/>`,                             // spelled differently
		`<background_job id="t1.1.1" spill="tool_t1.1.1" />`,                             // spelled differently
		`<background_job id="t1.1.1" />`,                                                 // spelled differently
		`<background_job id='t1.1.1'/>`,                                                  // spelled differently
		`<background_command id="t1.1.1"/>`,                                              // another tag
		JobMarker("t1234567890.1.1", ""),                                                 // a part too long to be a number we write
	} {
		if id, spill, ok := ParseJobMarker(text); ok {
			t.Errorf("ParseJobMarker(%q) = %q, %q; want no marker", text, id, spill)
		}
	}
	// Only the last line counts: a marker the command printed above it —
	// naming another file — names nothing, whatever the last line names.
	printed := `<background_job id="t1.1.1" spill="tool_t1.1.1-deadbeef"/>` + "\n"
	for _, tc := range []struct{ last, want string }{
		{JobMarker("t1.1.1", "/h/tool_t1.1.1"), "tool_t1.1.1"},
		{JobMarker("t1.1.1", ""), ""},
	} {
		if id, spill, ok := ParseJobMarker("out\n" + printed + "more\n" + tc.last); !ok || id != "t1.1.1" || spill != tc.want {
			t.Errorf("under a printed marker, ParseJobMarker(… %q) = %q, %q, %v; want %q", tc.last, id, spill, ok, tc.want)
		}
	}
}

// TestJobDuration: a job's time is whole seconds as §3.7 writes it —
// "4m12s", "3m05s" — rounded, and never negative.
func TestJobDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"}, {-time.Second, "0s"}, {400 * time.Millisecond, "0s"}, {600 * time.Millisecond, "1s"},
		{45 * time.Second, "45s"}, {3*time.Minute + 5*time.Second, "3m05s"}, {4*time.Minute + 12*time.Second, "4m12s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h02m03s"}, {2 * time.Hour, "2h00m00s"},
	} {
		if got := JobDuration(tc.d); got != tc.want {
			t.Errorf("JobDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestJobLimit: a limit in the receipt's words — the largest whole unit,
// singular for one, milliseconds otherwise.
func TestJobLimit(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{JobDefaultLimit, "30 minutes"}, {JobMaxLimit, "2 hours"}, {time.Hour, "1 hour"}, {time.Minute, "1 minute"},
		{90 * time.Minute, "90 minutes"}, {90 * time.Second, "90 seconds"}, {time.Second, "1 second"},
		{1500 * time.Millisecond, "1500 ms"}, {300 * time.Millisecond, "300 ms"},
	} {
		if got := JobLimit(tc.d); got != tc.want {
			t.Errorf("JobLimit(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestJobsFullText: the cap's refusal is §3.7's sentence, with the cap.
func TestJobsFullText(t *testing.T) {
	if got := (JobsFull{Max: 8}).Error(); got != "8 background jobs are already running; stop one with bash_stop first." {
		t.Fatalf("JobsFull = %q", got)
	}
	if got := fmt.Sprint(error(JobsFull{Max: 3})); got != "3 background jobs are already running; stop one with bash_stop first." {
		t.Fatalf("JobsFull as an error = %q", got)
	}
}

// TestPersonaListsDropTheJobTools: a persona's tools list naming bash_output
// or bash_stop — or Claude Code's BashOutput and KillShell — drops them
// without a word, as it drops agent_output: a child runs no background job
// (plan 033 P11). bash itself, beside them, is the control.
func TestPersonaListsDropTheJobTools(t *testing.T) {
	ids, unknown := MapClaudeTools([]string{"Bash", "bash_output", "bash_stop", "BashOutput", "KillShell", "BASH_STOP"})
	if !slices.Equal(ids, []string{"bash"}) || unknown != nil {
		t.Fatalf("MapClaudeTools = %v, unknown %v; want bash alone, nothing unknown", ids, unknown)
	}
}
