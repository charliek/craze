package chatgptauth

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
)

// The multi-process race's child: TestRefreshRaceChild runs as a second
// process of this test binary when raceChildEnv is set, on the directory
// raceDirEnv names, and prints raceMark and its token's digest.
const (
	raceChildEnv = "CHATGPTAUTH_RACE_CHILD"
	raceDirEnv   = "CHATGPTAUTH_RACE_DIR"
	raceMark     = "RACE-RESULT "
	raceLoaded   = "/test/child-loaded"
	raceWaiting  = "/test/child-waiting"
)

// onBusy installs onLockBusy for one test: each time a lock taker finds the
// lock held it signals busy (never blocking).
func onBusy(t *testing.T) chan struct{} {
	t.Helper()
	busy := make(chan struct{}, 16)
	onLockBusy = func() {
		select {
		case busy <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { onLockBusy = nil })
	return busy
}

// await fails the test if ch does not deliver within a generous bound: a
// forced schedule's step that never came, not a timing assumption.
func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(60 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// holdRefreshes makes every refresh at f signal its arrival and wait for the
// returned release.
func holdRefreshes(f *fakeOpenAI) (arrived chan struct{}, release func()) {
	arrived = make(chan struct{}, 4)
	hold := make(chan struct{})
	f.mu.Lock()
	f.refreshArrived, f.holdRefresh = arrived, hold
	f.mu.Unlock()
	var once sync.Once
	return arrived, func() { once.Do(func() { close(hold) }) }
}

type tokenResult struct {
	tok string
	gen uint64
	err error
}

func goToken(s *TokenSource) chan tokenResult {
	out := make(chan tokenResult, 1)
	go func() {
		tok, gen, err := s.Token(context.Background())
		out <- tokenResult{tok, gen, err}
	}()
	return out
}

// TestFreshTokenIsServedWithoutTheLock (P20 step 2): a token with more than
// 5 minutes left, its file unchanged, is returned without the lock — even
// while another process holds it — and without a refresh. The control: the
// first look, with no token in memory yet, does take the lock.
func TestFreshTokenIsServedWithoutTheLock(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seeded := seedSignedIn(t, f, dir, seedOpts{})
	busy := onBusy(t)
	held, err := atomicfile.Lock(lockFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	lockWait = 50 * time.Millisecond
	t.Cleanup(func() { lockWait = 60 * time.Second })

	s := newSource(dir)
	if _, _, err := s.Token(context.Background()); !errors.Is(err, atomicfile.ErrLockBusy) {
		t.Fatalf("the first look under a held lock = %v, want the lock's timeout", err)
	}
	await(t, busy, "the first look to find the lock held")
	held()

	tok, _, err := s.Token(context.Background())
	if err != nil || tok != seeded.AccessToken {
		t.Fatalf("Token = %s, %v", digest(tok), err)
	}
	held, err = atomicfile.Lock(lockFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	tok, _, err = s.Token(context.Background())
	if err != nil || tok != seeded.AccessToken {
		t.Fatalf("a fresh token under a held lock = %s, %v", digest(tok), err)
	}
	select {
	case <-busy:
		t.Fatal("a fresh token's Token waited for the lock")
	default:
	}
	if _, r, _, _ := f.counts(); r != 0 {
		t.Fatalf("refreshes = %d, want 0", r)
	}
}

// TestPeerRefreshIsAdopted (P20 step 4): a process whose token another
// process refreshed sees the file change and adopts the peer's token rather
// than refreshing again; its old values retire. The control: the peer did
// refresh once.
func TestPeerRefreshIsAdopted(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seeded := seedSignedIn(t, f, dir, seedOpts{})
	a, b := newSource(dir), newSource(dir)
	_, genA, err := a.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok, _, err := b.Token(context.Background()); err != nil || tok != seeded.AccessToken {
		t.Fatal("b did not load the seeded token")
	}
	if err := a.Invalidate(context.Background(), genA); err != nil {
		t.Fatal(err)
	}
	tokA, _, err := a.Token(context.Background())
	if err != nil || tokA == seeded.AccessToken {
		t.Fatalf("a did not refresh: %v", err)
	}
	tokB, _, err := b.Token(context.Background())
	if err != nil || tokB != tokA {
		t.Fatalf("b's token %s, a's %s, %v: want b to adopt a's", digest(tokB), digest(tokA), err)
	}
	if _, r, _, _ := f.counts(); r != 1 {
		t.Fatalf("refreshes = %d, want a's alone", r)
	}
	if readRec(t, dir).Generation != 2 {
		t.Fatal("the file is not at generation 2")
	}
	if !slices.Contains(b.Values(), seeded.RefreshToken) || !slices.Contains(b.Values(), tokA) {
		t.Fatal("b's values lack the retired refresh token or the adopted access token")
	}
}

// TestTokenAccountNamesTheTokensAccount (plan 033 C14r2, review r13 d):
// TokenAccount hands out Token's token with the subject and issued client id
// of the record it came from, so after another account signs in — its record
// written over the first's — the token it hands out next is the new
// account's, and says so. The control is Token itself, which hands out the
// new account's token just the same, with nothing to tell the two apart.
func TestTokenAccountNamesTheTokensAccount(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seeded := seedSignedIn(t, f, dir, seedOpts{})
	s := newSource(dir)
	tok, _, subject, client, err := s.TokenAccount(context.Background())
	if err != nil || tok != seeded.AccessToken || subject != testSubject || client != testClient {
		t.Fatalf("TokenAccount = %s, %q, %q, %v; want the seeded token of %q, %q", digest(tok), subject, client, err, testSubject, testClient)
	}

	const otherSubject, otherClient = "user-subject-other-0002", "app_client-other-0002"
	access, refresh := f.mint(otherClient)
	other := *seeded
	other.Subject, other.ClientID, other.AccessToken, other.RefreshToken = otherSubject, otherClient, access, refresh
	other.Incarnation, other.Generation = "inc-other", 1
	if err := writeRecord(dir, &other, false); err != nil {
		t.Fatal(err)
	}
	tok, _, subject, client, err = s.TokenAccount(context.Background())
	if err != nil || tok != access || subject != otherSubject || client != otherClient {
		t.Fatalf("after the other account signed in, TokenAccount = %s, %q, %q, %v", digest(tok), subject, client, err)
	}
	if tok, _, err := s.Token(context.Background()); err != nil || tok != access {
		t.Fatalf("control: Token = %s, %v; want the other account's token, unremarked", digest(tok), err)
	}
}

// TestMultiProcessRefreshRace (P20, A18): two processes — this test and a
// re-exec of its binary — both hold the same token in memory and both find
// it under 5 minutes from expiry (each runs a clock 57 minutes ahead once it
// has loaded the token). The forced schedule: the child loads the token and
// says so; this process then refreshes, its refresh held at the server while
// it holds the lock; the child is let go, finds the lock held and says so
// (the fake's raceWaiting path); only then is the refresh released. The
// child, once it has the lock, re-reads the file and adopts the rotation
// instead of refreshing the token it holds. Exactly one refresh reached the
// server, both processes hold its token, and the old refresh token was never
// used again — which the fake would have answered with refresh_token_reused
// (the control below shows it does).
func TestMultiProcessRefreshRace(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seeded := seedSignedIn(t, f, dir, seedOpts{earliestPassed: true})
	childLoaded, childWaiting := make(chan struct{}, 1), make(chan struct{}, 1)
	childGo := make(chan struct{})
	var letGo sync.Once
	defer letGo.Do(func() { close(childGo) })
	signal := func(ch chan struct{}) func() {
		return func() {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
	f.mu.Lock()
	f.hooks = map[string]func(){
		raceLoaded:  func() { signal(childLoaded)(); <-childGo },
		raceWaiting: signal(childWaiting),
	}
	f.expiresIn = 7200 // the rotation is fresh under either process's clock
	f.mu.Unlock()

	cmd := exec.Command(os.Args[0], "-test.run=^TestRefreshRaceChild$", "-test.v")
	cmd.Env = append(os.Environ(), raceChildEnv+"=1", raceDirEnv+"="+dir,
		IssuerEnv+"="+f.URL(), APIEnv+"="+f.URL()+"/v1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killer := time.AfterFunc(2*time.Minute, func() { _ = cmd.Process.Kill() })
	defer killer.Stop()
	await(t, childLoaded, "the child to load the token")

	s := newSource(dir)
	if tok, _, err := s.Token(context.Background()); err != nil || tok != seeded.AccessToken {
		t.Fatalf("this process's load: %v", err)
	}
	clock := time.Now().Add(57 * time.Minute)
	s.now = func() time.Time { return clock }
	arrived, release := holdRefreshes(f)
	defer release()
	parent := goToken(s)
	await(t, arrived, "this process's refresh at the server")
	letGo.Do(func() { close(childGo) })
	await(t, childWaiting, "the child to find the lock held")
	release()

	res := await(t, parent, "this process's token")
	if res.err != nil {
		t.Fatalf("this process: %v", res.err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the child: %v\n%s\n%s", err, stdout.Bytes(), stderr.Bytes())
	}
	childDigest := ""
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), raceMark); ok {
			childDigest = strings.TrimSpace(rest)
		}
	}
	if childDigest == "" {
		t.Fatalf("the child reported no token:\n%s", stderr.Bytes())
	}
	if _, r, _, _ := f.counts(); r != 1 {
		t.Fatalf("refreshes = %d, want exactly one rotation", r)
	}
	if childDigest != digest(res.tok) {
		t.Fatalf("the child holds %s, this process %s: want the one rotation in both", childDigest, digest(res.tok))
	}
	rec := readRec(t, dir)
	if rec.AccessToken != res.tok || rec.Generation != 2 || rec.Incarnation != seeded.Incarnation ||
		!f.liveAccess(res.tok) || f.refreshState(rec.RefreshToken) != "live" || f.refreshState(seeded.RefreshToken) != "rotated" {
		t.Fatal("the file does not hold the one rotation, or the old refresh token is not merely rotated")
	}

	// The control: the old refresh token, sent again, is what the fake
	// answers refresh_token_reused — the outcome a second rotation would
	// have met.
	ends, _ := currentEndpoints()
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {testClient}, "refresh_token": {seeded.RefreshToken}, "resource": {resource}}
	f.mu.Lock()
	f.refreshArrived, f.holdRefresh = nil, nil
	f.mu.Unlock()
	if _, _, err := ends.postToken(context.Background(), "refresh", form); refusal(err) == nil || refusal(err).Code != "refresh_token_reused" {
		t.Fatalf("reusing the old refresh token = %v, want refresh_token_reused", err)
	}
	f.assertNoLeak(t, stdout.String(), stderr.String())
}

// TestRefreshRaceChild is TestMultiProcessRefreshRace's second process: it
// loads the token on a true clock, then runs its clock 57 minutes ahead, says
// it has loaded (raceLoaded, which holds it until this process's refresh is
// in flight), and asks for the token again — finding the lock held
// (raceWaiting).
func TestRefreshRaceChild(t *testing.T) {
	if os.Getenv(raceChildEnv) == "" {
		t.Skip("the second process of TestMultiProcessRefreshRace")
	}
	issuer := os.Getenv(IssuerEnv)
	get := func(path string) {
		resp, err := http.Get(issuer + path)
		if err != nil {
			t.Fatalf("telling the parent %s: %v", path, err)
		}
		resp.Body.Close()
	}
	var ahead atomic.Int64
	now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := Source(os.Getenv(raceDirEnv))
	if _, _, err := s.Token(ctx); err != nil {
		t.Fatalf("the child's load: %v", err)
	}
	ahead.Store(int64(57 * time.Minute))
	get(raceLoaded)
	onLockBusy = func() { get(raceWaiting) }
	tok, _, err := s.Token(ctx)
	if err != nil {
		t.Fatalf("the child's Token: %v", err)
	}
	fmt.Println(raceMark + digest(tok))
}

// TestRefreshVsLogout (A18): a refresh holding the lock, and a sign-out
// that arrives meanwhile — forced: the sign-out is seen waiting before the
// refresh is released. The refresh lands, then the sign-out revokes the
// rotated refresh token and deletes the file; the refreshing process's next
// Token is ErrSignedOut, and neither Token nor Invalidate recreates the file.
// The control: the refresh's token was the rotation, live until the
// sign-out.
func TestRefreshVsLogout(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seeded := seedSignedIn(t, f, dir, seedOpts{expiresIn: 2 * time.Minute, earliestPassed: true})
	if _, err := hostID(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	arrived, release := holdRefreshes(f)
	defer release()
	busy := onBusy(t)
	s := newSource(dir)
	refreshed := goToken(s)
	await(t, arrived, "the refresh at the server")
	loggedOut := make(chan error, 1)
	var res LogoutResult
	go func() {
		var err error
		res, err = Logout(context.Background(), dir)
		loggedOut <- err
	}()
	await(t, busy, "the sign-out to wait for the lock")
	release()
	r := await(t, refreshed, "the refresh")
	if r.err != nil || r.tok == seeded.AccessToken || !f.liveAccess(r.tok) {
		t.Fatalf("the refresh: %v", r.err)
	}
	if err := await(t, loggedOut, "the sign-out"); err != nil {
		t.Fatal(err)
	}
	if !res.SignedIn || !res.Revoked {
		t.Fatalf("logout = %+v", res)
	}
	f.mu.Lock()
	revoked := f.revokes[0]
	f.mu.Unlock()
	if revoked.Get("token") == seeded.RefreshToken || f.refreshState(revoked.Get("token")) != "revoked" {
		t.Fatal("the sign-out did not revoke the rotated refresh token")
	}
	if !tokenFileGone(t, dir) {
		t.Fatal("the sign-out left the token file")
	}
	if _, _, err := s.Token(context.Background()); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("Token after the sign-out = %v, want ErrSignedOut", err)
	}
	if err := s.Invalidate(context.Background(), r.gen); err != nil && !errors.Is(err, ErrSignedOut) {
		t.Fatalf("Invalidate after the sign-out = %v", err)
	}
	if _, _, err := s.Token(context.Background()); !errors.Is(err, ErrSignedOut) || !tokenFileGone(t, dir) {
		t.Fatalf("Token = %v; the file must stay gone", err)
	}
	if c, _ := ReadClient(dir); c.ClientID != testClient {
		t.Fatal("the sign-out dropped the registration")
	}
	if _, err := readHostID(dir); err != nil {
		t.Fatal("the sign-out dropped the host id")
	}
}

// TestSignInVsRefresh (A18): a sign-in and a refresh contend for the lock,
// in both orders, each forced. Refresh first: the sign-in's install waits,
// then replaces the rotation with a new incarnation, which the refreshing
// process adopts at its next Token. Sign-in first (its write held inside the
// lock): the stale process waits, then adopts the new sign-in rather than
// refreshing the old one — no refresh at all, the old refresh token never
// sent.
func TestSignInVsRefresh(t *testing.T) {
	t.Run("refresh first", func(t *testing.T) {
		f := newFake(t)
		useFake(t, f)
		dir := nativeDir(t)
		seeded := seedSignedIn(t, f, dir, seedOpts{expiresIn: 2 * time.Minute, earliestPassed: true})
		arrived, release := holdRefreshes(f)
		defer release()
		busy := onBusy(t)
		s := newSource(dir)
		refreshed := goToken(s)
		await(t, arrived, "the refresh at the server")
		signedIn := make(chan error, 1)
		go func() {
			_, err := signInErr(f, dir)
			signedIn <- err
		}()
		await(t, busy, "the sign-in's install to wait for the lock")
		release()
		if r := await(t, refreshed, "the refresh"); r.err != nil {
			t.Fatal(r.err)
		}
		if err := await(t, signedIn, "the sign-in"); err != nil {
			t.Fatal(err)
		}
		rec := readRec(t, dir)
		if rec.Incarnation == seeded.Incarnation || rec.Generation != 1 {
			t.Fatal("the sign-in did not install a new incarnation over the refresh")
		}
		tok, _, err := s.Token(context.Background())
		if err != nil || tok != rec.AccessToken {
			t.Fatalf("the refreshing process did not adopt the sign-in: %v", err)
		}
		if _, r, _, _ := f.counts(); r != 1 {
			t.Fatalf("refreshes = %d, want the one", r)
		}
	})
	t.Run("sign-in first", func(t *testing.T) {
		f := newFake(t)
		useFake(t, f)
		dir := nativeDir(t)
		seedSignedIn(t, f, dir, seedOpts{})
		s := newSource(dir)
		if _, _, err := s.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
		old := readRec(t, dir)
		clock := time.Now().Add(57 * time.Minute) // the token in memory has 3 minutes left
		s.now = func() time.Time { return clock }

		installing, proceed := make(chan struct{}), make(chan struct{})
		writeSync = func(path string, b []byte, perm os.FileMode, check func() error) error {
			if path == TokenFile(dir) {
				close(installing)
				<-proceed
			}
			return atomicfile.WriteSyncChecked(path, b, perm, check)
		}
		t.Cleanup(func() { writeSync = atomicfile.WriteSyncChecked })
		busy := onBusy(t)
		f.expiresIn = 7200 // fresh under the stale process's clock
		signedIn := make(chan error, 1)
		go func() {
			_, err := signInErr(f, dir)
			signedIn <- err
		}()
		await(t, installing, "the sign-in's install, inside the lock")
		writeSync = atomicfile.WriteSyncChecked
		stale := goToken(s)
		await(t, busy, "the stale process to wait for the lock")
		close(proceed)
		if err := await(t, signedIn, "the sign-in"); err != nil {
			t.Fatal(err)
		}
		r := await(t, stale, "the stale process's token")
		rec := readRec(t, dir)
		if r.err != nil || r.tok != rec.AccessToken || rec.Incarnation == old.Incarnation {
			t.Fatalf("the stale process did not adopt the new sign-in: %v", r.err)
		}
		if _, refreshes, _, _ := f.counts(); refreshes != 0 || f.refreshState(old.RefreshToken) != "live" {
			t.Fatal("the stale process refreshed the old sign-in's token")
		}
	})
}

// TestDeletedFileIsNeverRecreated (A18): a token file removed under a
// process — by a sign-out elsewhere or a person's rm — is ErrSignedOut at
// its next Token, and nothing it does recreates it: not Invalidate, not a
// refresh that was in flight when the file went (its write's last check
// finds it gone). The control: the same in-flight refresh with the file in
// place writes generation 2.
func TestDeletedFileIsNeverRecreated(t *testing.T) {
	t.Run("removed while idle", func(t *testing.T) {
		f := newFake(t)
		useFake(t, f)
		dir := nativeDir(t)
		seedSignedIn(t, f, dir, seedOpts{})
		s := newSource(dir)
		_, gen, err := s.Token(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(TokenFile(dir)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Token(context.Background()); !errors.Is(err, ErrSignedOut) {
			t.Fatalf("Token = %v, want ErrSignedOut", err)
		}
		if err := s.Invalidate(context.Background(), gen); err != nil && !errors.Is(err, ErrSignedOut) {
			t.Fatalf("Invalidate = %v", err)
		}
		if err := s.Invalidate(context.Background(), gen+1); err != nil && !errors.Is(err, ErrSignedOut) {
			t.Fatalf("Invalidate of the current generation = %v", err)
		}
		if !tokenFileGone(t, dir) {
			t.Fatal("the token file was recreated")
		}
	})
	for _, remove := range []bool{false, true} {
		name := "a refresh in flight, the file kept (control)"
		if remove {
			name = "a refresh in flight, the file removed"
		}
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			useFake(t, f)
			dir := nativeDir(t)
			seedSignedIn(t, f, dir, seedOpts{expiresIn: 2 * time.Minute, earliestPassed: true})
			arrived, release := holdRefreshes(f)
			defer release()
			s := newSource(dir)
			got := goToken(s)
			await(t, arrived, "the refresh at the server")
			if remove {
				if err := os.Remove(TokenFile(dir)); err != nil {
					t.Fatal(err)
				}
			}
			release()
			r := await(t, got, "the refresh")
			if !remove {
				if r.err != nil || readRec(t, dir).Generation != 2 {
					t.Fatalf("the control's refresh: %v", r.err)
				}
				return
			}
			if !errors.Is(r.err, ErrSignedOut) {
				t.Fatalf("Token = %v, want ErrSignedOut", r.err)
			}
			if !tokenFileGone(t, dir) {
				t.Fatal("the refresh recreated the removed file")
			}
		})
	}
}

// TestWriteFailureAfterRotation (plan 033 §3.10 ordering): a refresh whose
// write fails is tried writeTries times and surfaced once — no token value in
// the error — while the rotated tokens stay usable in this process with no
// second refresh; once the disk recovers, the next Token puts them in the
// file, where another process adopts them. The control: the file kept the
// pre-rotation record until then.
func TestWriteFailureAfterRotation(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seeded := seedSignedIn(t, f, dir, seedOpts{expiresIn: 2 * time.Minute, earliestPassed: true})
	attempts := 0
	diskFull := errors.New("no space left on device")
	writeSync = func(path string, b []byte, perm os.FileMode, check func() error) error {
		if path == TokenFile(dir) {
			attempts++
			return diskFull
		}
		return atomicfile.WriteSyncChecked(path, b, perm, check)
	}
	t.Cleanup(func() { writeSync = atomicfile.WriteSyncChecked })

	s := newSource(dir)
	_, _, err := s.Token(context.Background())
	if !errors.Is(err, diskFull) || !strings.Contains(err.Error(), "could not be saved") {
		t.Fatalf("Token = %v, want the write failure surfaced", err)
	}
	if attempts != writeTries {
		t.Fatalf("the write was tried %d times, want %d", attempts, writeTries)
	}
	f.assertNoLeak(t, err.Error())
	if rec := readRec(t, dir); rec.Generation != 1 || rec.AccessToken != seeded.AccessToken {
		t.Fatal("the file changed though every write failed")
	}
	tok, _, err := s.Token(context.Background())
	if err != nil || tok == seeded.AccessToken || !f.liveAccess(tok) {
		t.Fatalf("after the failure Token = %s, %v; want the rotated token, kept in memory", digest(tok), err)
	}
	if _, r, _, _ := f.counts(); r != 1 {
		t.Fatalf("refreshes = %d, want 1", r)
	}

	writeSync = atomicfile.WriteSyncChecked
	again, _, err := s.Token(context.Background())
	if err != nil || again != tok {
		t.Fatalf("Token after the disk recovered = %v", err)
	}
	rec := readRec(t, dir)
	if rec.AccessToken != tok || rec.Generation != 2 {
		t.Fatal("the rotated tokens did not reach the file once it could be written")
	}
	other, _, err := newSource(dir).Token(context.Background())
	if err != nil || other != tok {
		t.Fatalf("another process: %v", err)
	}
	if _, r, _, _ := f.counts(); r != 1 {
		t.Fatalf("refreshes = %d, want still 1", r)
	}
}

// TestEarliestRefreshAtNeverBlocks (P20 step 5): a token under 5 minutes
// left whose earliest_refresh_at is still to come is returned at once, no
// refresh, nothing waited on. The controls: the hint passed refreshes; an
// expired token refreshes whatever the hint says.
func TestEarliestRefreshAtNeverBlocks(t *testing.T) {
	cases := []struct {
		name    string
		o       seedOpts
		refresh bool
	}{
		{"the hint to come", seedOpts{expiresIn: 2 * time.Minute, earliestIn: 30 * time.Minute}, false},
		{"the hint passed", seedOpts{expiresIn: 2 * time.Minute, earliestPassed: true}, true},
		{"no hint", seedOpts{expiresIn: 2 * time.Minute}, true},
		{"expired, the hint to come", seedOpts{expiresIn: -time.Minute, earliestIn: 30 * time.Minute}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			useFake(t, f)
			dir := nativeDir(t)
			seeded := seedSignedIn(t, f, dir, tc.o)
			tok, _, err := newSource(dir).Token(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, r, _, _ := f.counts()
			if tc.refresh != (r == 1) || tc.refresh == (tok == seeded.AccessToken) {
				t.Fatalf("refreshes %d, same token %v; want refresh %v", r, tok == seeded.AccessToken, tc.refresh)
			}
		})
	}
}

// TestInvalidateBypassesTheHint (plan 033 §3.10): after a 401, Invalidate
// refreshes even when earliest_refresh_at says not yet, and even a token with
// an hour left; an Invalidate of a generation already replaced does nothing
// (the control).
func TestInvalidateBypassesTheHint(t *testing.T) {
	for _, o := range []seedOpts{{expiresIn: 2 * time.Minute, earliestIn: 30 * time.Minute}, {earliestIn: 50 * time.Minute}} {
		f := newFake(t)
		useFake(t, f)
		dir := nativeDir(t)
		seeded := seedSignedIn(t, f, dir, o)
		s := newSource(dir)
		tok, gen, err := s.Token(context.Background())
		if err != nil || tok != seeded.AccessToken {
			t.Fatalf("Token: %v", err)
		}
		if err := s.Invalidate(context.Background(), gen); err != nil {
			t.Fatal(err)
		}
		next, gen2, err := s.Token(context.Background())
		if err != nil || next == tok || gen2 == gen {
			t.Fatalf("after Invalidate the token did not change: %v", err)
		}
		if err := s.Invalidate(context.Background(), gen); err != nil {
			t.Fatal(err)
		}
		if _, r, _, _ := f.counts(); r != 1 {
			t.Fatalf("refreshes = %d, want 1 (the stale Invalidate is a no-op)", r)
		}
	}
}

// TestLockTimeoutReturnsTheCurrentToken (P20 step 3): a process whose token
// needs the look under the lock, finding it held past the bound, keeps
// using its token while it has not expired; an expired one is the lock's
// error (the control); a cancelled context is the context's error.
func TestLockTimeoutReturnsTheCurrentToken(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seeded := seedSignedIn(t, f, dir, seedOpts{})
	s := newSource(dir)
	if _, _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	held, err := atomicfile.Lock(lockFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	lockWait = 50 * time.Millisecond
	t.Cleanup(func() { lockWait = 60 * time.Second })
	busy := onBusy(t)

	clock := time.Now().Add(57 * time.Minute)
	s.now = func() time.Time { return clock }
	tok, _, err := s.Token(context.Background())
	if err != nil || tok != seeded.AccessToken {
		t.Fatalf("Token under a held lock with 3 minutes left = %s, %v; want the current token", digest(tok), err)
	}
	await(t, busy, "the look to find the lock held")

	clock = time.Now().Add(61 * time.Minute)
	if _, _, err := s.Token(context.Background()); !errors.Is(err, atomicfile.ErrLockBusy) {
		t.Fatalf("Token with the token expired = %v, want the lock's timeout", err)
	}
	lockWait = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Token with a cancelled context = %v", err)
	}
	if _, r, _, _ := f.counts(); r != 0 {
		t.Fatal("a held lock let a refresh through")
	}
}

// TestRetirementAfterAnHour (P35): a rotated refresh or id token stays in
// Values for an hour after the rotation, an access token for an hour after
// its expiry, then leaves; the current values never leave (the control).
func TestRetirementAfterAnHour(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seeded := seedSignedIn(t, f, dir, seedOpts{})
	s := newSource(dir)
	base := time.Now()
	clock := base
	s.now = func() time.Time { return clock }
	_, gen, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Invalidate(context.Background(), gen); err != nil {
		t.Fatal(err)
	}
	cur := readRec(t, dir)
	has := func(v string) bool { return slices.Contains(s.Values(), v) }
	check := func(at time.Duration, access, refresh bool) {
		t.Helper()
		clock = base.Add(at)
		if has(seeded.AccessToken) != access || has(seeded.RefreshToken) != refresh || has(seeded.IDToken) != refresh {
			t.Fatalf("at +%s: old access %v, refresh %v, id %v; want %v, %v, %v", at,
				has(seeded.AccessToken), has(seeded.RefreshToken), has(seeded.IDToken), access, refresh, refresh)
		}
		if cur.IDToken == seeded.IDToken {
			t.Fatal("the fake renewed no id token; the id token's retirement is not tested")
		}
		if !has(cur.AccessToken) || !has(cur.RefreshToken) {
			t.Fatalf("at +%s the current values left the set", at)
		}
	}
	check(0, true, true)
	check(59*time.Minute, true, true)
	check(61*time.Minute, true, false)
	check(2*time.Hour-time.Minute, true, false)
	check(2*time.Hour+time.Minute, false, false)
}

// TestSubscribersNotifiedBeforeTheWrite (P19, plan 033 §3.10 ordering): a
// subscriber learns a refresh's new values while the file still holds the
// old record — before the write — and an adoption's before the token is
// returned; after Token the file holds the new record (the control that the
// subscriber's look was not of a file that never changes). An unsubscribed
// function hears nothing more.
func TestSubscribersNotifiedBeforeTheWrite(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seeded := seedSignedIn(t, f, dir, seedOpts{expiresIn: 2 * time.Minute, earliestPassed: true})
	s := newSource(dir)
	type call struct {
		values  []string
		fileGen uint64
	}
	var calls []call
	_, unsubscribe := s.Subscribe(func(values []string) {
		r, _, err := readRecord(dir)
		if err != nil {
			t.Errorf("the subscriber's look at the file: %v", err)
			return
		}
		calls = append(calls, call{values, r.Generation})
	})
	tok, _, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("the subscriber was called %d times, want the first look's adoption and the refresh", len(calls))
	}
	if !slices.Contains(calls[0].values, seeded.AccessToken) || calls[0].fileGen != 1 {
		t.Fatal("the adoption's call did not carry the file's values")
	}
	if !slices.Contains(calls[1].values, tok) || calls[1].fileGen != 1 {
		t.Fatalf("the refresh's call: has the new token %v, file generation %d; want the new values while the file is still at 1",
			slices.Contains(calls[1].values, tok), calls[1].fileGen)
	}
	if readRec(t, dir).Generation != 2 {
		t.Fatal("after Token the file is not at generation 2")
	}
	unsubscribe()
	_, gen, _ := s.Token(context.Background())
	if err := s.Invalidate(context.Background(), gen); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatal("an unsubscribed function was called")
	}
}

// TestTerminalRefreshErrors (plan 033 §3.10 error classes, A18): each
// unusable-refresh-token code deletes the token file, keeps the
// registration and host id, and is ErrSignInAgain; invalid_client also
// drops the client id; a renewed grant without plan usage is
// ErrPlanUsageDisabled with plan_usage false; a reply naming another client
// is ErrSignInAgain. No error carries a token value.
func TestTerminalRefreshErrors(t *testing.T) {
	codes := []string{"invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired",
		"refresh_token_invalidated", "refresh_token_reused", "invalid_client"}
	run := func(t *testing.T, shape func(f *fakeOpenAI)) (*fakeOpenAI, string, error) {
		f := newFake(t)
		useFake(t, f)
		dir := nativeDir(t)
		seedSignedIn(t, f, dir, seedOpts{expiresIn: 2 * time.Minute, earliestPassed: true})
		if _, err := hostID(context.Background(), dir); err != nil {
			t.Fatal(err)
		}
		shape(f)
		s := newSource(dir)
		_, _, err := s.Token(context.Background())
		if !tokenFileGone(t, dir) {
			t.Fatal("the token file survived a terminal refusal")
		}
		if _, _, again := s.Token(context.Background()); !errors.Is(again, ErrSignedOut) {
			t.Fatalf("Token after = %v, want ErrSignedOut", again)
		}
		if _, herr := readHostID(dir); herr != nil {
			t.Fatal("the host id went")
		}
		f.assertNoLeak(t, err.Error())
		return f, dir, err
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			_, dir, err := run(t, func(f *fakeOpenAI) { f.refreshErr = code })
			if !errors.Is(err, ErrSignInAgain) || refusal(err) == nil || refusal(err).Code != code {
				t.Fatalf("Token = %v, want ErrSignInAgain with %s", err, code)
			}
			c, _ := ReadClient(dir)
			if (code == codeInvalidClient) != (c.ClientID == "") || c.Subject != testSubject {
				t.Fatalf("registration after %s = %+v", code, c)
			}
		})
	}
	t.Run("plan usage withdrawn", func(t *testing.T) {
		_, dir, err := run(t, func(f *fakeOpenAI) { f.scope = "openid profile email offline_access resource.invoke" })
		if !errors.Is(err, ErrPlanUsageDisabled) {
			t.Fatalf("Token = %v", err)
		}
		if c, _ := ReadClient(dir); c.PlanUsage || c.ClientID != testClient {
			t.Fatalf("registration = %+v", c)
		}
	})
	t.Run("another client's access token", func(t *testing.T) {
		_, _, err := run(t, func(f *fakeOpenAI) { f.accessClient = "oaiapp_other" })
		if !errors.Is(err, ErrSignInAgain) || !strings.Contains(err.Error(), "another client") {
			t.Fatalf("Token = %v", err)
		}
	})
}

