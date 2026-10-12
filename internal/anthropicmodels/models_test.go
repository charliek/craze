package anthropicmodels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The keys these tests fetch with: dummies, nothing like real ones.
const (
	keyA = "sk-ant-canary-not-a-secret-000A"
	keyB = "sk-ant-canary-not-a-secret-000B"
)

// t0 is the tests' clock: now() reads it unless a test moves it.
var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// fakeAPI replaces listModels for the rest of the test: each call is
// counted and answered by reply. Nothing else is set, so a test that forgot
// it would fail on its own rather than reach the API: TestMain points the
// Messages API's origin at a loopback address nothing listens on.
type fakeAPI struct {
	calls atomic.Int32
	reply func(ctx context.Context, key string) ([]messagesapi.ModelInfo, bool, error)
}

func useFake(t *testing.T, reply func(ctx context.Context, key string) ([]messagesapi.ModelInfo, bool, error)) *fakeAPI {
	t.Helper()
	f := &fakeAPI{reply: reply}
	old := listModels
	listModels = func(ctx context.Context, key string) ([]messagesapi.ModelInfo, bool, error) {
		f.calls.Add(1)
		return f.reply(ctx, key)
	}
	t.Cleanup(func() { listModels = old })
	return f
}

// answer is a reply of models, complete.
func answer(models ...messagesapi.ModelInfo) func(context.Context, string) ([]messagesapi.ModelInfo, bool, error) {
	return func(context.Context, string) ([]messagesapi.ModelInfo, bool, error) { return models, true, nil }
}

// setNow sets the writer's clock to at for the rest of the test.
func setNow(t *testing.T, at time.Time) {
	t.Helper()
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })
}

// setSeam sets one of the package's test hooks for the rest of the test.
func setSeam(t *testing.T, seam *func(), f func()) {
	t.Helper()
	old := *seam
	*seam = f
	t.Cleanup(func() { *seam = old })
}

func TestMain(m *testing.M) {
	// No test reaches Anthropic (P30): a test whose fetch is the real
	// client's goes to a loopback port nothing serves, unless it starts its
	// own fake and says so.
	_ = os.Setenv(messagesapi.TestAPIEnv, "http://127.0.0.1:9")
	os.Exit(m.Run())
}

func model(id string) messagesapi.ModelInfo {
	adaptive := true
	return messagesapi.ModelInfo{ID: id, DisplayName: "Claude " + id, CreatedAt: t0.Add(-24 * time.Hour), Line: "opus",
		MaxInputTokens: 1000000, MaxTokens: 128000, Efforts: []string{"low", "medium", "high"}, ImageInput: true,
		AdaptiveThinking: &adaptive, Lifecycle: "active"}
}

