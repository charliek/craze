package modeltable

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// models.toml's [compaction] section (plan 028 §3.6): auto, threshold_percent
// and tail_tokens, each at its default when left out, strict and validated
// like the rest of the file, and saved exactly as written — no section for a
// table that sets none. (ptr is secret_test.go's.)

// TestCompactionDefaults: a models.toml with no [compaction] loads as the
// zero section, whose settings are the defaults — auto on, 85%, a 20,000-token
// tail — and a section that sets one key leaves the other two at theirs.
func TestCompactionDefaults(t *testing.T) {
	got, err := Load(writeFiles(t, validProviders, validModels))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Compaction, Compaction{}) {
		t.Fatalf("Compaction = %+v, want the zero value", got.Compaction)
	}
	c := got.Compaction
	if !c.Auto() || c.ThresholdPercent() != 85 || c.TailTokens() != 20000 {
		t.Fatalf("the defaults = auto %v, %d%%, tail %d; want true, 85%%, 20000", c.Auto(), c.ThresholdPercent(), c.TailTokens())
	}

	got, err = Load(writeFiles(t, validProviders, validModels+"\n[compaction]\nthreshold_percent = 70\n"))
	if err != nil {
		t.Fatal(err)
	}
	c = got.Compaction
	if !c.Auto() || c.ThresholdPercent() != 70 || c.TailTokens() != 20000 || c.AutoSet != nil || c.TailTokensSet != nil {
		t.Fatalf("one key set: %+v (auto %v, %d%%, tail %d); want 70%% and the other two left at their defaults",
			c, c.Auto(), c.ThresholdPercent(), c.TailTokens())
	}
}

// TestCompactionSectionRoundTrip: a section round-trips through Save and Load
// with exactly the keys it set — auto = false and tail_tokens = 0 included,
// which are not their defaults' absence — and saves the same bytes twice; a
// table that sets none writes no section at all (TestSaveWritesStableReadableFiles
// pins that body byte for byte).
func TestCompactionSectionRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Compaction
		want []string // lines the file holds
		not  []string // keys it must not
	}{
		{"every key", Compaction{AutoSet: ptr(false), ThresholdPercentSet: ptr(60), TailTokensSet: ptr(0)},
			[]string{"[compaction]\n", "auto = false\n", "threshold_percent = 60\n", "tail_tokens = 0\n"}, nil},
		{"one key", Compaction{TailTokensSet: ptr(5000)},
			[]string{"[compaction]\n", "tail_tokens = 5000\n"}, []string{"auto", "threshold_percent"}},
		{"none", Compaction{}, nil, []string{"compaction", "auto", "threshold_percent", "tail_tokens"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl := validTable()
			tbl.Compaction = tc.c
			dir := t.TempDir()
			if err := Save(dir, tbl); err != nil {
				t.Fatal(err)
			}
			got, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tbl) {
				t.Fatalf("round trip =\n%+v\nwant\n%+v", got.Compaction, tbl.Compaction)
			}
			mb, err := os.ReadFile(filepath.Join(dir, ModelsFile))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(mb), want) {
					t.Errorf("%s does not hold %q:\n%s", ModelsFile, want, mb)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(string(mb), not) {
					t.Errorf("%s holds %q:\n%s", ModelsFile, not, mb)
				}
			}
			dir2 := t.TempDir()
			if err := Save(dir2, got); err != nil {
				t.Fatal(err)
			}
			if mb2, err := os.ReadFile(filepath.Join(dir2, ModelsFile)); err != nil || string(mb2) != string(mb) {
				t.Fatalf("a second save wrote other bytes (%v):\n%s\nwant\n%s", err, mb2, mb)
			}
		})
	}
}

// TestCompactionUnknownKeyIsStrict: a key [compaction] does not have fails the
// load, naming the file, the table and the key, as every unknown key does.
func TestCompactionUnknownKeyIsStrict(t *testing.T) {
	dir := writeFiles(t, validProviders, validModels+"\n[compaction]\nthreshold = 80\n")
	_, err := Load(dir)
	fe := wantFileError(t, err, filepath.Join(dir, ModelsFile), "compaction", "threshold")
	if fe.Reason != "unknown key" {
		t.Fatalf("reason = %q, want unknown key", fe.Reason)
	}
}

// TestValidateCompactionBounds: threshold_percent is 1 to 99 and tail_tokens
// is not negative, whether the table was loaded — the error then names the
// file's path — or built in memory; both ends of each range load.
func TestValidateCompactionBounds(t *testing.T) {
	for _, tc := range []struct {
		section string
		key     string // "" for a valid section
	}{
		{"threshold_percent = 0", "threshold_percent"},
		{"threshold_percent = 100", "threshold_percent"},
		{"threshold_percent = -5", "threshold_percent"},
		{"tail_tokens = -1", "tail_tokens"},
		{"threshold_percent = 1", ""},
		{"threshold_percent = 99", ""},
		{"tail_tokens = 0", ""},
		{"auto = false", ""},
	} {
		t.Run(tc.section, func(t *testing.T) {
			dir := writeFiles(t, validProviders, validModels+"\n[compaction]\n"+tc.section+"\n")
			got, err := Load(dir)
			if tc.key == "" {
				if err != nil {
					t.Fatalf("a valid section: %v", err)
				}
				if err := got.Validate(); err != nil {
					t.Fatalf("Validate of the loaded table: %v", err)
				}
				return
			}
			wantFileError(t, err, filepath.Join(dir, ModelsFile), "compaction", tc.key)
		})
	}
	tbl := validTable()
	tbl.Compaction = Compaction{ThresholdPercentSet: ptr(0)}
	wantFileError(t, tbl.Validate(), ModelsFile, "compaction", "threshold_percent")
	tbl.Compaction = Compaction{TailTokensSet: ptr(-20000)}
	wantFileError(t, tbl.Validate(), ModelsFile, "compaction", "tail_tokens")
}
