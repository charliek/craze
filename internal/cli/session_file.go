package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/rundir"
)

// --session-file <path> (new --wait and send --wait) tells a caller the session
// before the command's result, which comes only as the turn ends. It is written
// atomically, 0600 as a whole result's file is (writeFull), twice:
//
//   - {session, short, hostPid, hostId} as soon as the host is known: a host this
//     command started when its ready line arrives, before anything dials it (a
//     reload's too); a session it found running, or a holder a reload met, once
//     attached, which is the first the command has heard from that host. Either
//     way before the prompt is sent, and failing that write means no prompt.
//   - the same with handle once the host has answered the prompt: the handle
//     craze wait --from takes. A failed rewrite leaves the first in place and says
//     so on stderr; the turn is under way and its result is unaffected.
//
// A file from an earlier run is removed as the command starts, once its flags and
// prompt have passed, so a reader never takes it for this one's; the path's only
// a file or a link, a directory there failing the first write instead. A session
// the command started whose prompt never reached it, which it then stopped, leaves
// no index row and nothing to name, so its file goes with it (promptJob.undo). A
// session that existed already, or that has had a prompt, keeps its file.

// sessionFileRecord is the file's content.
type sessionFileRecord struct {
	Session string `json:"session"`
	Short   string `json:"short"`
	HostPID int    `json:"hostPid"`
	HostID  string `json:"hostId"`
	Handle  string `json:"handle,omitempty"`
}

// sessionFile is one command's --session-file; a nil one is no flag, and every
// method does nothing.
type sessionFile struct {
	path  string
	rec   sessionFileRecord
	wrote bool
}

// newSessionFile is --session-file's value, nil for none.
func newSessionFile(path string) *sessionFile {
	if path == "" {
		return nil
	}
	return &sessionFile{path: path}
}

// clear removes what an earlier run left at the path: a file or a link, never a
// directory, which the first write then refuses.
func (f *sessionFile) clear() {
	if f == nil {
		return
	}
	if st, err := os.Lstat(f.path); err == nil && !st.IsDir() {
		_ = os.Remove(f.path)
	}
}

// host is the first write, for the host e names; a later call does nothing. Its
// failure is the command's: exit 1, kind config, the path being the caller's.
func (f *sessionFile) host(what string, e rundir.Entry) error {
	if f == nil || f.wrote {
		return nil
	}
	f.rec = sessionFileRecord{Session: e.CrazeSessionID, Short: psShort(e.CrazeSessionID), HostPID: e.PID, HostID: e.HostID}
	if err := f.write(); err != nil {
		return kindf(errkind.Config, "%s: --session-file: %s", what, sanitizeLine(err.Error()))
	}
	f.wrote = true
	return nil
}

// submitted is the second write, handle added; "" is a prompt with no handle to
// name yet, which leaves the first.
func (f *sessionFile) submitted(handle string) error {
	if f == nil || !f.wrote || handle == "" {
		return nil
	}
	f.rec.Handle = handle
	return f.write()
}

// remove takes the file this command wrote away.
func (f *sessionFile) remove() {
	if f != nil && f.wrote {
		_ = os.Remove(f.path)
		f.wrote = false
	}
}

func (f *sessionFile) write() error {
	b, err := json.Marshal(f.rec)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(f.path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("cannot write %s: %w", f.path, err)
	}
	return nil
}
