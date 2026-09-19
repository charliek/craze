package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/harness/gximport"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/paths"
)

// envGrokHome is gx's own home-directory variable: the second of the three
// ways `craze import gx` finds gx's files (plan 018 §3.3).
const envGrokHome = "GROK_HOME"

// importLockName is the file `craze import gx` flocks for the whole of a real
// import, so two invocations racing each other cannot interleave their reads
// and writes of providers.toml/models.toml. It never becomes a Save target
// itself and is never renamed, so every writer locks the same inode — the
// same rule config.toml's own lock follows (internal/tui/config.go).
//
// It lives in the craze directory, beside config.toml, and not in native/:
// taking a lock inside native/ meant creating native/ first, so a first
// import that then failed — no gx home, nothing importable — left an empty
// native/ (and the lock) behind. native/ is created by modeltable.Save, only
// once there is something to write into it.
const importLockName = "import.lock"

// crazeDirPerm is the mode a first import creates the craze directory with
// when nothing has created it yet, so the lock has somewhere to live. An
// existing craze directory's mode is left as it is: it is shared with
// config.toml and the session index, and not this command's to change.
const crazeDirPerm = 0o700

// newImportCmd is `craze import`, a parent with one subcommand today (`gx`);
// `craze import claude` is later work (D-13). It defines no PersistentPreRunE
// of its own — the root's removed-config-variable tripwire is a
// *cobra.Command field that a child would replace, not extend — so it must
// run before this or any subcommand does anything (internal/cli/root.go).
func newImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import model and provider settings into craze",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newImportGxCmd())
	return cmd
}

type importGxOpts struct {
	grokHome string
	dryRun   bool
}

// newImportGxCmd is `craze import gx`. Its help text says only what a user
// choosing a source needs — gx is where the settings come from — and never
// says native, harness, or --provider native: the provider that reads what
// this writes stays hidden (plan 018 §3.4).
func newImportGxCmd() *cobra.Command {
	o := &importGxOpts{}
	cmd := &cobra.Command{
		Use:   "gx",
		Short: "Import model and provider settings from gx into craze",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&o.grokHome, "grok-home", "",
		"gx's home directory (default: $GROK_HOME, else ~/.grok)")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false,
		"report what an import would do without writing anything")
	return cmd
}

// run is the whole of `craze import gx`: resolve gx's directory and craze's
// own, merge under lock, print a report that never carries a secret, and
// write it back unless --dry-run. It returns a plain error for a failure
// (exit 1: internal/cli/exit.go) — the command was invoked correctly, the
// import itself did not go through.
func (o *importGxOpts) run(out io.Writer) error {
	grokHome, source, err := resolveGrokHome(o.grokHome)
	if err != nil {
		return err
	}
	if err := checkGrokHome(grokHome, source); err != nil {
		return err
	}
	destDir := paths.NativeDir()
	if destDir == "" {
		return errors.New("craze: no craze directory to import into (set HOME or CRAZE_HOME)")
	}

	// A real import is locked end to end, from the read that decides what
	// merges to the write that lands it, so two invocations racing each other
	// cannot each merge into the same starting point and then both save,
	// silently dropping one's changes. The lock is in the craze directory —
	// native/'s parent, which NativeDir has already made absolute — so
	// nothing under native/ exists until Save has something to write
	// (importLockName). A dry run reads and writes nothing, so there is no
	// race for it to have and nothing of its own to create — "prefer creating
	// nothing at all on --dry-run" (plan 018 §3.3): not even the craze
	// directory or this lock file.
	if !o.dryRun {
		crazeDir := filepath.Dir(destDir)
		if err := os.MkdirAll(crazeDir, crazeDirPerm); err != nil {
			return fmt.Errorf("craze: %w", err)
		}
		unlock, err := atomicfile.Lock(filepath.Join(crazeDir, importLockName))
		if err != nil {
			return fmt.Errorf("craze: taking the import lock: %w", err)
		}
		defer unlock()
	}

	existing, err := modeltable.LoadForImport(destDir)
	if err != nil {
		return fmt.Errorf("craze: %w", err)
	}
	var warnings []string
	if existing != nil {
		warnings = append(warnings, existing.Warnings...)
	}

	merged, report, err := gximport.Import(grokHome, existing)
	nothingToImport := errors.Is(err, gximport.ErrNothingToImport)
	if err != nil && !nothingToImport {
		return fmt.Errorf("craze: %w", err)
	}
	// Import still fills in the report (what was skipped, and why) even when
	// nothing passed the filters, which is exactly what the owner needs to
	// see to fix gx's config or --grok-home; only a failure earlier than the
	// merge — the wrong directory, a file it could not parse — has nothing
	// to report.
	printReport(out, grokHome, destDir, report, warnings, o.dryRun)
	if nothingToImport {
		return errors.New("craze: import gx: no model could be imported (see the report above)")
	}
	if o.dryRun {
		return nil
	}
	if err := modeltable.Save(destDir, merged); err != nil {
		return fmt.Errorf("craze: %w", err)
	}
	return nil
}

