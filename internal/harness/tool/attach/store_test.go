package attach

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
)

// attachDir is a fresh attachments directory's path (not yet created) under
// a private temp parent: the store makes it itself.
func attachDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "attachments")
}

// madeDir is attachDir already created, 0700: an attachments directory the
// store made earlier.
func madeDir(t *testing.T) string {
	t.Helper()
	dir := attachDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// TestSaveWritesPrivately: the directory is created 0700, the file is 0600,
// named by its content's hash, at an absolute path, holding the bytes — and
// no temp file is left beside it.
func TestSaveWritesPrivately(t *testing.T) {
	dir := attachDir(t)
	data := encodePNG(t, gradient(16, 16))
	path, err := Save(dir, data, MIMEPNG)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || filepath.Dir(path) != dir || !StoredName(filepath.Base(path)) || !strings.HasSuffix(path, ".png") {
		t.Fatalf("path %q", path)
	}
	if got := perm(t, dir); got != 0o700 {
		t.Fatalf("directory mode %v, want 0700", got)
	}
	if got := perm(t, path); got != 0o600 {
		t.Fatalf("file mode %v, want 0600", got)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the file holds %d bytes (%v), want the %d saved", len(got), err, len(data))
	}
	if got := names(t, dir); len(got) != 2 || got[0] != lockName || got[1] != filepath.Base(path) {
		t.Fatalf("the directory holds %q, want the store's lock and the one file", got)
	}
	// The other two types the store keeps, by their own extensions.
	for mime, ext := range map[string]string{MIMEJPEG: ".jpg", MIMEWebP: ".webp"} {
		p, err := Save(dir, []byte(mime), mime)
		if err != nil || !strings.HasSuffix(p, ext) {
			t.Fatalf("Save(%s) = %q, %v", mime, p, err)
		}
	}
	if _, err := Save(dir, data, mimeGIF); err == nil {
		t.Fatal("a GIF was stored; Process never emits one")
	}
	if _, err := Save(dir, make([]byte, MaxBytes+1), MIMEPNG); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Save of more than MaxBytes = %v, want ErrTooLarge", err)
	}
}

// TestSaveTightensAWideDirectory: a directory the user made 0755 is put back
// to 0700 by the store, through its descriptor.
func TestSaveTightensAWideDirectory(t *testing.T) {
	dir := attachDir(t)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(dir, []byte("x"), MIMEPNG); err != nil {
		t.Fatal(err)
	}
	if got := perm(t, dir); got != 0o700 {
		t.Fatalf("directory mode %v, want 0700", got)
	}
}

// TestSaveDedupeRefreshesTheTime: the same bytes saved twice are one file,
// not rewritten (the same inode), with its modification time brought up to
// now so the age sweep keeps it.
func TestSaveDedupeRefreshesTheTime(t *testing.T) {
	dir := attachDir(t)
	data := encodePNG(t, gradient(16, 16))
	path, err := Save(dir, data, MIMEPNG)
	if err != nil {
		t.Fatal(err)
	}
	before := inode(t, path)
	old := time.Now().Add(-6 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	again, err := Save(dir, data, MIMEPNG)
	if err != nil || again != path {
		t.Fatalf("Save again = %q, %v; want %q", again, err, path)
	}
	if inode(t, path) != before {
		t.Fatal("a dedupe hit rewrote the file")
	}
	fi, _ := os.Stat(path)
	if time.Since(fi.ModTime()) > time.Minute {
		t.Fatalf("the modification time is still %v", fi.ModTime())
	}
}

// TestSaveReplacesOtherBytesAtomically: a file already at the name with
// other bytes — planted, or a truncated write from another tool — is replaced
// by a rename (a new inode), never written into.
func TestSaveReplacesOtherBytesAtomically(t *testing.T) {
	dir := attachDir(t)
	data := encodePNG(t, gradient(16, 16))
	path, err := Save(dir, data, MIMEPNG)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := inode(t, path)
	if _, err := Save(dir, data, MIMEPNG); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
		t.Fatal("the planted bytes survived")
	}
	if inode(t, path) == before {
		t.Fatal("the file was written in place, not renamed over")
	}
	if got := names(t, dir); len(got) != 2 || got[0] != lockName || got[1] != filepath.Base(path) {
		t.Fatalf("the directory holds %q, want the store's lock and the one file", got)
	}
}

