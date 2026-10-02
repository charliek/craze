package chatgptauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
)

// recordVersion is chatgpt.json's schema version; a file of any other is
// not one this craze can use (errCorrupt).
const recordVersion = 1

// maxFile bounds what is read of chatgpt.json, chatgpt-client.json and
// host-id: each is a few kilobytes at most.
const maxFile = 64 << 10

// lockWait bounds a wait for the lock (plan 033 §3.10: ≤ 60 s). A refresh
// holds it for one token request (refreshTimeout) and a write, a sign-in for
// two small writes, a sign-out for a revocation (revokeTimeout) and a
// removal, so a lock busy for longer is a holder stuck. A variable only so a
// test can hold the lock past a short one.
var lockWait = 60 * time.Second

// onLockBusy, when a test sets it, is called each time a lock taker here
// finds the lock held, just before it starts waiting for it: the place the
// forced schedules of the race tests hold a second taker, and learn that it
// is waiting. nil in production.
var onLockBusy func()

// errCorrupt is a token or registration file this package did not write — not
// JSON, another version, a field missing, not a regular file, too large. The
// sign-in it held cannot be used, so it reads as "sign in again"; nothing
// deletes it but a sign-out or the next sign-in, which replaces it.
var errCorrupt = fmt.Errorf("%w (its file is not one craze wrote)", ErrSignInAgain)

// record is chatgpt.json, the secret token record (plan 033 §3.10). Times are
// UTC. Incarnation is a random id per sign-in, which with the subject and the
// client id names one sign-in: a new sign-in installs a new one, and a
// process holding another's tokens adopts the file's rather than refreshing
// them (a different incarnation is never "older"). Generation counts the
// record's writes within an incarnation, from 1.
type record struct {
	Version           int       `json:"version"`
	ClientID          string    `json:"client_id"`
	Issuer            string    `json:"issuer"`
	Subject           string    `json:"subject"`
	Email             string    `json:"email"`
	Scopes            []string  `json:"scopes"`
	AccessToken       string    `json:"access_token"`
	AccessExpiresAt   time.Time `json:"access_expires_at"`
	RefreshToken      string    `json:"refresh_token"`
	EarliestRefreshAt time.Time `json:"earliest_refresh_at,omitzero"`
	IDToken           string    `json:"id_token,omitempty"`
	Incarnation       string    `json:"incarnation"`
	Generation        uint64    `json:"generation"`
	LastRefresh       time.Time `json:"last_refresh,omitzero"`
}

// version names one write of the record: its incarnation — the account,
// the registration and the sign-in's random id (plan 033 §3.10) — and its
// generation.
type version struct {
	subject, clientID, incarnation string
	generation                     uint64
}

func (r *record) version() version {
	return version{r.Subject, r.ClientID, r.Incarnation, r.Generation}
}

// valid says r is a record this package wrote.
func (r *record) valid() bool {
	return r.Version == recordVersion && r.ClientID != "" && r.Subject != "" &&
		r.AccessToken != "" && r.RefreshToken != "" && r.Incarnation != "" &&
		r.Generation > 0 && !r.AccessExpiresAt.IsZero()
}

// values is every secret r holds — the access, refresh and id tokens — for
// redaction (Values, StoredValues).
func (r *record) values() []string {
	var out []string
	for _, v := range []string{r.AccessToken, r.RefreshToken, r.IDToken} {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// Client is chatgpt-client.json: the registration, which is not secret and
// outlives a sign-out, so the next sign-in reuses the issued client id (the
// docs' client reuse) with the account's email as its login_hint. ClientID is
// "" once the server refused the registration (invalid_client), and the next
// sign-in registers anew. NoticeShown records the one-time plan-usage notice
// (plan 033 §3.13).
type Client struct {
	ClientID    string `json:"client_id"`
	Subject     string `json:"subject"`
	Email       string `json:"email"`
	PlanUsage   bool   `json:"plan_usage"`
	NoticeShown bool   `json:"notice_shown"`
}

// readFile reads path, a regular file of at most maxFile bytes, without
// following a symlink, and answers its stat with it. A file that is not
// there is fs.ErrNotExist; one that is not a regular file, or is larger, is
// errCorrupt.
func readFile(path string) ([]byte, os.FileInfo, error) { return readFileMax(path, maxFile) }

// readFileMax is readFile with its own bound.
func readFileMax(path string, max int64) ([]byte, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
		if errors.Is(err, syscall.ELOOP) {
			return nil, nil, errCorrupt
		}
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return nil, nil, errCorrupt
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(b)) > max {
		return nil, nil, errCorrupt
	}
	return b, info, nil
}

// readRecord reads dir's token record and its stat. A missing file is
// fs.ErrNotExist; one this package did not write is errCorrupt.
func readRecord(dir string) (*record, os.FileInfo, error) {
	b, info, err := readFile(TokenFile(dir))
	if err != nil {
		return nil, nil, err
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil || !r.valid() {
		return nil, nil, errCorrupt
	}
	return &r, info, nil
}

// writeSync is the token record's and the registration's write: a seam for
// the test of a write that fails after a rotation, atomicfile's durable
// write in production.
var writeSync = atomicfile.WriteSyncChecked

// errFileGone is a write's last check finding the token file removed: a
// sign-out (or a person's rm) that a write must never undo.
var errFileGone = errors.New("chatgptauth: the token file was removed")

// writeRecord replaces dir's token record with r, durably, at 0600. With
// mustExist, the write is refused (errFileGone) if the file is not there
// just before the rename: a refresh never recreates a deleted file.
func writeRecord(dir string, r *record, mustExist bool) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	var check func() error
	if mustExist {
		check = func() error {
			if _, err := os.Lstat(TokenFile(dir)); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return errFileGone
				}
				return err
			}
			return nil
		}
	}
	return writeSync(TokenFile(dir), append(b, '\n'), 0o600, check)
}