// resolveGrokHome is --grok-home, else $GROK_HOME, else ~/.grok — the same
// three-level precedence every other craze setting resolves through, flag
// first (plan 018 §3.3) — and which of the three it came from, so an error
// about the directory can name the setting that chose it. paths.HomeDir is
// what finds "~" here, exactly as it does for craze's own directory, rather
// than this package guessing at the account database on its own.
func resolveGrokHome(flag string) (dir, source string, err error) {
	if v := strings.TrimSpace(flag); v != "" {
		return v, "--grok-home", nil
	}
	if v := strings.TrimSpace(os.Getenv(envGrokHome)); v != "" {
		return v, envGrokHome, nil
	}
	home := paths.HomeDir()
	if home == "" {
		return "", "", errors.New("craze: no home directory to find gx's config in (set --grok-home, GROK_HOME, or HOME)")
	}
	return filepath.Join(home, ".grok"), "~/.grok", nil
}

// checkGrokHome refuses a gx home that is a file rather than a directory —
// the easy mistake is to point --grok-home at ~/.grok/config.toml itself,
// which otherwise surfaces as a puzzling ".../config.toml/config.toml" error.
// A directory that does not exist, or one Stat cannot read, is left for the
// import to report the way it always has (it names the directory and what it
// looked for); only the file case gets its own message. source names the
// setting that chose dir (resolveGrokHome).
func checkGrokHome(dir, source string) error {
	info, err := os.Stat(dir)
	if err != nil || info.IsDir() {
		return nil
	}
	return fmt.Errorf("craze: %s must be gx's home directory (the one holding config.toml), not a file: %s", source, dir)
}

// printReport is the whole of what `craze import gx` prints: stable and
// human-readable, and never a Report field that is not a name, a bucket, or a
// reason (Report's own doc comment; §3.3's "never prints an api_key, the
// value of any environment variable named in env_keys, or a file body"). The
// same function runs for --dry-run and for a real import — only the closing
// line differs — so the report is never what invites the two to drift.
func printReport(w io.Writer, grokHome, destDir string, report gximport.Report, warnings []string, dryRun bool) {
	fmt.Fprintf(w, "craze import gx: %s -> %s\n", grokHome, destDir)
	printChanges(w, "providers", report.Providers)
	printChanges(w, "models", report.Models)
	if report.DefaultModel != "" {
		fmt.Fprintf(w, "default model: %s (%s)\n", report.DefaultModel, report.DefaultRule)
	}
	for _, warn := range warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
	if dryRun {
		fmt.Fprintln(w, "dry run: nothing written")
	}
}

func printChanges(w io.Writer, kind string, c gximport.Changes) {
	fmt.Fprintf(w, "%s:\n", kind)
	printBucket(w, "added", c.Added)
	printBucket(w, "updated", c.Updated)
	printBucket(w, "unchanged", c.Unchanged)
	printBucket(w, "kept", c.Kept)
	printBucket(w, "stale", c.Stale)
	if len(c.Skipped) > 0 {
		fmt.Fprintf(w, "  skipped (%d):\n", len(c.Skipped))
		for _, s := range c.Skipped {
			fmt.Fprintf(w, "    %s: %s\n", s.ID, s.Reason)
		}
	}
	if len(c.Notes) > 0 {
		fmt.Fprintf(w, "  notes (%d):\n", len(c.Notes))
		for _, n := range c.Notes {
			fmt.Fprintf(w, "    %s: %s\n", n.ID, n.Text)
		}
	}
}

func printBucket(w io.Writer, label string, ids []string) {
	if len(ids) == 0 {
		return
	}
	fmt.Fprintf(w, "  %s (%d): %s\n", label, len(ids), strings.Join(ids, ", "))
}
