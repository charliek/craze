package agent

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	maxSkillDepth = 10
	maxSkillFiles = 256
	maxSkillBytes = 1 << 20
)

// Skill is a provider skill the slash picker can offer.
type Skill struct {
	Name        string
	Description string
}

// DiscoverSkills is the provider-owned catalog: a filesystem walk of the
// provider's SkillScan roots under the workspace and home. Skills the agent
// itself advertises arrive over ACP as available_commands_update and are
// merged by the slash catalog, not here.
func DiscoverSkills(p Provider, cwd, home string) []Skill {
	return walkProviderSkills(p.SkillScan(), cwd, home)
}

func walkProviderSkills(scan SkillScan, workspace, home string) []Skill {
	var bases []string
	seenBase := make(map[string]struct{})
	for _, base := range []string{workspace, home} {
		if strings.TrimSpace(base) == "" {
			continue
		}
		abs, err := filepath.Abs(base)
		if err != nil {
			abs = base
		}
		if _, ok := seenBase[abs]; ok {
			continue
		}
		seenBase[abs] = struct{}{}
		bases = append(bases, abs)
	}
	var out []Skill
	seenName := make(map[string]struct{})
	nFiles := 0
	for _, base := range bases {
		for _, rel := range scan.RelRoots {
			if nFiles >= maxSkillFiles {
				return out
			}
			walkSkillRoot(filepath.Join(base, rel), scan, &nFiles, seenName, &out)
		}
	}
	return out
}

