package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNoTranscriptYet is WriteSegment's refusal before the transcript exists:
// a segment holds part of a conversation the file has, and the session's lock
// is taken with its file.
var ErrNoTranscriptYet = errors.New("store: the transcript is not written yet")

// WriteSegment writes data as the segment file name in the session's segment
// directory (SegmentDir, plan 028 §3.10): the directory 0700, the file 0600,
// as the transcript's own are. It writes a temporary file in the directory
// and renames it onto name, so the file is whole or absent, and a file already
// there — an orphan a crash left between a segment and its compaction entry —
// is replaced. It runs under the store's mutex while the store holds the
// transcript's lock, so no other writer of this session can race it.
//
// It writes no entry: the caller appends the compaction that names the file
// once this has returned, so an entry never names a file that is not there.
// A failure leaves the transcript untouched and the store usable.
func (s *Store) WriteSegment(name string, data []byte) error {
	if !plainFileName(name) {
		return fmt.Errorf("store: segment %q is not a file name", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	if s.f == nil {
		return ErrNoTranscriptYet
	}
	dir := SegmentDir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	tmp := filepath.Join(dir, "."+name+"."+randomEntryID()+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, filepath.Join(dir, name))
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: segment %s: %w", name, werr)
	}
	return nil
}