// readList is dir's list file as this package wrote it.
func readList(t *testing.T, dir string) List {
	t.Helper()
	b, err := os.ReadFile(path(dir))
	if err != nil {
		t.Fatal(err)
	}
	var l List
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// writeList lays down a list for key, fetched at, with models.
func writeList(t *testing.T, dir, key string, at time.Time, models ...messagesapi.ModelInfo) {
	t.Helper()
	b, err := json.Marshal(List{Version: modeltable.AnthropicModelsVersion, FetchedAt: at, KeyID: modeltable.AnthropicKeyID(key), Models: models})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func ids(models []messagesapi.ModelInfo) []string {
	var out []string
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// TestFetchWritesTheList (A5): a forced fetch writes the account's list —
// version, the time, the key's id and never the key, 0600 in a directory it
// makes 0700 — without the models already retired (P25: judged by the
// writer's clock), and answers how many it lists. The control: a model that
// retires later is kept.
func TestFetchWritesTheList(t *testing.T) {
	setNow(t, t0)
	gone, later := model("claude-gone"), model("claude-later")
	gone.RetiresAt, later.RetiresAt = t0, t0.Add(time.Hour)
	f := useFake(t, answer(model("claude-opus-5-5"), gone, later))
	dir := filepath.Join(t.TempDir(), "native")
	n, err := Fetch(context.Background(), dir, keyA)
	if err != nil || n != 2 || f.calls.Load() != 1 {
		t.Fatalf("Fetch = %d, %v (%d calls)", n, err, f.calls.Load())
	}
	l := readList(t, dir)
	if l.Version != modeltable.AnthropicModelsVersion || !l.FetchedAt.Equal(t0) || l.KeyID != modeltable.AnthropicKeyID(keyA) ||
		!slices.Equal(ids(l.Models), []string{"claude-opus-5-5", "claude-later"}) {
		t.Fatalf("the list = %+v", l)
	}
	for p, want := range map[string]os.FileMode{dir: 0o700, path(dir): 0o600} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: %v, %v; want %04o", p, info, err, want)
		}
	}
	if b, _ := os.ReadFile(path(dir)); strings.Contains(string(b), keyA) {
		t.Fatal("the list holds the key")
	}
}

// TestABadReplyKeepsTheList (P10, A5): an incomplete reply, an empty one,
// one whose every model has retired, one too large to keep and a failure each
// write nothing — the list there stays, byte for byte — with the error
// naming why. The control: a good reply replaces it.
func TestABadReplyKeepsTheList(t *testing.T) {
	setNow(t, t0)
	retired := model("claude-retired")
	retired.RetiresAt = t0.Add(-time.Hour)
	big := make([]messagesapi.ModelInfo, 0, 4000)
	for i := range 4000 {
		m := model(fmt.Sprintf("claude-big-%04d", i))
		m.DisplayName = strings.Repeat("x", 120)
		big = append(big, m)
	}
	failed := errors.New("the API is down")
	for _, tc := range []struct {
		name  string
		reply func(context.Context, string) ([]messagesapi.ModelInfo, bool, error)
		want  error
	}{
		{"incomplete", func(context.Context, string) ([]messagesapi.ModelInfo, bool, error) {
			return []messagesapi.ModelInfo{model("claude-x")}, false, nil
		}, ErrIncomplete},
		{"empty", answer(), ErrEmpty},
		{"every model retired", answer(retired), ErrEmpty},
		{"too large", answer(big...), ErrTooLarge},
		{"a failure", func(context.Context, string) ([]messagesapi.ModelInfo, bool, error) { return nil, false, failed }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeList(t, dir, keyA, t0.Add(-48*time.Hour), model("claude-old"))
			before, _ := os.ReadFile(path(dir))
			useFake(t, tc.reply)
			for name, call := range map[string]func() error{
				"Fetch":   func() error { _, err := Fetch(context.Background(), dir, keyA); return err },
				"Refresh": func() error { _, err := Refresh(context.Background(), dir, keyA, MaxAge); return err },
			} {
				err := call()
				if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || (tc.want == nil && !strings.Contains(err.Error(), "the API is down")) {
					t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
				}
				if after, _ := os.ReadFile(path(dir)); string(after) != string(before) {
					t.Fatalf("%s replaced the list", name)
				}
			}
		})
	}
	// The control.
	dir := t.TempDir()
	writeList(t, dir, keyA, t0.Add(-48*time.Hour), model("claude-old"))
	useFake(t, answer(model("claude-new")))
	if _, err := Fetch(context.Background(), dir, keyA); err != nil || !slices.Equal(ids(readList(t, dir).Models), []string{"claude-new"}) {
		t.Fatalf("a good reply: %v, list %q", err, ids(readList(t, dir).Models))
	}
}