func walkSkillRoot(root string, scan SkillScan, nFiles *int, seen map[string]struct{}, out *[]Skill) {
	info, err := os.Lstat(root)
	if err != nil {
		return
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if *nFiles >= maxSkillFiles {
			return fs.SkipAll
		}
		if scan.SkipCursorPlugins && isCursorPlugins(path) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if d.IsDir() {
			if relDepth(rel) > maxSkillDepth {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() != "SKILL.md" {
			return nil
		}
		*nFiles++
		fi, infoErr := d.Info()
		if infoErr != nil || !fi.Mode().IsRegular() || fi.Size() > maxSkillBytes {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if len(data) > maxSkillBytes {
			return nil
		}
		name, desc, invocable, parseErr := parseSkillMarkdown(path, data, scan.NameFromDir)
		if parseErr != nil || !invocable {
			return nil
		}
		key := strings.ToLower(name)
		if key == "" {
			return nil
		}
		if _, ok := seen[key]; ok {
			return nil
		}
		seen[key] = struct{}{}
		*out = append(*out, Skill{Name: name, Description: desc})
		return nil
	})
}

func relDepth(rel string) int {
	if rel == "." {
		return 0
	}
	return strings.Count(rel, string(os.PathSeparator)) + 1
}

func isCursorPlugins(path string) bool {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == ".cursor" && parts[i+1] == "plugins" {
			return true
		}
	}
	return false
}

// parseSkillMarkdown reads a SKILL.md's identity, description and
// user-invocable bit. nameFromDir is cursor's rule (§3.6):
// on, the directory basename is the identity outright and the frontmatter
// name is never even consulted; off (grok), the frontmatter name wins and
// the directory is only the fallback. Either way a directory name that is not
// a single token fails skillNameOK below, so it is dropped rather than offered
// malformed.
func parseSkillMarkdown(path string, data []byte, nameFromDir bool) (name, desc string, invocable bool, err error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	invocable = true
	var fmName string
	hasFrontmatter := false
	if strings.HasPrefix(text, "---") {
		first, rest, found := strings.Cut(text, "\n")
		first = strings.TrimSuffix(first, "\r")
		if first == "---" && found {
			fm, ok := cutFrontmatter(rest)
			if !ok {
				return "", "", false, fmt.Errorf("unclosed frontmatter")
			}
			hasFrontmatter = true
			fmName, desc, invocable = parseFrontmatterLines(fm)
		}
	}
	// The directory basename wins outright when nameFromDir is set, and by
	// default whenever the frontmatter gave no name at all.
	name = fmName
	if nameFromDir || name == "" {
		name = filepath.Base(filepath.Dir(path))
	}
	if name == "" || name == "." || name == ".." {
		return "", "", false, fmt.Errorf("missing skill name")
	}
	if !skillNameOK(name) {
		return "", "", false, fmt.Errorf("invalid skill name %q", name)
	}
	// No frontmatter block at all: the description is the first non-empty
	// body line, literally — a "# Heading" keeps its "#" because this is the
	// agents' own rule, not markdown rendering. A frontmatter block with no
	// description key stays empty (today's "(skill)" label downstream).
	if !hasFrontmatter {
		desc = firstBodyLine(text)
	}
	return name, desc, invocable, nil
}

// skillNameOK is the token check a directory basename has to pass as well as a
// frontmatter name. A name carrying whitespace could never be typed as one
// slash token, and the check is unicode.IsSpace over the untrimmed name on
// purpose: trimming first would have let the directory "dir-alpha " through as
// "dir-alpha", and ContainsAny(" \t\n") would have let a non-breaking space
// through. A separator would mean a path arrived where a name was expected.
func skillNameOK(name string) bool {
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

// firstBodyLine is the first non-blank line of a frontmatter-less SKILL.md,
// trimmed of surrounding whitespace but otherwise verbatim. It scans line by
// line, like cutFrontmatter, rather than splitting the whole (up to 1 MiB)
// file into a slice just to keep its first element.
func firstBodyLine(text string) string {
	remaining := text
	for remaining != "" {
		line, next, found := strings.Cut(remaining, "\n")
		if trimmed := trimLine(line); trimmed != "" {
			return trimmed
		}
		if !found {
			break
		}
		remaining = next
	}
	return ""
}

// trimLine strips a trailing "\r" (CRLF line endings) and surrounding
// whitespace from one line of frontmatter or body text.
func trimLine(s string) string {
	return strings.TrimSpace(strings.TrimSuffix(s, "\r"))
}

func cutFrontmatter(rest string) (string, bool) {
	offset := 0
	remaining := rest
	for remaining != "" {
		line, next, found := strings.Cut(remaining, "\n")
		if strings.TrimSuffix(line, "\r") == "---" {
			return rest[:offset], true
		}
		if !found {
			break
		}
		offset += len(line) + 1
		remaining = next
	}
	return "", false
}

// parseFrontmatterLines reads name, description and the user-invocable bit
// off the line-oriented scalar frontmatter (no YAML library). invocable
// defaults true; it goes false only for user-invocable/user_invocable
// (cursor's and grok's own spellings of the same field, key case-insensitive)
// with value false or no, quotes stripped by unquoteScalar.
//
// Only top-level keys count. An indented line belongs to a block — cursor's own
// frontmatter nests things under `metadata:` — and the agents read only the
// document's own field, so trimming the indentation away would hide a skill
// they show.
func parseFrontmatterLines(fm string) (name, desc string, invocable bool) {
	invocable = true
	for _, raw := range strings.Split(fm, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		if raw != strings.TrimLeft(raw, " \t") {
			continue
		}
		line := trimLine(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(unquoteScalar(strings.TrimSpace(key)))
		val = unquoteScalar(strings.TrimSpace(val))
		switch key {
		case "name":
			name = strings.TrimSpace(val)
		case "description":
			desc = val
		case "user-invocable", "user_invocable":
			invocable = !isFalsyScalar(val)
		}
	}
	return name, desc, invocable
}

// isFalsyScalar is user-invocable's own value grammar: false or no,
// case-insensitive. Only the first field of the value is read, because YAML
// ends a plain scalar at an unquoted " #": "false # hidden" is still false, and
// a skill the agent hides must not be offered on the strength of a comment.
func isFalsyScalar(v string) bool {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToLower(fields[0]) {
	case "false", "no":
		return true
	default:
		return false
	}
}

func unquoteScalar(s string) string {
	if len(s) >= 2 {
		a, b := s[0], s[len(s)-1]
		if (a == '"' && b == '"') || (a == '\'' && b == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