// removeRecord deletes dir's token record, durably. A record already gone is
// no error.
func removeRecord(dir string) error { return atomicfile.RemoveSync(TokenFile(dir)) }

// ReadClient is dir's registration. No file is the zero Client and no error:
// craze has never been registered here.
func ReadClient(dir string) (Client, error) {
	b, _, err := readFile(ClientFile(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Client{}, nil
		}
		return Client{}, err
	}
	var c Client
	if err := json.Unmarshal(b, &c); err != nil {
		return Client{}, errCorrupt
	}
	return c, nil
}

// writeClient replaces dir's registration, durably, at 0600: a lost client
// id would make the next sign-in register craze a second time.
func writeClient(dir string, c Client) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeSync(ClientFile(dir), append(b, '\n'), 0o600, nil)
}

// ensureAuthDir makes dir's sign-in directory 0700, creating it (and dir)
// when missing and tightening an existing one. A symlink there, or anything
// that is not a directory, is refused: the credentials go only into a
// directory craze made.
func ensureAuthDir(dir string) error {
	ad := AuthDir(dir)
	info, err := os.Lstat(ad)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(ad, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(ad)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("chatgptauth: %s is not a directory", ad)
	}
	if info.Mode().Perm() != 0o700 {
		return os.Chmod(ad, 0o700)
	}
	return nil
}

// lock takes dir's lock: at once when it is free, else — after onLockBusy —
// waiting for it within lockWait, giving up when ctx is done
// (atomicfile.LockContext). The returned unlock is always non-nil.
func lock(ctx context.Context, dir string) (func(), error) {
	if err := ensureAuthDir(dir); err != nil {
		return func() {}, err
	}
	path := lockFile(dir)
	unlock, err := atomicfile.LockWithin(path, 0)
	if !errors.Is(err, atomicfile.ErrLockBusy) {
		return unlock, err
	}
	if onLockBusy != nil {
		onLockBusy()
	}
	return atomicfile.LockContext(ctx, path, lockWait)
}

// hostID is this host's ext_agent_host_id, made once — a urn:uuid of a
// version-4 UUID, under the lock — and kept for good, sign-outs included
// (the docs: choose and persist it before the first sign-in). A file that
// holds anything else is an error naming it, never replaced: a new id would
// be a new host to the server.
func hostID(ctx context.Context, dir string) (string, error) {
	if id, err := readHostID(dir); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	unlock, err := lock(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("chatgptauth: taking the sign-in lock: %w", err)
	}
	defer unlock()
	if id, err := readHostID(dir); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	id := "urn:uuid:" + uuid4()
	if err := atomicfile.WriteSync(HostIDFile(dir), []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// readHostID is host-id's value, fs.ErrNotExist when there is none.
func readHostID(dir string) (string, error) {
	b, _, err := readFile(HostIDFile(dir))
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if !validHostID(id) {
		return "", fmt.Errorf("chatgptauth: %s does not hold a urn:uuid host id; remove it to make a new one", HostIDFile(dir))
	}
	return id, nil
}

// validHostID says id is urn:uuid: and a UUID's 8-4-4-4-12 hex form.
func validHostID(id string) bool {
	u, ok := strings.CutPrefix(id, "urn:uuid:")
	if !ok || len(u) != 36 {
		return false
	}
	for i := 0; i < len(u); i++ {
		switch i {
		case 8, 13, 18, 23:
			if u[i] != '-' {
				return false
			}
		default:
			if !isHex(u[i]) {
				return false
			}
		}
	}
	return true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// uuid4 is a random version-4 UUID in its hyphenated form.
func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// randomString is n random bytes, base64url without padding: the state, the
// nonce, the PKCE verifier and an incarnation.
func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64URL(b)
}

// Status is what craze auth list and /connect say of the sign-in, read
// without a secret: whether craze is registered, whether a token file is
// there (a non-empty regular file — the funding rule, plan 033 §3.11),
// whether plan usage was granted, and the account's email.
type Status struct {
	Registered bool
	SignedIn   bool
	PlanUsage  bool
	Email      string
}

// ReadStatus is dir's Status. It reads chatgpt-client.json and stats
// chatgpt.json, and never reads a token.
func ReadStatus(dir string) (Status, error) {
	c, err := ReadClient(dir)
	if err != nil {
		return Status{}, err
	}
	st := Status{Registered: c.ClientID != "", PlanUsage: c.PlanUsage, Email: c.Email}
	if info, err := os.Lstat(TokenFile(dir)); err == nil {
		st.SignedIn = info.Mode().IsRegular() && info.Size() > 0
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Status{}, err
	}
	return st, nil
}

// StoredValues is every secret in dir's token file — the access, refresh and
// id tokens — for a session's turn-start look (plan 033 §3.12, P19), which
// hands them to the redactor. No file is no values and no error; it never
// touches the network or the lock.
func StoredValues(dir string) ([]string, error) {
	r, _, err := readRecord(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return r.values(), nil
}

// MarkNoticeShown records that the one-time plan-usage notice was shown for
// dir's registration (plan 033 §3.13). With no registration it does nothing.
func MarkNoticeShown(ctx context.Context, dir string) error {
	unlock, err := lock(ctx, dir)
	if err != nil {
		return fmt.Errorf("chatgptauth: taking the sign-in lock: %w", err)
	}
	defer unlock()
	c, err := ReadClient(dir)
	if err != nil || c.ClientID == "" || c.NoticeShown {
		return err
	}
	c.NoticeShown = true
	return writeClient(dir, c)
}