// TestRefreshFetchesOnlyWhenDue (§3.6): the background refresh fetches when
// the list is missing, unreadable, another key's, a day old or more, or
// further in the future than the clock's slack, and not when it is this
// key's and fresh — then it answers false and makes no request.
func TestRefreshFetchesOnlyWhenDue(t *testing.T) {
	setNow(t, t0)
	for _, tc := range []struct {
		name  string
		setup func(dir string)
		fetch bool
	}{
		{"fresh", func(dir string) { writeList(t, dir, keyA, t0.Add(-time.Hour), model("claude-a")) }, false},
		{"fresh, a little in the future", func(dir string) { writeList(t, dir, keyA, t0.Add(clockSkew/2), model("claude-a")) }, false},
		{"missing", func(string) {}, true},
		{"another key's", func(dir string) { writeList(t, dir, keyB, t0.Add(-time.Hour), model("claude-a")) }, true},
		{"a day old", func(dir string) { writeList(t, dir, keyA, t0.Add(-MaxAge), model("claude-a")) }, true},
		{"far in the future", func(dir string) { writeList(t, dir, keyA, t0.Add(time.Hour), model("claude-a")) }, true},
		{"with no model", func(dir string) { writeList(t, dir, keyA, t0.Add(-time.Hour)) }, true},
		{"unreadable", func(dir string) { _ = os.WriteFile(path(dir), []byte("{"), 0o600) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(dir)
			f := useFake(t, answer(model("claude-b")))
			fetched, err := Refresh(context.Background(), dir, keyA, MaxAge)
			if err != nil || fetched != tc.fetch || (f.calls.Load() == 1) != tc.fetch {
				t.Fatalf("Refresh = %v, %v (%d calls); want fetched %v", fetched, err, f.calls.Load(), tc.fetch)
			}
			if tc.fetch && !slices.Equal(ids(readList(t, dir).Models), []string{"claude-b"}) {
				t.Fatalf("the list = %q", ids(readList(t, dir).Models))
			}
		})
	}
}

// TestHostsOpeningAtOnceFetchOnce (P8, A5): a second refresh that finds the
// lock held waits for it, checks again under it, finds the list the first
// wrote, and fetches nothing. The control: a second refresh for another key
// fetches its own.
func TestHostsOpeningAtOnceFetchOnce(t *testing.T) {
	setNow(t, t0)
	for _, tc := range []struct {
		name       string
		second     string
		wantCalls  int32
		secondGets bool
	}{
		{"the same key", keyA, 1, false},
		{"another key", keyB, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fetching, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f := useFake(t, func(ctx context.Context, key string) ([]messagesapi.ModelInfo, bool, error) {
				if key == keyA {
					once.Do(func() { close(fetching) })
					<-release
				}
				return []messagesapi.ModelInfo{model("claude-" + key[len(key)-1:])}, true, nil
			})
			waiting := make(chan struct{})
			setSeam(t, &onLockBusy, func() { close(waiting) })
			first := make(chan error, 1)
			go func() { _, err := Refresh(context.Background(), dir, keyA, MaxAge); first <- err }()
			<-fetching // the first holds the lock, mid-fetch
			second := make(chan struct {
				fetched bool
				err     error
			}, 1)
			go func() {
				fetched, err := Refresh(context.Background(), dir, tc.second, MaxAge)
				second <- struct {
					fetched bool
					err     error
				}{fetched, err}
			}()
			<-waiting // the second waits on the lock
			close(release)
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			got := <-second
			if got.err != nil || got.fetched != tc.secondGets || f.calls.Load() != tc.wantCalls {
				t.Fatalf("second = %+v, calls %d; want fetched %v, %d calls", got, f.calls.Load(), tc.secondGets, tc.wantCalls)
			}
		})
	}
}

// TestFetchNeverOverwritesANewerList (P8, A5): a forced fetch whose reply
// arrives after another host wrote a list for the same key — fetched after
// this one began — leaves that list; one written before it began, or for
// another key, it replaces.
func TestFetchNeverOverwritesANewerList(t *testing.T) {
	setNow(t, t0)
	for _, tc := range []struct {
		name   string
		key    string
		at     time.Time
		stands bool
	}{
		{"a newer list for the key", keyA, t0.Add(time.Second), true},
		{"an older list for the key", keyA, t0.Add(-time.Second), false},
		{"a newer list for another key", keyB, t0.Add(time.Second), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			useFake(t, answer(model("claude-mine")))
			setSeam(t, &afterReply, func() { writeList(t, dir, tc.key, tc.at, model("claude-theirs")) })
			n, err := Fetch(context.Background(), dir, keyA)
			if err != nil || n != 1 {
				t.Fatalf("Fetch = %d, %v", n, err)
			}
			want := "claude-mine"
			if tc.stands {
				want = "claude-theirs"
			}
			if got := ids(readList(t, dir).Models); !slices.Equal(got, []string{want}) {
				t.Fatalf("the list = %q, want %q", got, want)
			}
		})
	}
}

