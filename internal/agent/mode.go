package agent

// modeVocabulary is craze's whole mode vocabulary: one canonical name per
// kind, every spelling an agent is known to advertise for it, and what craze
// takes that mode to mean. ResolveMode and Provider both read this table, so
// there is no second list of mode names anywhere.
var modeVocabulary = []struct {
	canonical string
	kind      ModeKind
	aliases   []string
}{
	{"agent", ModeImplement, []string{"agent", "code", "default"}},
	{"plan", ModePlan, []string{"plan", "architect"}},
	{"ask", ModeReadOnly, []string{"ask"}},
}

// modeAliasesOf is every spelling of a canonical mode name, in preference
// order. A name the table does not know stands for itself.
func modeAliasesOf(want string) []string {
	for _, v := range modeVocabulary {
		if v.canonical == want {
			return v.aliases
		}
	}
	return []string{want}
}

func ResolveMode(want string, available []string) (string, bool) {
	if want == "" {
		return "", false
	}
	have := make(map[string]struct{}, len(available))
	for _, id := range available {
		have[id] = struct{}{}
	}
	for _, a := range modeAliasesOf(want) {
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
