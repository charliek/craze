//go:build unix

package journal

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// withUmask sets the process's umask for one test and restores it after.
// The umask belongs to the whole process, so no test in this package runs in
// parallel (none calls t.Parallel): a parallel test creating files while
// this one holds an odd umask would see modes it did not ask for.
func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

// modeOf is path's permission bits.
func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// TestModesAreExactUnderAnOpenOrARestrictiveUmask (A20): the journal asks
// for 0700 directories and a 0600 file, so an open umask (0) cannot widen
// them and a restrictive one (0077) takes nothing they have.
func TestModesAreExactUnderAnOpenOrARestrictiveUmask(t *testing.T) {
	for _, mask := range []int{0, 0o022, 0o077} {
		opts := testOptions(t) // its temp directory made before the umask changes
		withUmask(t, mask)
		w := newWriter(t, opts)
		w.Append(event(1))
		closeWriter(t, w)
		for _, dir := range []string{opts.Dir, filepath.Dir(w.Path())} {
			if got := modeOf(t, dir); got != 0o700 {
				t.Errorf("umask %04o: %s is %04o, want 0700", mask, dir, got)
			}
		}
		if got := modeOf(t, w.Path()); got != 0o600 {
			t.Errorf("umask %04o: the file is %04o, want 0600", mask, got)
		}
	}
}

// TestAUmaskThatNarrowsTheFileNeverBreaksTheJournal (A20): a umask that
// takes the owner's write bit makes the file 0400, narrower than asked,
// and the journal still writes it (the descriptor was opened for writing)
// and still reads it back.
func TestAUmaskThatNarrowsTheFileNeverBreaksTheJournal(t *testing.T) {
	opts := testOptions(t)
	w := newWriter(t, opts)
	// The directory exists already, as it would for a second session in the
	// same workspace; a 0277 umask on a directory the journal had to create
	// would leave it unwritable, which no umask a person sets would do.
	if err := os.MkdirAll(filepath.Dir(w.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	withUmask(t, 0o277)
	appendEvents(w, 1, 3)
	waitFlushed(t, w, 3)
	if got := modeOf(t, w.Path()); got != 0o400 {
		t.Fatalf("the file is %04o, want the umask's narrower 0400", got)
	}
	recs, err := collect(func(fn func(Record) error) error { return w.ReadRange(1, 3, fn) })
	if err != nil || !slices.Equal(seqs(recs), []uint64{1, 2, 3}) {
		t.Fatalf("ReadRange = %v, %v", seqs(recs), err)
	}
	closeWriter(t, w)
	if h := w.Health(); h.State != StateOK {
		t.Fatalf("Health = %+v, want ok", h)
	}
}