// TestRetirementIsJudgedUnderTheLock (P25, C4 review 2): a model's
// retires_at is judged by the clock read once the list's lock is held — the
// time the file's fetched_at says — not when the reply came: a forced fetch
// that waits for the lock past a model's retirement leaves that model out, and
// one whose every model retired meanwhile writes nothing (ErrEmpty, the list
// there kept); so does a refresh. The control: a model that retires after the
// write is kept.
func TestRetirementIsJudgedUnderTheLock(t *testing.T) {
	clock := t0
	old := now
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = old })
	soon, later := model("claude-soon"), model("claude-later")
	soon.RetiresAt, later.RetiresAt = t0.Add(time.Second), t0.Add(time.Hour)
	waited := func() { clock = t0.Add(time.Minute) } // the reply came at t0; the lock was held a minute later
	for _, tc := range []struct {
		name  string
		seam  *func()
		call  func(dir string) (int, error)
		reply []messagesapi.ModelInfo
		want  []string // nil: nothing written
	}{
		{"Fetch", &afterReply, func(dir string) (int, error) { return Fetch(context.Background(), dir, keyA) },
			[]messagesapi.ModelInfo{model("claude-a"), soon, later}, []string{"claude-a", "claude-later"}},
		{"Fetch, every model retired", &afterReply, func(dir string) (int, error) { return Fetch(context.Background(), dir, keyA) },
			[]messagesapi.ModelInfo{soon}, nil},
		{"Refresh", &inLock, func(dir string) (int, error) {
			fetched, err := Refresh(context.Background(), dir, keyA, MaxAge)
			return map[bool]int{true: 2}[fetched], err
		}, []messagesapi.ModelInfo{model("claude-a"), soon, later}, []string{"claude-a", "claude-later"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock = t0
			dir := t.TempDir()
			writeList(t, dir, keyA, t0.Add(-48*time.Hour), model("claude-old"))
			useFake(t, answer(tc.reply...))
			setSeam(t, tc.seam, waited)
			n, err := tc.call(dir)
			if tc.want == nil {
				if !errors.Is(err, ErrEmpty) || !slices.Equal(ids(readList(t, dir).Models), []string{"claude-old"}) {
					t.Fatalf("err = %v, list %q; want ErrEmpty and the old list kept", err, ids(readList(t, dir).Models))
				}
				return
			}
			l := readList(t, dir)
			if err != nil || n != len(tc.want) || !slices.Equal(ids(l.Models), tc.want) || !l.FetchedAt.Equal(t0.Add(time.Minute)) {
				t.Fatalf("%d, %v: list %q fetched at %v; want %q at the lock's time", n, err, ids(l.Models), l.FetchedAt, tc.want)
			}
		})
	}
}

