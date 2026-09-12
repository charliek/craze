package agent

var modeAliases = map[string][]string{
	"plan":  {"plan", "architect"},
	"ask":   {"ask"},
	"agent": {"agent", "code", "default"},
}

func ResolveMode(want string, available []string) (string, bool) {
	if want == "" {
		return "", false
	}
	aliases := modeAliases[want]
	if aliases == nil {
		aliases = []string{want}
	}
	have := make(map[string]struct{}, len(available))
	for _, id := range available {
		have[id] = struct{}{}
	}
	for _, a := range aliases {
		if _, ok := have[a]; ok {
			return a, true
		}
	}
	return "", false
}

func modeIDs(modes []ModeInfo) []string {
	ids := make([]string, 0, len(modes))
	for _, m := range modes {
		ids = append(ids, m.ID)
	}
	return ids
}