// TestOpenDirRefuses is every directory the host will not read from.
func TestOpenDirRefuses(t *testing.T) {
	t.Run("relative", func(t *testing.T) {
		if _, err := OpenDir("attachments"); err == nil {
			t.Fatal("a relative directory opened")
		}
		if _, err := Save("attachments", []byte("x"), MIMEPNG); err == nil {
			t.Fatal("Save took a relative directory")
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, err := OpenDir(attachDir(t)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("OpenDir = %v, want not-exist", err)
		}
	})
	t.Run("a symlink", func(t *testing.T) {
		real := madeDir(t)
		link := filepath.Join(t.TempDir(), "attachments")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenDir(link); !errors.Is(err, ErrUnsafeDir) {
			t.Fatalf("OpenDir = %v, want ErrUnsafeDir", err)
		}
		if _, err := Save(link, []byte("x"), MIMEPNG); !errors.Is(err, ErrUnsafeDir) {
			t.Fatalf("Save = %v, want ErrUnsafeDir", err)
		}
	})
	t.Run("a file", func(t *testing.T) {
		dir := attachDir(t)
		if err := os.WriteFile(dir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenDir(dir); !errors.Is(err, ErrUnsafeDir) {
			t.Fatalf("OpenDir = %v, want ErrUnsafeDir", err)
		}
	})
	t.Run("not 0700", func(t *testing.T) {
		dir := madeDir(t)
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenDir(dir); !errors.Is(err, ErrUnsafeDir) {
			t.Fatalf("OpenDir = %v, want ErrUnsafeDir", err)
		}
		if got := perm(t, dir); got != 0o750 {
			t.Fatalf("the read changed the mode to %v", got)
		}
	})
	t.Run("swapped while opening", func(t *testing.T) {
		dir := madeDir(t)
		other := filepath.Join(filepath.Dir(dir), "other")
		if err := os.Mkdir(other, 0o700); err != nil {
			t.Fatal(err)
		}
		prev := dirChecked
		t.Cleanup(func() { dirChecked = prev })
		dirChecked = func() {
			// Swap the checked directory for a symlink to another one in
			// the same parent: os.Root follows it, and the comparison
			// must catch it.
			if err := os.Rename(dir, dir+".gone"); err != nil {
				t.Error(err)
			}
			if err := os.Symlink("other", dir); err != nil {
				t.Error(err)
			}
		}
		if _, err := OpenDir(dir); !errors.Is(err, ErrUnsafeDir) {
			t.Fatalf("OpenDir = %v, want ErrUnsafeDir", err)
		}
	})
}

const stored = "0123456789abcdef.png"

// TestReadFile is the host's confined read of one stored name.
func TestReadFile(t *testing.T) {
	data := encodePNG(t, gradient(16, 16))
	// prepare is a created attachments directory with plant's entry in it,
	// opened for reading.
	prepare := func(t *testing.T, plant func(dir string)) *os.Root {
		t.Helper()
		dir := madeDir(t)
		plant(dir)
		root, err := OpenDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { root.Close() })
		return root
	}
	setup := func(t *testing.T, plant func(dir string)) ([]byte, error) {
		t.Helper()
		return ReadFile(prepare(t, plant), stored)
	}
	t.Run("a regular file", func(t *testing.T) {
		got, err := setup(t, func(dir string) {
			if err := os.WriteFile(filepath.Join(dir, stored), data, 0o600); err != nil {
				t.Fatal(err)
			}
		})
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("ReadFile = %d bytes, %v", len(got), err)
		}
	})
	t.Run("a missing file", func(t *testing.T) {
		if _, err := setup(t, func(string) {}); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ReadFile = %v, want not-exist", err)
		}
	})
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, dir string)
		want  error
	}{
		{"a symlink inside the directory", func(t *testing.T, dir string) {
			// os.Root would follow this one, O_NOFOLLOW or not.
			if err := os.WriteFile(filepath.Join(dir, "fedcba9876543210.png"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("fedcba9876543210.png", filepath.Join(dir, stored)); err != nil {
				t.Fatal(err)
			}
		}, ErrNotRegular},
		{"a symlink out of the directory", func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "secret.png")
			if err := os.WriteFile(target, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, stored)); err != nil {
				t.Fatal(err)
			}
		}, ErrNotRegular},
		{"a FIFO", func(t *testing.T, dir string) {
			if err := syscall.Mkfifo(filepath.Join(dir, stored), 0o600); err != nil {
				t.Fatal(err)
			}
		}, ErrNotRegular},
		{"a directory", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, stored), 0o700); err != nil {
				t.Fatal(err)
			}
		}, ErrNotRegular},
		{"over the cap", func(t *testing.T, dir string) {
			// Sparse: the size is what matters, and it costs no disk.
			if err := os.WriteFile(filepath.Join(dir, stored), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(filepath.Join(dir, stored), MaxBytes+1); err != nil {
				t.Fatal(err)
			}
		}, ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := prepare(t, func(dir string) { tc.plant(t, dir) })
			if err := readGuarded(t, root); !errors.Is(err, tc.want) {
				t.Fatalf("ReadFile = %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("growing past the cap after the fstat", func(t *testing.T) {
		var path string
		prev := sizeChecked
		t.Cleanup(func() { sizeChecked = prev })
		sizeChecked = func() {
			if err := os.Truncate(path, MaxBytes+10); err != nil {
				t.Error(err)
			}
		}
		_, err := setup(t, func(dir string) {
			path = filepath.Join(dir, stored)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		})
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("ReadFile = %v, want ErrTooLarge", err)
		}
	})
	// Swaps after the Lstat said "regular": what the open and the fstat
	// comparison are for. A FIFO is the case O_NONBLOCK exists for — opened
	// without it, with no writer, the open itself would never return.
	for _, tc := range []struct {
		name string
		swap func(t *testing.T, path string)
	}{
		{"swapped for a FIFO", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Error(err)
			}
		}},
		{"swapped for a symlink to another stored file", func(t *testing.T, path string) {
			other := filepath.Join(filepath.Dir(path), "fedcba9876543210.png")
			if err := os.WriteFile(other, data, 0o600); err != nil {
				t.Error(err)
			}
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
			if err := os.Symlink("fedcba9876543210.png", path); err != nil {
				t.Error(err)
			}
		}},
		{"swapped for another regular file", func(t *testing.T, path string) {
			other := filepath.Join(filepath.Dir(path), "other")
			if err := os.WriteFile(other, data, 0o600); err != nil {
				t.Error(err)
			}
			if err := os.Rename(other, path); err != nil {
				t.Error(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			root := prepare(t, func(dir string) {
				path = filepath.Join(dir, stored)
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			})
			prev := nameChecked
			t.Cleanup(func() { nameChecked = prev })
			nameChecked = func() { tc.swap(t, path) }
			if err := readGuarded(t, root); !errors.Is(err, ErrNotRegular) {
				t.Fatalf("ReadFile = %v, want ErrNotRegular", err)
			}
		})
	}
	t.Run("a name the store does not write", func(t *testing.T) {
		root := prepare(t, func(string) {})
		for _, name := range []string{"x.png", "../" + stored, "0123456789ABCDEF.png", "0123456789abcdef.gif", "." + stored, ""} {
			if _, err := ReadFile(root, name); !errors.Is(err, ErrNotStored) {
				t.Errorf("ReadFile(%q) = %v, want ErrNotStored", name, err)
			}
		}
	})
	t.Run("by its path", func(t *testing.T) {
		var dir string
		root := prepare(t, func(d string) {
			dir = d
			if err := os.WriteFile(filepath.Join(d, stored), data, 0o600); err != nil {
				t.Fatal(err)
			}
		})
		if got, err := ReadPath(root, filepath.Join(dir, stored)); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("ReadPath = %d bytes, %v", len(got), err)
		}
		for _, path := range []string{
			stored,                                   // relative
			dir + "/./" + stored,                     // not clean
			dir + "//" + stored,                      // not clean
			filepath.Join(dir, "sub", stored),        // not directly inside
			filepath.Join(filepath.Dir(dir), stored), // beside the directory
			filepath.Join(dir, "x.png"),              // not a stored name
		} {
			if _, err := ReadPath(root, path); !errors.Is(err, ErrNotStored) {
				t.Errorf("ReadPath(%q) = %v, want ErrNotStored", path, err)
			}
		}
	})
}