// TestTransientRefreshErrorsKeepCredentials (plan 033 §3.10): a 5xx — even
// one naming invalid_grant — or an unreachable server keeps the token file;
// Token returns the current token while it has not expired, and Invalidate
// (the token refused) the error. The control: the file is untouched and a
// later refresh succeeds.
func TestTransientRefreshErrorsKeepCredentials(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(t *testing.T, f *fakeOpenAI)
	}{
		{"500", func(t *testing.T, f *fakeOpenAI) { f.refreshStatus = 500 }},
		{"503 naming invalid_grant", func(t *testing.T, f *fakeOpenAI) { f.refreshStatus, f.refreshErr = 503, "invalid_grant" }},
		{"unreachable", func(t *testing.T, f *fakeOpenAI) {
			gone := newFake(t)
			gone.srv.Close()
			setEnv(t, map[string]string{IssuerEnv: gone.URL(), APIEnv: gone.URL() + "/v1"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			useFake(t, f)
			dir := nativeDir(t)
			seeded := seedSignedIn(t, f, dir, seedOpts{expiresIn: 2 * time.Minute, earliestPassed: true})
			before := snapshot(t, dir)
			tc.shape(t, f)
			s := newSource(dir)
			tok, gen, err := s.Token(context.Background())
			if err != nil || tok != seeded.AccessToken {
				t.Fatalf("Token = %v, want the current token", err)
			}
			if err := s.Invalidate(context.Background(), gen); err == nil || errors.Is(err, ErrSignInAgain) {
				t.Fatalf("Invalidate = %v, want the transient error", err)
			}
			assertUnchanged(t, dir, before)
			useFake(t, f)
			f.mu.Lock()
			f.refreshStatus, f.refreshErr = 0, ""
			f.mu.Unlock()
			if err := s.Invalidate(context.Background(), gen); err != nil {
				t.Fatalf("the later refresh: %v", err)
			}
		})
	}
}

// TestRefusalsRepeatOnlyKnownCodes (plan 033 C14r, r12 #1): an endpoint that
// echoes a credential as its error code — the refresh token a refresh sent,
// in the standard {"error":"…"} shape; a short opaque value (a token's
// fragment, shaped like a code) from the model list, in the
// {"error":{"code":…}} shape, and in the authorize redirect's error — gets "an unrecognised error code" in the message, and the value
// nowhere: not in the text, not in Code. The control is a code craze knows,
// kept and named in the same three places — so the allowlist, not a
// blanket drop, is what hides the echoed one.
func TestRefusalsRepeatOnlyKnownCodes(t *testing.T) {
	const echoed = "test-oauth-echo-0042"
	type shaped struct {
		err   error
		value string // the value the endpoint echoed back
	}
	refreshing := func(t *testing.T, known string) shaped {
		f := newFake(t)
		useFake(t, f)
		dir := nativeDir(t)
		rec := seedSignedIn(t, f, dir, seedOpts{})
		code := rec.RefreshToken
		if known != "" {
			code = known
		}
		f.mu.Lock()
		f.refreshErr = code
		f.mu.Unlock()
		s := newSource(dir)
		_, gen, err := s.Token(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		err = s.Invalidate(context.Background(), gen)
		if err == nil || tokenFileGone(t, dir) {
			t.Fatalf("Invalidate = %v (file gone %v), want the refusal with the sign-in kept", err, tokenFileGone(t, dir))
		}
		f.assertNoLeak(t, err.Error())
		return shaped{err, rec.RefreshToken}
	}
	listing := func(t *testing.T, known string) shaped {
		f := newFake(t)
		useFake(t, f)
		dir := nativeDir(t)
		seedSignedIn(t, f, dir, seedOpts{})
		code := echoed
		if known != "" {
			code = known
		}
		f.mu.Lock()
		f.modelsErr = code
		f.mu.Unlock()
		_, err := FetchModels(context.Background(), newSource(dir))
		if err == nil {
			t.Fatal("FetchModels succeeded against a refusal")
		}
		f.assertNoLeak(t, err.Error())
		return shaped{err, echoed}
	}
	authorizing := func(t *testing.T, known string) shaped {
		f := newFake(t)
		useFake(t, f)
		a, err := Begin(context.Background(), nativeDir(t), BeginOptions{PasteOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		code := echoed
		if known != "" {
			code = known
		}
		q := url.Values{"error": {code}, "state": {authQuery(t, a).Get("state")}}
		if err := a.Paste(a.RedirectURI() + "?" + q.Encode()); err != nil {
			t.Fatal(err)
		}
		_, err = a.Wait(context.Background())
		if err == nil {
			t.Fatal("Wait succeeded against a refusal")
		}
		return shaped{err, echoed}
	}
	for _, tc := range []struct {
		name  string
		run   func(t *testing.T, known string) shaped
		known string
	}{
		{"refresh", refreshing, "invalid_scope"},
		{"models", listing, "subscription_sharing_usage_unavailable"},
		{"authorize", authorizing, "consent_required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.run(t, "")
			var oe *OAuthError
			if !errors.As(got.err, &oe) || oe.Code != "" || !oe.Unrecognised {
				t.Fatalf("the refusal = %#v (%v), want an unrecognised code", oe, got.err)
			}
			if strings.Contains(got.err.Error(), got.value) || !strings.Contains(got.err.Error(), "an unrecognised error code") {
				t.Fatalf("the message = %q, want the code unnamed (the echoed value is %d bytes)", got.err, len(got.value))
			}
			t.Run("a known code (control)", func(t *testing.T) {
				kept := tc.run(t, tc.known)
				if !errors.As(kept.err, &oe) || oe.Code != tc.known || oe.Unrecognised || !strings.Contains(kept.err.Error(), tc.known) {
					t.Fatalf("the refusal = %#v (%v), want %s kept", oe, kept.err, tc.known)
				}
			})
		})
	}
	for _, code := range []string{"invalid_grant", "refresh_token_reused", "invalid_client", "access_denied", "subscription_sharing_usage_limit_exceeded"} {
		if c, odd := codeOf(code); c != code || odd {
			t.Errorf("codeOf(%q) = %q, %v: a code craze acts on was dropped", code, c, odd)
		}
	}
	if c, odd := codeOf(""); c != "" || odd {
		t.Errorf(`codeOf("") = %q, %v: no code is not an unrecognised one`, c, odd)
	}
}

// TestCorruptTokenFile: a token file that is not one craze wrote is "sign
// in again", never ErrSignedOut, and is left in place for a sign-in to
// replace.
func TestCorruptTokenFile(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seedSignedIn(t, f, dir, seedOpts{})
	if err := os.WriteFile(TokenFile(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := newSource(dir).Token(context.Background())
	if !errors.Is(err, ErrSignInAgain) || errors.Is(err, ErrSignedOut) {
		t.Fatalf("Token = %v", err)
	}
	if tokenFileGone(t, dir) {
		t.Fatal("the corrupt file was removed")
	}
	signIn(t, f, dir)
	if _, _, err := newSource(dir).Token(context.Background()); err != nil {
		t.Fatalf("after a sign-in: %v", err)
	}
}

// TestUsageLatch (P33): latched, Token is ErrUsageLimited at once — no
// network, no file — until cleared; the control is the same Token before
// and after.
func TestUsageLatch(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seedSignedIn(t, f, dir, seedOpts{expiresIn: 2 * time.Minute, earliestPassed: true})
	s := newSource(dir)
	s.LatchUsageLimit()
	if !s.UsageLimited() {
		t.Fatal("the latch is not set")
	}
	if _, _, err := s.Token(context.Background()); !errors.Is(err, ErrUsageLimited) {
		t.Fatalf("Token while latched = %v", err)
	}
	if _, r, _, _ := f.counts(); r != 0 {
		t.Fatal("a latched Token refreshed")
	}
	s.ClearUsageLimit()
	if _, _, err := s.Token(context.Background()); err != nil || s.UsageLimited() {
		t.Fatalf("Token after the latch was cleared = %v", err)
	}
}

// TestLatchSetWhileATokenWaits (plan 033 C14r, r12 #5): a Token call that
// passed the latch's first look and then waited — for a refresh's reply as
// its own look, for another caller's look it joined, for the lock a peer
// held — is answered ErrUsageLimited when the latch was set meanwhile, not
// with a token for a new request. The rotation the wait obtained is on disk
// and in use all the same: the file is at generation 2, and once the latch
// lifts the next Token is that token, with no second refresh. The control is
// each schedule without the latch, which answers the rotated token.
func TestLatchSetWhileATokenWaits(t *testing.T) {
	for _, tc := range []struct {
		name string
		// wait starts Token on s and returns once it waits; release ends the
		// wait; the result is the Token call's answer.
		wait func(t *testing.T, f *fakeOpenAI, s *TokenSource) (release func(), results []chan tokenResult)
	}{
		{"its own refresh", func(t *testing.T, f *fakeOpenAI, s *TokenSource) (func(), []chan tokenResult) {
			arrived, release := holdRefreshes(f)
			out := goToken(s)
			await(t, arrived, "the refresh to reach the server")
			return release, []chan tokenResult{out}
		}},
		{"another caller's refresh", func(t *testing.T, f *fakeOpenAI, s *TokenSource) (func(), []chan tokenResult) {
			arrived, release := holdRefreshes(f)
			joined := make(chan struct{}, 1)
			onJoin = func() { joined <- struct{}{} }
			t.Cleanup(func() { onJoin = nil })
			owner := goToken(s)
			await(t, arrived, "the owner's refresh to reach the server")
			joiner := goToken(s)
			await(t, joined, "the second caller to join the look")
			return release, []chan tokenResult{owner, joiner}
		}},
		{"a peer's lock", func(t *testing.T, f *fakeOpenAI, s *TokenSource) (func(), []chan tokenResult) {
			held, err := atomicfile.Lock(lockFile(s.dir))
			if err != nil {
				t.Fatal(err)
			}
			busy := onBusy(t)
			out := goToken(s)
			await(t, busy, "the look to find the lock held")
			return held, []chan tokenResult{out}
		}},
	} {
		for _, latch := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, latched %v", tc.name, latch), func(t *testing.T) {
				f := newFake(t)
				useFake(t, f)
				dir := nativeDir(t)
				seedSignedIn(t, f, dir, seedOpts{expiresIn: 2 * time.Minute, earliestPassed: true})
				s := newSource(dir)
				release, results := tc.wait(t, f, s)
				if latch {
					s.LatchUsageLimit()
				}
				release()
				rotated := ""
				for i, out := range results {
					r := await(t, out, "the waiting Token call")
					rotated = readRec(t, dir).AccessToken
					switch {
					case latch && (!errors.Is(r.err, ErrUsageLimited) || r.tok != ""):
						t.Fatalf("call %d: Token = %s, %v; want ErrUsageLimited", i, digest(r.tok), r.err)
					case !latch && (r.err != nil || r.tok != rotated):
						t.Fatalf("control, call %d: Token = %s, %v; want the rotated token", i, digest(r.tok), r.err)
					}
				}
				if readRec(t, dir).Generation != 2 || !slices.Contains(s.Values(), rotated) {
					t.Fatal("the rotation the wait obtained is not on disk and in use")
				}
				s.ClearUsageLimit()
				if tok, _, err := s.Token(context.Background()); err != nil || tok != rotated {
					t.Fatalf("Token after the latch lifted = %s, %v; want the rotated token", digest(tok), err)
				}
				if _, r, _, _ := f.counts(); r != 1 {
					t.Fatalf("%d refreshes, want the one the wait obtained", r)
				}
			})
		}
	}
}

// TestSubscribeBetweenAnnouncementAndUse (plan 033 C14r, r12 #6b): a
// subscriber that registers after a refresh's announcement took its snapshot
// of the subscribers — so its function is not called with the new values —
// and before the record holding them is the source's, gets them from
// Subscribe itself: notify published them first. The barrier holds the
// refresh there, between the snapshot and the publication of the record. The
// control is the subscriber registered before the barrier, whose function is
// called; and the announced values reach Values in the same window.
func TestSubscribeBetweenAnnouncementAndUse(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seeded := seedSignedIn(t, f, dir, seedOpts{expiresIn: 2 * time.Minute, earliestPassed: true})
	s := newSource(dir)
	var mu sync.Mutex
	var early, late [][]string
	_, unsubscribe := s.Subscribe(func(v []string) {
		mu.Lock()
		defer mu.Unlock()
		early = append(early, v)
	})
	defer unsubscribe()
	announced, proceed := make(chan []string, 1), make(chan struct{})
	onNotify = func(values []string) {
		if slices.Contains(values, seeded.AccessToken) {
			return // the first look's adoption: not the refresh
		}
		announced <- values
		<-proceed
	}
	t.Cleanup(func() { onNotify = nil })
	out := goToken(s)
	values := await(t, announced, "the refresh's announcement")

	got, unsubscribeLate := s.Subscribe(func(v []string) {
		mu.Lock()
		defer mu.Unlock()
		late = append(late, v)
	})
	defer unsubscribeLate()
	inValues := s.Values()
	close(proceed)
	r := await(t, out, "the refreshing Token call")
	if r.err != nil || !slices.Contains(values, r.tok) {
		t.Fatalf("Token = %v; want the token the refresh announced", r.err)
	}
	for _, v := range values {
		if !slices.Contains(got, v) || !slices.Contains(inValues, v) {
			t.Fatal("a value announced before the subscription is in neither what Subscribe returned nor Values")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(late) != 0 {
		t.Fatal("premise: the late subscriber's function was called, so the window was not between the snapshot and the publication")
	}
	if len(early) != 2 || !slices.Contains(early[1], r.tok) {
		t.Fatalf("control: the early subscriber heard %d announcements, want the adoption's and the refresh's", len(early))
	}
}

// TestSourceIsOnePerDirectory: Source answers one token source per
// directory, however the path is spelled; another directory gets its own.
func TestSourceIsOnePerDirectory(t *testing.T) {
	dir := nativeDir(t)
	if Source(dir) != Source(dir+"/") || Source(dir) != Source(dir+"/../native") {
		t.Fatal("one directory has two token sources")
	}
	if Source(dir) == Source(nativeDir(t)) {
		t.Fatal("two directories share a token source")
	}
}

// TestStoredValuesAndStatus: the turn-start look's StoredValues is the
// file's three token values, and none with no file; ReadStatus never needs a
// token; MarkNoticeShown makes the next sign-in of the same account skip the
// notice (the first one showed it: the control).
func TestStoredValuesAndStatus(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	if v, err := StoredValues(dir); err != nil || v != nil {
		t.Fatalf("no file: %v, %v", v, err)
	}
	if st, err := ReadStatus(dir); err != nil || st != (Status{}) {
		t.Fatalf("no registration: %+v, %v", st, err)
	}
	if res := signIn(t, f, dir); !res.ShowNotice {
		t.Fatal("the first sign-in did not ask for the notice")
	}
	rec := readRec(t, dir)
	v, err := StoredValues(dir)
	if err != nil || len(v) != 3 || !slices.Contains(v, rec.AccessToken) || !slices.Contains(v, rec.RefreshToken) || !slices.Contains(v, rec.IDToken) {
		t.Fatalf("StoredValues = %d values, %v", len(v), err)
	}
	if st, err := ReadStatus(dir); err != nil || st != (Status{Registered: true, SignedIn: true, PlanUsage: true, Email: testEmail}) {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if err := MarkNoticeShown(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if res := signIn(t, f, dir); res.ShowNotice {
		t.Fatal("the notice was asked for again for the same account")
	}
}
