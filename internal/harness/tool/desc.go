package tool

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Render fills a tool description's ${name} placeholders from vars, the way
// opencode renders its shell prompt (shell/prompt.ts:28-34): every
// placeholder must have a value, and a "${" that does not open a known
// ${name} is an error rather than text the model reads. Values are inserted
// as they are and not scanned again, so a value may itself hold "${".
//
// It uses strings.Replacer, never text/template: the template executor looks
// methods up by name, so the linker keeps every exported method of every
// linked type, and cmd/craze's link test fails the build on it (D-37).
func Render(text string, vars map[string]string) (string, error) {
	for name := range vars {
		if !wordName(name) {
			return "", fmt.Errorf("tool: description variable %q is not a name (letters, digits, underscores)", name)
		}
	}
	for rest := text; ; {
		i := strings.Index(rest, "${")
		if i < 0 {
			break
		}
		rest = rest[i+2:]
		j := strings.IndexByte(rest, '}')
		if j < 0 {
			return "", fmt.Errorf("tool: description has a %q with no closing brace", "${")
		}
		if _, ok := vars[rest[:j]]; !ok {
			return "", fmt.Errorf("tool: description placeholder ${%s} has no value", rest[:j])
		}
		rest = rest[j+1:]
	}
	pairs := make([]string, 0, 2*len(vars))
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		pairs = append(pairs, "${"+name+"}", vars[name])
	}
	return strings.NewReplacer(pairs...).Replace(text), nil
}

// wordName reports whether s matches opencode's placeholder name, \w+.
func wordName(s string) bool { return allBytes(s, wordByte) }

// allBytes reports whether s is non-empty and ok holds for its every byte.
func allBytes(s string, ok func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !ok(s[i]) {
			return false
		}
	}
	return s != ""
}

// wordByte reports whether c is in \w: an ASCII letter, digit or underscore.
func wordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