// readGuarded is ReadFile(root, stored) on its own goroutine, so a read that
// blocks — a FIFO opened without O_NONBLOCK waits for a writer forever —
// fails the test instead of hanging it.
func readGuarded(t *testing.T, root *os.Root) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := ReadFile(root, stored)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("ReadFile blocked")
		return nil
	}
}

func TestStoredAndTempNames(t *testing.T) {
	for name, want := range map[string]bool{
		"0123456789abcdef.png":  true,
		"0123456789abcdef.jpg":  true,
		"0123456789abcdef.webp": true,
		"0123456789abcdef.gif":  false,
		"0123456789abcde.png":   false,
		"0123456789abcdefa.png": false,
		"0123456789abcdeg.png":  false,
		"0123456789abcdef":      false,
		"0123456789abcdef.png.": false,
		"":                      false,
	} {
		if got := StoredName(name); got != want {
			t.Errorf("StoredName(%q) = %v", name, got)
		}
	}
	for name, want := range map[string]bool{
		".0123456789abcdef.png.0badc0de.tmp": true,
		"0123456789abcdef.png.0badc0de.tmp":  false,
		".0123456789abcdef.png.0badc0d.tmp":  false,
		".0123456789abcdef.png.0badc0de":     false,
		".x.png.0badc0de.tmp":                false,
		".tmp":                               false,
	} {
		if got := tempName(name); got != want {
			t.Errorf("tempName(%q) = %v", name, got)
		}
	}
}