// TestNoKeyNoRequest: no key, or a value too short to be one, sends nothing.
func TestNoKeyNoRequest(t *testing.T) {
	f := useFake(t, answer(model("claude-a")))
	for _, key := range []string{"", "  ", "short"} {
		if _, err := Fetch(context.Background(), t.TempDir(), key); !errors.Is(err, ErrNoKey) {
			t.Fatalf("Fetch(%q) = %v", key, err)
		}
		if _, err := Refresh(context.Background(), t.TempDir(), key, MaxAge); !errors.Is(err, ErrNoKey) {
			t.Fatalf("Refresh(%q) = %v", key, err)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatalf("%d requests", f.calls.Load())
	}
}

// TestErrorsCarryNoKey (P13, A5): whatever a fetch's failure says — the key
// plain, JSON-escaped or percent-encoded in its text, inside an API error's
// message, type and request id — neither Fetch nor Refresh returns it, and
// what a caller classifies by survives: the context's error, and the API
// error's status. The control: the failures themselves do carry the key.
func TestErrorsCarryNoKey(t *testing.T) {
	const weird = "sk-ant-canary/with+odd&chars\"0001"
	quoted, _ := json.Marshal(weird)
	for _, tc := range []struct {
		name  string
		key   string
		cause error
		check func(t *testing.T, err error)
	}{
		{"plain, wrapping the context's error", keyA, fmt.Errorf("dial with %s: %w", keyA, context.DeadlineExceeded), func(t *testing.T, err error) {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost the context's class: %v", err)
			}
		}},
		{"JSON-escaped and percent-encoded", weird, fmt.Errorf("body %s, url ?k=%s, path /%s", quoted, url.QueryEscape(weird), url.PathEscape(weird)), nil},
		{"an API error", keyA, &messagesapi.Error{StatusCode: http.StatusUnauthorized, Type: "authentication_error",
			Message: "invalid x-api-key " + keyA, RequestID: "req_" + keyA}, func(t *testing.T, err error) {
			apiErr, ok := errors.AsType[*messagesapi.Error](err)
			if !ok || !apiErr.AuthError() || strings.Contains(apiErr.Message+apiErr.RequestID+apiErr.Error(), keyA) {
				t.Fatalf("the API error = %+v (%v)", apiErr, ok)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.cause.Error(), strings.TrimSpace(tc.key)[:12]) {
				t.Fatalf("the control: the failure %q does not carry the key", tc.cause)
			}
			useFake(t, func(context.Context, string) ([]messagesapi.ModelInfo, bool, error) { return nil, false, tc.cause })
			for name, call := range map[string]func() error{
				"Fetch":   func() error { _, err := Fetch(context.Background(), t.TempDir(), tc.key); return err },
				"Refresh": func() error { _, err := Refresh(context.Background(), t.TempDir(), tc.key, MaxAge); return err },
			} {
				err := call()
				if err == nil {
					t.Fatalf("%s: no error", name)
				}
				for _, form := range []string{tc.key, string(quoted[1 : len(quoted)-1]), url.QueryEscape(tc.key), url.PathEscape(tc.key)} {
					if strings.Contains(err.Error(), form) {
						t.Fatalf("%s: %q carries the key (%q)", name, err, form)
					}
				}
				if !strings.Contains(err.Error(), redacted) {
					t.Fatalf("%s: %q says nothing was taken out", name, err)
				}
				if tc.check != nil {
					tc.check(t, err)
				}
			}
		})
	}
}

// TestTheRealClient: the default fetch is the Messages API client against
// this process's origin — here a fake on loopback, named by the test
// override (P21, P30): it lists, the key goes in x-api-key, and a 401 that
// echoes the key comes back an AuthError without it. A test override that is
// not a loopback URL is refused before any request.
func TestTheRealClient(t *testing.T) {
	setNow(t, t0)
	var gotKey atomic.Value
	reject := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey.Store(r.Header.Get("x-api-key"))
		if reject.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key `+keyA+`"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"type":"model","id":"claude-opus-5-5","display_name":"Claude Opus 5.5","line":"opus","created_at":"2026-09-21T16:24:00Z","capabilities":{"image_input":{"supported":true}}}],"has_more":false}`)
	}))
	defer srv.Close()
	t.Setenv(messagesapi.TestAPIEnv, srv.URL)
	dir := t.TempDir()
	if n, err := Fetch(context.Background(), dir, keyA); err != nil || n != 1 || gotKey.Load() != keyA {
		t.Fatalf("Fetch = %d, %v (key sent %v)", n, err, gotKey.Load() == keyA)
	}
	if l := readList(t, dir); len(l.Models) != 1 || l.Models[0].DisplayName != "Claude Opus 5.5" || !l.Models[0].ImageInput {
		t.Fatalf("the list = %+v", l)
	}
	reject.Store(true)
	_, err := Fetch(context.Background(), dir, keyA)
	if apiErr, ok := errors.AsType[*messagesapi.Error](err); !ok || !apiErr.AuthError() || strings.Contains(err.Error(), keyA) {
		t.Fatalf("a 401: %v", err)
	}
	t.Setenv(messagesapi.TestAPIEnv, "https://api.example.com")
	if _, err := Fetch(context.Background(), dir, keyA); !errors.Is(err, messagesapi.ErrOverride) {
		t.Fatalf("an override off loopback: %v", err)
	}
}

