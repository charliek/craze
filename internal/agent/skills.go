package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxSkillDepth   = 10
	maxSkillFiles   = 256
	maxSkillBytes   = 1 << 20
	maxInspectBytes = 1 << 20
	inspectTimeout  = 4 * time.Second
)

// Skill is a provider skill the slash picker can offer.
type Skill struct {
	Name        string
	Description string
}

type inspectRunner func(bin string, args []string, dir string, timeout time.Duration) ([]byte, error)

func execInspect(bin string, args []string, dir string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	var buf cappedBuffer
	buf.max = maxInspectBytes
	cmd.Stdout = &buf
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("agent: inspect timed out")
	}
	if buf.hit {
		return nil, fmt.Errorf("agent: inspect output too large")
	}
	return buf.buf.Bytes(), err
}

// cappedBuffer stops buffering once max bytes have been written so inspect
// cannot grow without bound.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
	hit bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.hit {
		return len(p), nil
	}
	if c.buf.Len()+len(p) > c.max {
		c.hit = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

// SkillDiscovery is one catalog lookup. InspectErr is set when inspect was
// attempted and failed (timeout, spawn, or decode); Skills is then the
// filesystem fallback rather than an empty catalog treated as success.
type SkillDiscovery struct {
	Skills     []Skill
	InspectErr error
}

// DiscoverSkills is the provider-owned catalog: inspect when advertised,
// otherwise a filesystem walk of SkillScan.RelRoots. Inspect failure falls
// back to the walk rather than caching an empty catalog.
func DiscoverSkills(p Provider, cwd, home, bin string) []Skill {
	return DiscoverSkillsReport(p, cwd, home, bin).Skills
}

// DiscoverSkillsReport is DiscoverSkills plus whether inspect failed, so the
// TUI can surface a diagnostic without treating a valid empty catalog as an
// error.
func DiscoverSkillsReport(p Provider, cwd, home, bin string) SkillDiscovery {
	return discoverSkillsReport(p, cwd, home, bin, execInspect)
}

func discoverSkills(p Provider, cwd, home, bin string, run inspectRunner) []Skill {
	return discoverSkillsReport(p, cwd, home, bin, run).Skills
}

func discoverSkillsReport(p Provider, cwd, home, bin string, run inspectRunner) SkillDiscovery {
	scan := p.SkillScan()
	if len(scan.InspectArgs) > 0 && bin != "" && run != nil {
		out, err := run(bin, scan.InspectArgs, cwd, inspectTimeout)
		if err != nil {
			return SkillDiscovery{Skills: walkProviderSkills(scan, cwd, home), InspectErr: err}
		}
		skills, ok := parseInspectSkills(out)
		if !ok {
			return SkillDiscovery{
				Skills:     walkProviderSkills(scan, cwd, home),
				InspectErr: fmt.Errorf("agent: inspect: invalid json"),
			}
		}
		return SkillDiscovery{Skills: skills}
	}
	return SkillDiscovery{Skills: walkProviderSkills(scan, cwd, home)}
}

func parseInspectSkills(raw []byte) ([]Skill, bool) {
	var parsed struct {
		Skills []struct {
			Name          string `json:"name"`
			Description   string `json:"description"`
			UserInvocable *bool  `json:"userInvocable"`
			Source        struct {
				Type string `json:"type"`
				Path string `json:"path"`
			} `json:"source"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, false
	}
	if parsed.Skills == nil {
		return nil, false
	}
	out := make([]Skill, 0, len(parsed.Skills))
	seen := make(map[string]struct{})
	for _, e := range parsed.Skills {
		if e.UserInvocable != nil && !*e.UserInvocable {
			continue
		}
		name := strings.TrimSpace(e.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Skill{Name: name, Description: strings.TrimSpace(e.Description)})
		if len(out) >= maxSkillFiles {
			break
		}
	}
	return out, true
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