// plantAged writes size bytes at name in dir with the given age.
func plantAged(t *testing.T, dir, name string, size int, age time.Duration, now time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	when := now.Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestSweepByAge: everything the store wrote that is older than seven days
// goes — stored files and a crashed write's temp file — and nothing else:
// not a younger file, not a file of another name, not a symlink or a
// directory with a stored name, and not the directory.
func TestSweepByAge(t *testing.T) {
	dir := madeDir(t)
	now := time.Now()
	week := MaxAge
	plantAged(t, dir, "aaaaaaaaaaaaaaaa.png", 10, week+time.Hour, now)
	plantAged(t, dir, ".aaaaaaaaaaaaaaab.jpg.01234567.tmp", 10, week+time.Hour, now)
	plantAged(t, dir, "bbbbbbbbbbbbbbbb.png", 10, week-time.Hour, now)
	plantAged(t, dir, "notes.txt", 10, 30*week, now)
	target := filepath.Join(t.TempDir(), "kept.png")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "cccccccccccccccc.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dddddddddddddddd.png"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-30 * week)
	_ = os.Chtimes(filepath.Join(dir, "dddddddddddddddd.png"), old, old)
	n, err := Sweep(dir, now)
	if err != nil || n != 2 {
		t.Fatalf("Sweep = %d, %v; want 2 removed", n, err)
	}
	// The store's lock (created by the sweep itself) is no stored name: it
	// stays.
	want := []string{lockName, "bbbbbbbbbbbbbbbb.png", "cccccccccccccccc.png", "dddddddddddddddd.png", "notes.txt"}
	if got := names(t, dir); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("left %q, want %q", got, want)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the symlink's target was touched: %v", err)
	}
}

// TestSweepByBudget: once the old files are gone, the oldest go first until
// what is left fits the budget (a small one, injected), and the newest stay.
func TestSweepByBudget(t *testing.T) {
	dir := madeDir(t)
	now := time.Now()
	plantAged(t, dir, "1111111111111111.png", 100, 8*24*time.Hour, now) // by age
	plantAged(t, dir, "2222222222222222.png", 100, 5*time.Hour, now)
	plantAged(t, dir, "3333333333333333.png", 100, 4*time.Hour, now)
	plantAged(t, dir, "4444444444444444.png", 100, 3*time.Hour, now)
	plantAged(t, dir, "5555555555555555.png", 100, 2*time.Hour, now)
	n, err := sweep(dir, now, 250)
	if err != nil || n != 3 {
		t.Fatalf("sweep = %d, %v; want 3 removed", n, err)
	}
	want := []string{lockName, "4444444444444444.png", "5555555555555555.png"}
	if got := names(t, dir); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("left %q, want %q", got, want)
	}
	// Within budget, nothing more goes.
	if n, err := sweep(dir, now, 250); err != nil || n != 0 {
		t.Fatalf("a second sweep = %d, %v", n, err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("the directory is gone: %v", err)
	}
}

