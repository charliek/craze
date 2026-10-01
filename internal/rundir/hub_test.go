package rundir

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/protocol"
)

// lockHub is LockHub for a fresh hub id, released at cleanup.
func lockHub(t *testing.T, env Env) *HubLock {
	t.Helper()
	l, err := LockHub(env, NewHubID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	return l
}

// hubHeld is LockHub expected to be refused as held.
func hubHeld(t *testing.T, env Env) *HubHeldError {
	t.Helper()
	l, err := LockHub(env, NewHubID())
	if err == nil {
		_ = l.Release()
		t.Fatal("LockHub succeeded; want the hub lock held")
	}
	var held *HubHeldError
	if !errors.As(err, &held) {
		t.Fatalf("LockHub = %v; want a *HubHeldError", err)
	}
	return held
}

// hubsDir is env's hubs directory.
func hubsDir(env Env) string { return filepath.Join(env.Home, cacheName, crazeName, hubsName) }

func namespaceOf(t *testing.T, env Env) string {
	t.Helper()
	ns, err := Namespace(env.CrazeDir)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

// Two hubs race for one namespace: exactly one takes the lock, and the other
// is refused naming the winner — or nobody, when it read the lock in the
// instant between the winner's flock and its holder line. Once the winner's
// line is down a third reads it; once released, the lock is free again and
// its file is still there.
func TestTwoHubLockersOneWins(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	for range 20 {
		type result struct {
			l   *HubLock
			err error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		for range 2 {
			go func() {
				<-start
				l, err := LockHub(env, NewHubID())
				results <- result{l, err}
			}()
		}
		close(start)
		var won []*HubLock
		var lost []*HubHeldError
		for range 2 {
			r := <-results
			var held *HubHeldError
			switch {
			case r.err == nil:
				won = append(won, r.l)
			case errors.As(r.err, &held):
				lost = append(lost, held)
			default:
				t.Fatalf("LockHub: %v", r.err)
			}
		}
		if len(won) != 1 || len(lost) != 1 {
			for _, l := range won {
				_ = l.Release()
			}
			t.Fatalf("%d hubs took the lock and %d were refused; want one of each", len(won), len(lost))
		}
		w, h := won[0], lost[0]
		if blank, named := (h.PID == 0 && h.HubID == ""), (h.PID == os.Getpid() && h.HubID == w.ID()); !blank && !named {
			t.Fatalf("the loser read holder pid %d hub %q; want nobody or the winner %d %s", h.PID, h.HubID, os.Getpid(), w.ID())
		}
		if third := hubHeld(t, env); third.PID != os.Getpid() || third.HubID != w.ID() || third.NS != namespaceOf(t, env) {
			t.Fatalf("a third locker read %+v; want the winner %d %s", third, os.Getpid(), w.ID())
		}
		if err := w.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if !exists(t, filepath.Join(hubsDir(env), namespaceOf(t, env)+".lock")) {
		t.Fatal("releasing the hub lock unlinked it")
	}
	lockHub(t, env)
}

// TestAHubLockLoserReadsTheHolderLine is not parallel: it replaces
// hubLockTaken, which every LockHub calls. The winner is held in the instant
// between its flock and its holder line, over the line a hub that died
// holding the lock left: a loser then reads nobody — not the dead hub, and
// not a half-written line. Once the winner writes its line, a loser reads it.
func TestAHubLockLoserReadsTheHolderLine(t *testing.T) {
	if err := syscall.Kill(noSuchPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("setup: kill(%d, 0) = %v, want ESRCH", noSuchPID, err)
	}
	env := testEnv(t)
	cacheSubdir(t, env, hubsName)
	writeFile(t, filepath.Join(hubsDir(env), namespaceOf(t, env)+".lock"), strconv.Itoa(noSuchPID)+" 0123456789ab\n")
	paused, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hubLockTaken = func() { once.Do(func() { close(paused); <-resume }) }
	t.Cleanup(func() { hubLockTaken = func() {} })

	winnerID := NewHubID()
	type result struct {
		l   *HubLock
		err error
	}
	done := make(chan result, 1)
	go func() {
		l, err := LockHub(env, winnerID)
		done <- result{l, err}
	}()
	select {
	case <-paused:
	case <-time.After(10 * time.Second):
		t.Fatal("the winner never took the lock")
	}
	before := hubHeld(t, env)
	close(resume)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { _ = r.l.Release() })
	if before.PID != 0 || before.HubID != "" || !strings.Contains(before.Error(), "pid ?") {
		t.Fatalf("before the holder line: %+v (%q); want nobody", before, before.Error())
	}
	after := hubHeld(t, env)
	if after.PID != os.Getpid() || after.HubID != winnerID {
		t.Fatalf("after the holder line: %+v; want pid %d hub %s", after, os.Getpid(), winnerID)
	}
	if msg := after.Error(); !strings.Contains(msg, "pid "+strconv.Itoa(os.Getpid())) || !strings.Contains(msg, winnerID) {
		t.Fatalf("the refusal %q does not name its holder", msg)
	}
}

func TestAReleasedHubLockLeavesItsFile(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	l := lockHub(t, env)
	want := filepath.Join(mustCanonical(t, env.Home), cacheName, crazeName, hubsName, namespaceOf(t, env)+".lock")
	if l.Path() != want || l.Namespace() != namespaceOf(t, env) || !ValidHostID(l.ID()) {
		t.Fatalf("the lock: %s, ns %s, id %s; want %s", l.Path(), l.Namespace(), l.ID(), want)
	}
	if b, err := os.ReadFile(l.Path()); err != nil || string(b) != strconv.Itoa(os.Getpid())+" "+l.ID()+"\n" {
		t.Fatalf("the holder line: %q, %v", b, err)
	}
	if got := perm(t, l.Path()); got != 0o600 {
		t.Fatalf("the lock file has mode %04o, want 0600", got)
	}
	if got := perm(t, hubsDir(env)); got != 0o700 {
		t.Fatalf("the hubs directory has mode %04o, want 0700", got)
	}
	for range 2 {
		if err := l.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := os.ReadFile(l.Path()); err != nil || len(b) != 0 {
		t.Fatalf("a released hub lock's file holds %q (%v); want it there, its line cleared", b, err)
	}
	if _, err := l.WriteRecord(HubRecord{Socket: "/s"}); !errors.Is(err, ErrReleased) {
		t.Fatalf("WriteRecord after Release = %v, want ErrReleased", err)
	}
	lockHub(t, env)
}

func TestLockHubRefusesABadHubID(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	if l, err := LockHub(env, "../x"); err == nil {
		_ = l.Release()
		t.Fatal("LockHub took a bad hub id")
	}
	if exists(t, filepath.Join(env.Home, cacheName)) {
		t.Fatal("a refused hub id built the cache tree")
	}
}

// The hubs tree takes the cache tree's rules, as the registry does: on the
// owner's own kind of box — umask 002 under user-private groups, a 0775 home
// and ~/.cache — a hub takes its lock, writes its record and has it read back
// (the residual this accepts: hub.go's doc). Still the euid's own only: a
// 0775 ~/.cache of another owner is refused, as it is for the registry.
func TestTheHubsTreeAcceptsAGroupWritableDirectoryOfTheUsersOwn(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	chmod(t, env.Home, 0o775)
	mkdir(t, filepath.Join(env.Home, cacheName), 0o775)
	bind(t, env) // the registry takes it
	l := lockHub(t, env)
	w := hubRecordFor(t, env, l)
	if r, _, err := ReadHubRecord(env); err != nil || r.HubID != w.HubID {
		t.Fatalf("ReadHubRecord under a 0775 ~/.cache = %+v, %v; want the record %s", r, err, w.HubID)
	}
	other := env
	other.EUID = os.Geteuid() + 1
	if l, err := LockHub(other, NewHubID()); err == nil {
		_ = l.Release()
		t.Fatal("LockHub took a hubs tree under a 0775 ~/.cache of another owner")
	}
}

// TestLockHubRefusesALockReplacedAtItsName is not parallel: it replaces
// hubLockTaken. A lock file replaced at its name between its open and its
// flock — what a second hub would see after the first's tree was renamed
// aside and remade — is nobody's singleton: LockHub refuses it, leaves the
// file now at the name, and a hub that opens that file takes it.
func TestLockHubRefusesALockReplacedAtItsName(t *testing.T) {
	env := testEnv(t)
	lockPath := filepath.Join(cacheSubdir(t, env, hubsName), namespaceOf(t, env)+".lock")
	var once sync.Once
	hubLockTaken = func() {
		once.Do(func() {
			next := lockPath + ".next"
			writeFile(t, next, "")
			if err := os.Rename(next, lockPath); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(func() { hubLockTaken = func() {} })
	l, err := LockHub(env, NewHubID())
	if err == nil {
		_ = l.Release()
		t.Fatal("LockHub took a lock no longer at its name")
	}
	var held *HubHeldError
	if errors.As(err, &held) || !strings.Contains(err.Error(), "is no longer the hub lock just taken") {
		t.Fatalf("LockHub = %v; want the replaced lock refused", err)
	}
	if !exists(t, lockPath) {
		t.Fatal("the refusal unlinked the lock now at the name")
	}
	lockHub(t, env)
}

// hubRecordFor writes a record through l with the socket HubSocket gives.
func hubRecordFor(t *testing.T, env Env, l *HubLock) HubRecord {
	t.Helper()
	sock, err := HubSocket(env)
	if err != nil {
		t.Fatal(err)
	}
	r, err := l.WriteRecord(HubRecord{Socket: sock, CrazeVersion: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The record is written atomically, 0600, with WriteRecord's members filled
// from this process whatever the caller put there, and read back with the
// identity of the very file written.
func TestTheHubRecordRoundTrips(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	l := lockHub(t, env)
	sock, err := HubSocket(env)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	w, err := l.WriteRecord(HubRecord{Protocol: 99, HubID: "ffffffffffff", NS: "other", PID: 1, StartToken: "1",
		Socket: sock, CrazeVersion: "1.2.3", StartedAt: started})
	if err != nil {
		t.Fatal(err)
	}
	token, err := StartToken(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	want := HubRecord{Protocol: protocol.ProtocolVersion, HubID: l.ID(), NS: namespaceOf(t, env), PID: os.Getpid(),
		StartToken: token, Socket: sock, CrazeVersion: "1.2.3", StartedAt: started}
	if w != want {
		t.Fatalf("WriteRecord answered %+v, want %+v", w, want)
	}
	got, id, err := ReadHubRecord(env)
	if err != nil {
		t.Fatal(err)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Fatalf("read startedAt %v, want %v", got.StartedAt, want.StartedAt)
	}
	got.StartedAt = want.StartedAt
	if got != want {
		t.Fatalf("ReadHubRecord = %+v, want %+v", got, want)
	}
	fi, err := os.Lstat(l.RecordPath())
	if err != nil {
		t.Fatal(err)
	}
	if idOf(fi) != id {
		t.Fatalf("ReadHubRecord's identity %+v is not the file's %+v", id, idOf(fi))
	}
	if p := permOf(fi); p != 0o600 {
		t.Fatalf("the record has mode %04o, want 0600", p)
	}
	if l.RecordPath() != filepath.Join(filepath.Dir(l.Path()), namespaceOf(t, env)+".json") {
		t.Fatalf("the record is at %s", l.RecordPath())
	}
	ns := namespaceOf(t, env)
	if names := dirNames(t, hubsDir(env)); !slices.Equal(names, []string{ns + ".json", ns + ".lock"}) {
		t.Fatalf("the hubs directory holds %v; want the record and the lock, no temporary", names)
	}
	b, err := os.ReadFile(l.RecordPath())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	members := []string{"protocol", "hubId", "ns", "pid", "startToken", "socket", "crazeVersion", "startedAt"}
	if len(m) != len(members) {
		t.Fatalf("the record has members %v, want %v", m, members)
	}
	for _, k := range members {
		if _, ok := m[k]; !ok {
			t.Errorf("the record has no %q", k)
		}
	}
	if _, err := l.WriteRecord(HubRecord{Socket: "relative"}); err == nil {
		t.Fatal("WriteRecord took a relative socket path")
	}
}

// A record put in this hub's record's place — a newer hub's, after this one
// lost it — is not this hub's to remove: RemoveRecord and Release leave it,
// and RecordLost names the replacement.
func TestAReplacedHubRecordIsNotRemovedByItsOldOwner(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	l, err := LockHub(env, NewHubID())
	if err != nil {
		t.Fatal(err)
	}
	hubRecordFor(t, env, l)
	p := l.RecordPath()
	aside := filepath.Join(filepath.Dir(p), "replacement")
	const newer = `{"hubId":"newer"}`
	writeFile(t, aside, newer)
	if err := os.Rename(aside, p); err != nil {
		t.Fatal(err)
	}
	if why := l.RecordLost(); !strings.Contains(why, "is another file now") {
		t.Fatalf("RecordLost = %q; want the replacement named", why)
	}
	if err := l.RemoveRecord(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != newer {
		t.Fatalf("after RemoveRecord the record path holds %q (%v); want the replacement left", b, err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != newer {
		t.Fatalf("after Release the record path holds %q (%v); want the replacement left", b, err)
	}
}

// A hub's own record is removed by its owner — by RemoveRecord, or by Release
// before the lock is let go — and one removed by anyone else is lost.
func TestTheHubRecordIsRemovedByItsOwner(t *testing.T) {
	t.Parallel()
	for name, remove := range map[string]func(*HubLock) error{
		"RemoveRecord": (*HubLock).RemoveRecord,
		"Release":      (*HubLock).Release,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			l := lockHub(t, env)
			hubRecordFor(t, env, l)
			if why := l.RecordLost(); why != "" {
				t.Fatalf("RecordLost = %q for the record just written", why)
			}
			if err := remove(l); err != nil {
				t.Fatal(err)
			}
			if exists(t, l.RecordPath()) {
				t.Fatalf("%s left the hub's own record", name)
			}
			if _, _, err := ReadHubRecord(env); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("ReadHubRecord after %s = %v, want fs.ErrNotExist", name, err)
			}
		})
	}
	t.Run("lost", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		l := lockHub(t, env)
		hubRecordFor(t, env, l)
		if err := os.Remove(l.RecordPath()); err != nil {
			t.Fatal(err)
		}
		if why := l.RecordLost(); !strings.Contains(why, "is gone") {
			t.Fatalf("RecordLost = %q; want the record gone", why)
		}
	})
}

func TestReadHubRecordWithNoTreeCreatesNothing(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	if _, _, err := ReadHubRecord(env); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadHubRecord = %v, want fs.ErrNotExist", err)
	}
	if exists(t, filepath.Join(env.Home, cacheName)) {
		t.Fatal("ReadHubRecord built the cache tree")
	}
}

func TestReadHubRecordRefusesARecordOfNoHub(t *testing.T) {
	t.Parallel()
	good := func(env Env) HubRecord {
		return HubRecord{HubID: "0123456789ab", NS: namespaceOf(t, env), PID: 1, Socket: "/s"}
	}
	for name, content := range map[string]func(Env) string{
		"not JSON":          func(Env) string { return "{" },
		"no hub id":         func(env Env) string { r := good(env); r.HubID = ""; return marshal(t, r) },
		"no pid":            func(env Env) string { r := good(env); r.PID = 0; return marshal(t, r) },
		"another namespace": func(env Env) string { r := good(env); r.NS = "ffffffff"; return marshal(t, r) },
		"oversized":         func(Env) string { return strings.Repeat(" ", hubRecordMax+1) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			cacheSubdir(t, env, hubsName)
			writeFile(t, filepath.Join(hubsDir(env), namespaceOf(t, env)+".json"), content(env))
			if r, _, err := ReadHubRecord(env); err == nil {
				t.Fatalf("ReadHubRecord read %+v", r)
			}
		})
	}
	t.Run("the good one", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		cacheSubdir(t, env, hubsName)
		writeFile(t, filepath.Join(hubsDir(env), namespaceOf(t, env)+".json"), marshal(t, good(env)))
		if _, _, err := ReadHubRecord(env); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a symlink", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		cacheSubdir(t, env, hubsName)
		target := filepath.Join(shortDir(t), "record.json")
		writeFile(t, target, marshal(t, good(env)))
		symlink(t, target, filepath.Join(hubsDir(env), namespaceOf(t, env)+".json"))
		if r, _, err := ReadHubRecord(env); err == nil {
			t.Fatalf("ReadHubRecord followed a symlinked record to %+v", r)
		}
	})
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tokenParts is a start token's four fields: version, boot, PID namespace and
// start time.
func tokenParts(t *testing.T, token string) []string {
	t.Helper()
	f := strings.Split(token, "/")
	if len(f) != 4 || f[0] != "st1" || f[1] == "" || f[2] == "" {
		t.Fatalf("StartToken = %q; want st1/<boot>/<pid namespace>/<start>", token)
	}
	if _, err := strconv.ParseUint(f[3], 10, 64); err != nil {
		t.Fatalf("StartToken = %q, whose start %q is not a decimal start time", token, f[3])
	}
	return f
}

// A start token tells the process it was taken from from any other that has
// its pid: this process carries its own; the same pid with another start —
// a later process given a reused pid — does not, nor the same pid and start
// in another boot or another PID namespace, nor a token of another shape (a
// bare start time, as plan 032 first wrote it; another version); nor does a
// reaped child's pid, nor an empty token or a pid that cannot be one.
//
// It is not parallel because it starts a child: a child forked holds a copy
// of every descriptor this process has until it execs, and one forked while a
// parallel test kills a host (Host.die closes its listener) keeps that host's
// socket listening — a sweep then finds it live (TestStartTokens beside
// TestTheSweepRemovesAHostKilledMidBind failed 91 of 500 runs).
func TestStartTokens(t *testing.T) {
	self, err := StartToken(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	f := tokenParts(t, self)
	if !CarriesStartToken(os.Getpid(), self) {
		t.Fatal("this process does not carry its own start token")
	}
	id, err := ProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	join := func(f ...string) string { return strings.Join(f, "/") }
	for name, token := range map[string]string{
		"a reused pid":             join(f[0], f[1], f[2], strconv.FormatUint(id.Start+1, 10)),
		"another boot":             join(f[0], "00000000-0000-0000-0000-000000000000", f[2], f[3]),
		"another PID namespace":    join(f[0], f[1], "1", f[3]),
		"a bare start time":        f[3],
		"another version":          join("st2", f[1], f[2], f[3]),
		"an empty token":           "",
		"the start with its boot":  join(f[1], f[3]),
		"the token with no prefix": join(f[1], f[2], f[3]),
	} {
		if CarriesStartToken(os.Getpid(), token) {
			t.Errorf("%s: this process carries %q", name, token)
		}
	}
	for _, pid := range []int{0, -1} {
		if CarriesStartToken(pid, self) {
			t.Fatalf("CarriesStartToken(%d, %q) = true", pid, self)
		}
	}

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	child, err := StartToken(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if !CarriesStartToken(cmd.Process.Pid, child) {
		t.Fatal("a running child does not carry its own start token")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	reaped = true
	if CarriesStartToken(cmd.Process.Pid, child) {
		t.Fatal("a reaped child's pid carries its token")
	}
	if _, err := StartToken(cmd.Process.Pid); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("StartToken of a reaped child = %v, want ErrNoProcess", err)
	}
}

// TestARecordsTokenOutlivesItsBoot is not parallel: it replaces tokenScope,
// which every StartToken reads. A hub's record outlives a reboot in ~/.cache,
// and a Linux start time counts ticks since boot, so after a reboot another
// process may have the hub's pid and its very start time. Read in another
// boot — or from another PID namespace, where the pid names another process
// — the token is not carried, and neither is any token while the scope cannot
// be read. Back in its own boot and namespace, it is.
func TestARecordsTokenOutlivesItsBoot(t *testing.T) {
	self, err := StartToken(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	f := tokenParts(t, self)
	t.Cleanup(func() { tokenScope = processTokenScope })
	for name, scope := range map[string]func() (string, string, error){
		"another boot":          func() (string, string, error) { return "11111111-2222-3333-4444-555555555555", f[2], nil },
		"another PID namespace": func() (string, string, error) { return f[1], "4026531999", nil },
		"no scope":              func() (string, string, error) { return "", "", errors.New("no /proc") },
		"no boot session UUID (macOS)": func() (string, string, error) {
			return bootSessionScope(func(string) (string, error) { return "", errors.New("unknown sysctl") })
		},
	} {
		tokenScope = scope
		if CarriesStartToken(os.Getpid(), self) {
			t.Errorf("%s: this process carries the token %q", name, self)
		}
	}
	tokenScope = func() (string, string, error) { return "", "", errors.New("no /proc") }
	if tok, err := StartToken(os.Getpid()); err == nil {
		t.Errorf("StartToken with no scope = %q; want an error", tok)
	}
	tokenScope = processTokenScope
	if !CarriesStartToken(os.Getpid(), self) {
		t.Fatal("back in its own boot and namespace, this process does not carry its token")
	}
}

// macOS's scope is its boot session UUID and nothing else: read through a
// fake sysctl, a UUID scopes a token with no PID namespace; a UUID that cannot
// be read, or that is empty or would break the token's fields, is no scope —
// no fallback stands in. kern.boottime is never asked for, so a wall-clock
// step, which XNU applies to the boot time, cannot change a live hub's scope.
func TestBootSessionScope(t *testing.T) {
	t.Parallel()
	values := map[string]string{"kern.bootsessionuuid": "6C1B2A4E-0000-4000-8000-0123456789AB", "kern.boottime": "1727700000"}
	var asked []string
	sysctl := func(name string) (string, error) {
		asked = append(asked, name)
		v, ok := values[name]
		if !ok {
			return "", errors.New("unknown sysctl " + name)
		}
		return v, nil
	}
	boot, ns, err := bootSessionScope(sysctl)
	if err != nil || boot != values["kern.bootsessionuuid"] || ns != "-" {
		t.Fatalf("bootSessionScope = %q, %q, %v; want the UUID and no namespace", boot, ns, err)
	}
	values["kern.boottime"] = "1727700001" // a wall-clock step
	if again, _, err := bootSessionScope(sysctl); err != nil || again != boot {
		t.Fatalf("after a clock step the scope is %q, %v; want %q", again, err, boot)
	}
	for _, name := range asked {
		if name != "kern.bootsessionuuid" {
			t.Fatalf("bootSessionScope asked for %s", name)
		}
	}
	for name, uuid := range map[string]string{"empty": "", "unknown": "unknown", "a slash": "a/b"} {
		values["kern.bootsessionuuid"] = uuid
		if boot, _, err := bootSessionScope(sysctl); err == nil {
			t.Errorf("%s UUID: scope %q; want none", name, boot)
		}
	}
	delete(values, "kern.bootsessionuuid")
	if boot, _, err := bootSessionScope(sysctl); err == nil {
		t.Fatalf("no UUID: scope %q; want none, no fallback", boot)
	}
}

// A boot session UUID must have a UUID's shape — 8-4-4-4-12 hex digits,
// either case — and nothing else does.
func TestValidUUID(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		"6c1b2a4e-0000-4000-8000-0123456789ab":   true,
		"6C1B2A4E-0000-4000-8000-0123456789AB":   true,
		"6c1B2a4E-abCD-4000-8000-0123456789Ab":   true,
		"":                                       false,
		"unknown":                                false,
		"6c1b2a4e-0000-4000-8000-0123456789a":    false, // 35
		"6c1b2a4e-0000-4000-8000-0123456789abc":  false, // 37
		"6c1b2a4e00000-4000-8000-0123456789ab":   false, // a hyphen missing
		"6c1b2a4e-0000-4000-8000-0123456789ag":   false, // not hex
		"6c1b2a4e-0000-4000-8000\t0123456789ab":  false, // a tab for a hyphen
		"6c1b2a4e-0000-4000-8000-01234567/9ab":   false,
		"{6c1b2a4e-0000-4000-8000-0123456789ab}": false,
		"6c1b2a4e-0000-4000-80000123456789ab-":   false, // a hyphen moved
	} {
		if got := validUUID(s); got != want {
			t.Errorf("validUUID(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestAHubWithoutAScopeRegistersWithNoToken is not parallel: it replaces
// tokenScope, which every StartToken reads. A hub whose start token has no
// trustworthy scope (no NStgid check on Linux, no boot session UUID on macOS)
// still writes its record — with an empty token, which a reader reads back
// as written — and nobody carries an empty token, so that hub is never
// terminated on its record's word: not under the failing scope, and not once
// a scope can be read again.
func TestAHubWithoutAScopeRegistersWithNoToken(t *testing.T) {
	env := testEnv(t)
	l := lockHub(t, env)
	tokenScope = func() (string, string, error) { return "", "", errors.New("no NStgid line") }
	t.Cleanup(func() { tokenScope = processTokenScope })
	w := hubRecordFor(t, env, l)
	if w.StartToken != "" {
		t.Fatalf("WriteRecord with no scope wrote the token %q; want none", w.StartToken)
	}
	r, _, err := ReadHubRecord(env)
	if err != nil || r.HubID != l.ID() || r.PID != os.Getpid() || r.StartToken != "" {
		t.Fatalf("ReadHubRecord = %+v, %v; want this hub's record with no token", r, err)
	}
	if CarriesStartToken(r.PID, r.StartToken) {
		t.Fatal("the hub carries its empty token with no scope")
	}
	tokenScope = processTokenScope
	if CarriesStartToken(r.PID, r.StartToken) {
		t.Fatal("the hub carries its empty token once a scope can be read")
	}
}

// The hub's socket is beside its namespace's hosts', in the base they choose.
func TestHubSocketIsInTheHostsNamespaceDirectory(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	sock, err := HubSocket(env)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(h.Socket()), "hub.sock"); sock != want {
		t.Fatalf("HubSocket = %s, want %s", sock, want)
	}
	if exists(t, sock) {
		t.Fatal("HubSocket made a file")
	}
}

// A base is measured for the longest socket its namespace holds, a host's,
// even when the hub asks: a base where hub.sock would fit but a host's
// socket would not is refused, so the hub never chooses a base its hosts
// cannot.
func TestTheHubsBaseIsMeasuredForAHostSocket(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	parent := env.CrazeRuntimeDir
	const baseLen = 78 // <base>/<8>/hub.sock is 96 bytes; <base>/<8>/<12>.sock is 105
	name := strings.Repeat("x", baseLen-len(mustCanonical(t, parent))-1)
	env.CrazeRuntimeDir = filepath.Join(parent, name)
	if n := len(filepath.Join(mustCanonical(t, parent), name, "01234567", "hub.sock")); n > SocketPathMax {
		t.Fatalf("setup: the hub's socket path is %d bytes; it must fit", n)
	}
	_, err := HubSocket(env)
	if err == nil || !strings.Contains(err.Error(), "over the 100-byte limit") {
		t.Fatalf("HubSocket = %v; want the base refused for a host's socket path", err)
	}
	if names, _ := os.ReadDir(parent); len(names) != 0 {
		t.Fatalf("%s holds %d entries after the refusal; nothing may be created", parent, len(names))
	}
}

// listening binds a socket at p that accepts connections, closed (and
// unlinked) at cleanup.
func listening(t *testing.T, p string) {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

func TestClearStaleSocket(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setup func(t *testing.T, p string)
		fate  SocketFate
		kept  bool
	}{
		"absent":       {setup: func(*testing.T, string) {}, fate: SocketAbsent},
		"refusing":     {setup: listenAt, fate: SocketRemoved},
		"listening":    {setup: listening, fate: SocketLive, kept: true},
		"a plain file": {setup: func(t *testing.T, p string) { writeFile(t, p, "x") }, fate: SocketNotSocket, kept: true},
		"a directory":  {setup: func(t *testing.T, p string) { mkdir(t, p, 0o700) }, fate: SocketNotSocket, kept: true},
		"a symlink to a refusing socket": {setup: func(t *testing.T, p string) {
			target := filepath.Join(filepath.Dir(p), "target.sock")
			listenAt(t, target)
			symlink(t, target, p)
		}, fate: SocketNotSocket, kept: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := filepath.Join(shortDir(t), "s.sock")
			tc.setup(t, p)
			fate, err := ClearStaleSocket(p)
			if err != nil || fate != tc.fate {
				t.Fatalf("ClearStaleSocket = %v, %v; want %v", fate, err, tc.fate)
			}
			if _, err := os.Lstat(p); (err == nil) != tc.kept {
				t.Fatalf("after ClearStaleSocket the path is there: %v; want %v", err == nil, tc.kept)
			}
		})
	}
	t.Run("the target of a symlink", func(t *testing.T) {
		t.Parallel()
		d := shortDir(t)
		target := filepath.Join(d, "target.sock")
		listenAt(t, target)
		symlink(t, target, filepath.Join(d, "s.sock"))
		if _, err := ClearStaleSocket(filepath.Join(d, "s.sock")); err != nil {
			t.Fatal(err)
		}
		if !isSocket(t, target) {
			t.Fatal("ClearStaleSocket removed a symlink's target")
		}
	})
}

// TestClearStaleSocketSettlesASocketGoneDuringItsProbe is not parallel: it
// replaces probeSocket. A socket another cleanup unlinks while it is probed
// fails the connect ENOENT: the path is absent, not live — so the hub's start
// does not refuse a vacant path — unless something is there again, which is
// left: another file put there is a replacement, and the very socket probed,
// still there, is live.
func TestClearStaleSocketSettlesASocketGoneDuringItsProbe(t *testing.T) {
	dial := probeSocket
	t.Cleanup(func() { probeSocket = dial })
	// The socket goes from its path by a rename aside, not an unlink: kept
	// alive elsewhere, its inode cannot be handed to what is put at the path
	// next (replaceSocket).
	aside := func(t *testing.T, p string) {
		if err := os.Rename(p, p+".aside"); err != nil {
			t.Error(err)
		}
	}
	for name, tc := range map[string]struct {
		during func(t *testing.T, p string) // before the connect
		after  func(t *testing.T, p string) // after it
		fate   SocketFate
		kept   bool
	}{
		"unlinked": {
			during: func(t *testing.T, p string) { _ = os.Remove(p) },
			fate:   SocketAbsent,
		},
		"gone, then another socket bound there": {
			during: aside,
			after:  func(t *testing.T, p string) { listenAt(t, p) },
			fate:   SocketReplaced, kept: true,
		},
		"gone, then a file put there": {
			during: aside,
			after:  func(t *testing.T, p string) { writeFile(t, p, "x") },
			fate:   SocketReplaced, kept: true,
		},
		"an ENOENT about another path": {
			after: func(*testing.T, string) {},
			fate:  SocketLive, kept: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(shortDir(t), "s.sock")
			listenAt(t, p)
			probeSocket = func(path string) error {
				if tc.during != nil {
					tc.during(t, path)
				}
				err := dial(path)
				if tc.during == nil {
					err = &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ENOENT)}
				}
				if tc.after != nil {
					tc.after(t, path)
				}
				return err
			}
			fate, err := ClearStaleSocket(p)
			if err != nil || fate != tc.fate {
				t.Fatalf("ClearStaleSocket = %v, %v; want %v", fate, err, tc.fate)
			}
			if _, err := os.Lstat(p); (err == nil) != tc.kept {
				t.Fatalf("after ClearStaleSocket the path is there: %v; want %v", err == nil, tc.kept)
			}
		})
	}
}

// A live socket whose backlog is full answers a connect EAGAIN on Linux, not
// ECONNREFUSED: it is live, and left. (Darwin refuses a connect to a full
// backlog, which is why SweepOrphans sweeps no socket there.)
func TestClearStaleSocketLeavesAFullBacklog(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("a full Unix-socket backlog refuses on " + runtime.GOOS)
	}
	p := filepath.Join(shortDir(t), "s.sock")
	fullBacklog(t, p)
	fate, err := ClearStaleSocket(p)
	if err != nil || fate != SocketLive {
		t.Fatalf("ClearStaleSocket = %v, %v; want live", fate, err)
	}
	if !isSocket(t, p) {
		t.Fatal("a live socket with a full backlog was removed")
	}
}

// rawSocket is a Unix stream socket's descriptor, close-on-exec, closed at
// cleanup: made and marked under syscall.ForkLock, as the net package marks
// its own where SOCK_CLOEXEC is missing (Darwin), so no child a test forks
// meanwhile inherits it.
func rawSocket(t *testing.T) int {
	t.Helper()
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

// fullBacklog binds and listens on p with a backlog of 0 and fills it with
// one connection nobody accepts, so the next connect fails EAGAIN (Linux).
func fullBacklog(t *testing.T, p string) {
	t.Helper()
	fd := rawSocket(t)
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: p}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		c, err := net.Dial("unix", p)
		if err != nil {
			if !errors.Is(err, syscall.EAGAIN) {
				t.Fatalf("setup: a connect to a full backlog failed %v, want EAGAIN", err)
			}
			return
		}
		t.Cleanup(func() { _ = c.Close() })
	}
	t.Fatal("setup: the backlog never filled")
}

// replaceSocket puts another refusing socket at p and answers its identity:
// bound beside p and renamed over it, so the two exist at once and cannot
// share an inode (an unlink then a bind at one path may be handed the inode
// just freed, and (dev, ino) tells files apart only while both exist).
func replaceSocket(t *testing.T, p string) FileID {
	t.Helper()
	next := p + ".next"
	listenAt(t, next)
	id, err := SocketID(next)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, p); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSocketIDAndUnlinkIfOurs(t *testing.T) {
	t.Parallel()
	d := shortDir(t)
	p := filepath.Join(d, "s.sock")
	listenAt(t, p)
	id, err := SocketID(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SocketID(d); err == nil {
		t.Fatal("SocketID took a directory for a socket")
	}
	replaceSocket(t, p)
	if err := UnlinkIfOurs(p, id); err != nil || !isSocket(t, p) {
		t.Fatalf("UnlinkIfOurs removed a replacement (%v)", err)
	}
	now, err := SocketID(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := UnlinkIfOurs(p, now); err != nil || exists(t, p) {
		t.Fatalf("UnlinkIfOurs left its own socket (%v)", err)
	}
}
