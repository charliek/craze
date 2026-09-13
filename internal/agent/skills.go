package agent

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
			walkSkillRoot(filepath.Join(base, rel), scan.SkipCursorPlugins, &nFiles, seenName, &out)
		}
	}
	return out
}

func walkSkillRoot(root string, skipPlugins bool, nFiles *int, seen map[string]struct{}, out *[]Skill) {
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
		if skipPlugins && isCursorPlugins(path) {
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
		name, desc, parseErr := parseSkillMarkdown(path, data)
		if parseErr != nil {
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

func parseSkillMarkdown(path string, data []byte) (string, string, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	var name, desc string
	if strings.HasPrefix(text, "---") {
		first, rest, found := strings.Cut(text, "\n")
		first = strings.TrimSuffix(first, "\r")
		if first == "---" && found {
			fm, ok := cutFrontmatter(rest)
			if !ok {
				return "", "", fmt.Errorf("unclosed frontmatter")
			}
			name, desc = parseFrontmatterLines(fm)
		}
	}
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(filepath.Dir(path))
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", "", fmt.Errorf("missing skill name")
	}
	if strings.ContainsAny(name, "/\\ \t\n") {
		return "", "", fmt.Errorf("invalid skill name %q", name)
	}
	return name, desc, nil
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

func parseFrontmatterLines(fm string) (name, desc string) {
	for _, line := range strings.Split(fm, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = unquoteScalar(strings.TrimSpace(val))
		switch key {
		case "name":
			name = val
		case "description":
			desc = val
		}
	}
	return name, desc
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