// TestSweepLeavesAnUnsafeDirectoryAlone: a symlinked or widened directory is
// an error, and nothing in it is removed; a missing one is nothing to do.
func TestSweepLeavesAnUnsafeDirectoryAlone(t *testing.T) {
	now := time.Now()
	if n, err := Sweep(attachDir(t), now); n != 0 || err != nil {
		t.Fatalf("a missing directory: %d, %v", n, err)
	}
	real := madeDir(t)
	plantAged(t, real, "aaaaaaaaaaaaaaaa.png", 10, 30*24*time.Hour, now)
	link := filepath.Join(t.TempDir(), "attachments")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Sweep(link, now); !errors.Is(err, ErrUnsafeDir) {
		t.Fatalf("Sweep through a symlink = %v", err)
	}
	if err := os.Chmod(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Sweep(real, now); !errors.Is(err, ErrUnsafeDir) {
		t.Fatalf("Sweep of a 0755 directory = %v", err)
	}
	if len(names(t, real)) != 1 {
		t.Fatal("a refused sweep removed a file")
	}
}

// agedSave is Save of data, then the saved file made older than MaxAge.
func agedSave(t *testing.T, dir string, data []byte, now time.Time) string {
	t.Helper()
	path, err := Save(dir, data, MIMEPNG)
	if err != nil {
		t.Fatal(err)
	}
	old := now.Add(-MaxAge - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestASaveDuringASweepKeepsItsFile is r1 #12's interleaving (plan 033 C3r):
// a Sweep has condemned an old file and is about to remove it when another
// craze pastes the same image. That Save finds the store's lock held, waits
// for the sweep, and then — the file gone — writes it again: when both are
// done the chip's file is there. Without the lock the Save's dedupe would
// refresh the file and return, and the sweep would remove it under it.
func TestASaveDuringASweepKeepsItsFile(t *testing.T) {
	dir := madeDir(t)
	now := time.Now()
	data := []byte("the same screenshot, pasted again")
	path := agedSave(t, dir, data, now)
	busy := make(chan struct{}, 1)
	saved := make(chan error, 1)
	lockBusy = func() {
		select {
		case busy <- struct{}{}:
		default:
		}
	}
	sweepRemoving = func(string) {
		go func() {
			_, err := Save(dir, data, MIMEPNG)
			saved <- err
		}()
		select {
		case <-busy: // the Save waits for this sweep
		case err := <-saved: // the Save did not wait: it refreshed the file this sweep removes next
			saved <- err
		case <-time.After(5 * time.Second):
			t.Error("the Save neither waited nor finished")
		}
	}
	t.Cleanup(func() { lockBusy, sweepRemoving = func() {}, func(string) {} })
	if n, err := Sweep(dir, now); err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v; want the old file removed", n, err)
	}
	select {
	case err := <-saved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Save never finished")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the saved file is gone after the sweep: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || now.Sub(fi.ModTime()) > time.Minute {
		t.Fatalf("the saved file is not fresh: %v", err)
	}
}

// TestASweepKeepsAFileThatChanged: a file refreshed between the sweep's
// listing and its removal — by a Save that waited out the lock and went on
// without it — is stat'ed again and kept.
func TestASweepKeepsAFileThatChanged(t *testing.T) {
	dir := madeDir(t)
	now := time.Now()
	path := agedSave(t, dir, []byte("refreshed in the window"), now)
	sweepStatted = func(string) {
		if err := os.Chtimes(path, now, now); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { sweepStatted = func(string) {} })
	if n, err := Sweep(dir, now); err != nil || n != 0 {
		t.Fatalf("Sweep = %d, %v; want the refreshed file kept", n, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the refreshed file was removed: %v", err)
	}
}

// TestTheStoreLockIsBounded: with the store's lock held elsewhere, Sweep gives
// up quietly — nothing removed, no error — and Save goes on without it, each
// after its own short wait.
func TestTheStoreLockIsBounded(t *testing.T) {
	saveLockWait, sweepLockWait = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { saveLockWait, sweepLockWait = time.Second, time.Second })
	dir := madeDir(t)
	now := time.Now()
	old := agedSave(t, dir, []byte("old"), now)
	unlock, err := atomicfile.Lock(filepath.Join(dir, lockName))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if n, err := Sweep(dir, now); err != nil || n != 0 {
		t.Fatalf("Sweep under a held lock = %d, %v; want nothing, quietly", n, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("a sweep that gave up removed a file: %v", err)
	}
	path, err := Save(dir, []byte("new"), MIMEPNG)
	if err != nil {
		t.Fatalf("Save under a held lock: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("Save under a held lock wrote %q, %v", got, err)
	}
}
