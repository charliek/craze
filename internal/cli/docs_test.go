package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// docsModuleRoot finds go.mod upward from the test's working directory.
func docsModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// docHeading is one markdown heading of docs/reference/cli.md and the text of
// its section: from the heading to the next heading of the same or a higher
// level, so a section holds its own subsections.
type docHeading struct {
	level int
	text  string // backticks removed
	body  string
}

var docHeadingRE = regexp.MustCompile("^(#{1,6}) +(.*?) *$")

func parseDocHeadings(doc string) []docHeading {
	lines := strings.Split(doc, "\n")
	var heads []docHeading
	var starts []int
	inFence := false
	for i, l := range lines {
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		m := docHeadingRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		heads = append(heads, docHeading{level: len(m[1]), text: strings.ReplaceAll(m[2], "`", "")})
		starts = append(starts, i)
	}
	for i := range heads {
		end := len(lines)
		for j := i + 1; j < len(heads); j++ {
			if heads[j].level <= heads[i].level {
				end = starts[j]
				break
			}
		}
		heads[i].body = strings.Join(lines[starts[i]:end], "\n")
	}
	return heads
}

// findCommandSection is the heading naming path. An exact heading wins, so
// `craze new` selects its own section and not `craze new --wait`'s. Else a
// heading that starts with path at a word boundary and continues with a flag
// (`craze new --wait`), a usage bracket, or a description after a dash or
// colon (`craze send — Send a prompt`). A heading that merely continues with
// another word (`craze auth login` for `craze auth`) does not count.
func findCommandSection(heads []docHeading, path string) (docHeading, bool) {
	for _, h := range heads {
		if h.text == path {
			return h, true
		}
	}
	for _, h := range heads {
		rest, ok := strings.CutPrefix(h.text, path+" ")
		if !ok {
			continue
		}
		rest = strings.TrimLeft(rest, " ")
		for _, p := range []string{"-", "[", "—", "–", ":"} {
			if strings.HasPrefix(rest, p) {
				return h, true
			}
		}
	}
	return docHeading{}, false
}

// mentionsFlag reports whether text names --name as a whole token: the
// character after the name must not continue a flag name, so --wait is not
// satisfied by --wait-timeout.
func mentionsFlag(text, name string) bool {
	needle := "--" + name
	for i := 0; ; {
		j := strings.Index(text[i:], needle)
		if j < 0 {
			return false
		}
		end := i + j + len(needle)
		if end == len(text) || !isFlagNameChar(text[end]) {
			return true
		}
		i = end
	}
}

func isFlagNameChar(c byte) bool {
	return c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func docVisibleCommands(cmd *cobra.Command, out *[]*cobra.Command) {
	for _, c := range cmd.Commands() {
		// help and completion are cobra's own, generated at execution; their
		// documentation is a hand-written section, not checked here.
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		*out = append(*out, c)
		docVisibleCommands(c, out)
	}
}

// TestCLIReferenceDocumentsEveryCommandAndFlag keeps docs/reference/cli.md
// current: every visible command has a heading, and every visible,
// non-deprecated flag of it is named in that command's section.
func TestCLIReferenceDocumentsEveryCommandAndFlag(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(docsModuleRoot(t), "docs", "reference", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	heads := parseDocHeadings(string(raw))

	checkFlags := func(c *cobra.Command, where string, section docHeading) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden || f.Deprecated != "" || f.Name == "help" {
				return
			}
			if !mentionsFlag(section.body, f.Name) {
				t.Errorf("docs/reference/cli.md: flag --%s of %q is not in %s", f.Name, c.CommandPath(), where)
			}
		})
	}

	root := NewRootCmd()
	flagsSection, ok := findCommandSection(heads, "Flags")
	if !ok {
		t.Fatal(`docs/reference/cli.md has no "Flags" heading for the root command's flags`)
	}
	checkFlags(root, `the "## Flags" section`, flagsSection)

	var cmds []*cobra.Command
	docVisibleCommands(root, &cmds)
	for _, c := range cmds {
		path := c.CommandPath()
		sec, ok := findCommandSection(heads, path)
		if !ok {
			t.Errorf("docs/reference/cli.md: no heading names command %q (want e.g. \"## %s\")", path, path)
			continue
		}
		checkFlags(c, `the section headed "`+sec.text+`"`, sec)
	}
}