// TestTheFileShapePin (P19): what this package writes is what modeltable
// reads — every field of the client's ModelInfo comes back through
// ReadAnthropicModels as it was, the two types carry the same JSON names, and
// a load with the key makes a model of the entry. The control: a list whose
// names are not the reader's comes back without those fields, so the
// comparison is not blind to them.
func TestTheFileShapePin(t *testing.T) {
	setNow(t, t0)
	adaptive := false
	full := messagesapi.ModelInfo{ID: "claude-new-7", DisplayName: "Claude New 7", CreatedAt: t0.Add(-time.Hour), Line: "sonnet",
		MaxInputTokens: 300000, MaxTokens: 32000, Efforts: []string{"low", "max"}, ImageInput: true,
		AdaptiveThinking: &adaptive, Lifecycle: "deprecated", RetiresAt: t0.Add(48 * time.Hour)}
	useFake(t, answer(full))
	dir := t.TempDir()
	if _, err := Fetch(context.Background(), dir, keyA); err != nil {
		t.Fatal(err)
	}
	doc, err := modeltable.ReadAnthropicModels(dir)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != modeltable.AnthropicModelsVersion || !doc.FetchedAt.Equal(t0) || doc.KeyID != modeltable.AnthropicKeyID(keyA) || len(doc.Models) != 1 {
		t.Fatalf("read back = %+v", doc)
	}
	same := func(m modeltable.AnthropicModel) bool {
		return m.ID == full.ID && m.DisplayName == full.DisplayName && m.CreatedAt.Equal(full.CreatedAt) && m.Line == full.Line &&
			m.MaxInputTokens == full.MaxInputTokens && m.MaxTokens == full.MaxTokens && slices.Equal(m.Efforts, full.Efforts) &&
			m.ImageInput == full.ImageInput && m.AdaptiveThinking != nil && *m.AdaptiveThinking == *full.AdaptiveThinking &&
			m.Lifecycle == full.Lifecycle && m.RetiresAt.Equal(full.RetiresAt)
	}
	if !same(doc.Models[0]) {
		t.Fatalf("read back %+v\nwrote %+v", doc.Models[0], full)
	}
	if w, r := jsonNames(reflect.TypeFor[messagesapi.ModelInfo]()), jsonNames(reflect.TypeFor[modeltable.AnthropicModel]()); !slices.Equal(w, r) {
		t.Fatalf("the writer's names %q, the reader's %q", w, r)
	}
	if w, r := jsonNames(reflect.TypeFor[List]()), jsonNames(reflect.TypeFor[modeltable.AnthropicModels]()); !slices.Equal(w, r) {
		t.Fatalf("the writer's file %q, the reader's %q", w, r)
	}

	// A load with the key makes the entry a model.
	t.Setenv("ANTHROPIC_API_KEY", keyA)
	tbl, err := modeltable.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := tbl.Models[modeltable.AnthropicAliasPrefix+full.ID]
	if !ok || m.Name != "Claude New 7 (deprecated)" || m.ContextWindow != 300000 || m.MaxOutputTokens != 32000 ||
		!slices.Equal(m.Efforts, []string{"low", "max"}) || !m.Vision || m.Thinking != modeltable.ThinkingNone ||
		m.Family != "sonnet" || m.Released != "2026-10-11" {
		t.Fatalf("loaded %+v (present %v)", m, ok)
	}

	// The control: the reader's names renamed in the file, the fields are lost.
	b, _ := os.ReadFile(path(dir))
	renamed := strings.NewReplacer(`"line"`, `"family"`, `"display_name"`, `"name"`).Replace(string(b))
	if err := os.WriteFile(path(dir), []byte(renamed), 0o600); err != nil {
		t.Fatal(err)
	}
	if doc, err := modeltable.ReadAnthropicModels(dir); err != nil || len(doc.Models) != 1 || same(doc.Models[0]) {
		t.Fatalf("the control: a renamed field still reads back the same (%+v, %v)", doc, err)
	}
}

// jsonNames are t's fields' JSON names, sorted.
func jsonNames(t reflect.Type) []string {
	var out []string
	for f := range t.Fields() {
		out = append(out, strings.Split(f.Tag.Get("json"), ",")[0])
	}
	slices.Sort(out)
	return out
}
