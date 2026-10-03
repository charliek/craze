package chatgptauth

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

// TestFetchModels (P34, plan 033 §3.10): the list keeps only the models shown
// for display (visibility "list"), in priority order, drops an entry whose
// slug or name could not be shown safely, keeps the efforts as listed, and is
// written 0600 bound to the account — its subject and client id — with the
// X-Models-Etag. The raw list's codex prompts are not kept.
func TestFetchModels(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	signIn(t, f, dir)
	m, err := FetchModels(context.Background(), newSource(dir), FetchOptions{ClientVersion: testPin})
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, x := range m.Models {
		slugs = append(slugs, x.Slug)
	}
	if want := "gpt-6.1-sol,gpt-6-astra,gpt-6-sol,gpt-6-luna,gpt-5.6-sol,gpt-5.6-terra,gpt-5.6-luna,gpt-5.5"; strings.Join(slugs, ",") != want {
		t.Fatalf("models = %v, want the eight listed ones in priority order", slugs)
	}
	bySlug := map[string]Model{}
	for _, x := range m.Models {
		bySlug[x.Slug] = x
	}
	astra := bySlug["gpt-6-astra"]
	if astra.DisplayName != "GPT-6-Astra" || astra.ContextWindow != 272000 || astra.DefaultEffort != "medium" ||
		strings.Join(astra.Efforts, ",") != "low,medium,high,xhigh,max,ultra" || strings.Join(astra.InputModalities, ",") != "text,image" ||
		astra.Priority != 2 || astra.ParallelToolCalls == nil || !*astra.ParallelToolCalls {
		t.Fatalf("astra = %+v", astra)
	}
	if luna := bySlug["gpt-5.6-luna"]; luna.ParallelToolCalls == nil || *luna.ParallelToolCalls {
		t.Fatalf("luna's parallel tool calls = %v, want false", luna.ParallelToolCalls)
	}
	if sol := bySlug["gpt-5.6-sol"]; sol.ParallelToolCalls != nil {
		t.Fatal("sol's unstated parallel tool calls became a value")
	}
	// The cache records the version it was fetched with and each model's
	// minimum (plan 034 A1, Q3).
	wantMin := map[string]string{"gpt-6.1-sol": "0.153.0", "gpt-6-astra": "0.153.0", "gpt-6-sol": "0.155.0", "gpt-6-luna": "0.155.0",
		"gpt-5.6-sol": "0.144.0", "gpt-5.6-terra": "0.144.0", "gpt-5.6-luna": "0.144.0", "gpt-5.5": "0.124.0"}
	for slug, min := range wantMin {
		if bySlug[slug].MinClientVersion != min {
			t.Errorf("%s: minimal_client_version = %q, want %q", slug, bySlug[slug].MinClientVersion, min)
		}
	}
	if m.ClientVersion != testPin {
		t.Fatalf("the cache's client_version = %q, want %q", m.ClientVersion, testPin)
	}
	if got := f.versionsSeen(); len(got) != 1 || got[0] != testPin {
		t.Fatalf("the list request's client_version = %v, want [%s]", got, testPin)
	}
	if m.Version != 1 || m.Subject != testSubject || m.ClientID != testClient || m.ModelsEtag != "fake-models-etag-1" ||
		time.Since(m.FetchedAt) > time.Minute {
		t.Fatalf("cache header = %+v", m)
	}
	info, err := os.Stat(ModelsFile(dir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("models file: %v, %v", info, err)
	}
	b, _ := os.ReadFile(ModelsFile(dir))
	if strings.Contains(string(b), "codex prompt") || strings.Contains(string(b), "gpt-reserve") {
		t.Fatal("the cache keeps the raw list's prompt or a hidden model")
	}
	var onDisk map[string]any
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "subject", "client_id", "models_etag", "client_version", "fetched_at", "models"} {
		if _, ok := onDisk[k]; !ok {
			t.Fatalf("the cache lacks %q", k)
		}
	}
	read, err := ReadModels(dir)
	if err != nil || len(read.Models) != 8 || read.ClientID != testClient || read.ClientVersion != testPin {
		t.Fatalf("ReadModels = %+v, %v", read, err)
	}
	f.assertNoLeak(t, string(b))
}

// TestFetchModelsRenewsOnce: a 401 renews the token once and the fetch
// succeeds with the new one; the control is the request count (two lists,
// one refresh).
func TestFetchModelsRenewsOnce(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	seeded := seedSignedIn(t, f, dir, seedOpts{})
	f.mu.Lock()
	delete(f.access, seeded.AccessToken) // the server no longer takes it
	f.mu.Unlock()
	if _, err := FetchModels(context.Background(), newSource(dir), FetchOptions{ClientVersion: testPin}); err != nil {
		t.Fatal(err)
	}
	if _, r, _, m := f.counts(); r != 1 || m != 2 {
		t.Fatalf("refreshes %d, list requests %d; want 1 and 2", r, m)
	}
}

// TestRefreshModels (plan 033 §3.10): a list of the registered account
// fetched less than a day ago is kept; a stale one, a missing one and
// another account's are fetched again; and two refreshes at once — forced:
// the second is seen waiting for the list's lock while the first's request
// is held — fetch once, the second finding the list current under the lock.
func TestRefreshModels(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	signIn(t, f, dir)
	src := newSource(dir)
	ctx := context.Background()

	if got, err := RefreshModels(ctx, src, ModelsMaxAge, FetchOptions{ClientVersion: testPin}); err != nil || !got {
		t.Fatalf("a missing list: fetched %v, %v", got, err)
	}
	if got, err := RefreshModels(ctx, src, ModelsMaxAge, FetchOptions{ClientVersion: testPin}); err != nil || got {
		t.Fatalf("a current list: fetched %v, %v", got, err)
	}
	rewrite := func(edit func(m *Models)) {
		t.Helper()
		m, err := ReadModels(dir)
		if err != nil {
			t.Fatal(err)
		}
		edit(m)
		b, _ := json.Marshal(m)
		if err := os.WriteFile(ModelsFile(dir), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rewrite(func(m *Models) { m.FetchedAt = time.Now().Add(-25 * time.Hour) })
	if got, err := RefreshModels(ctx, src, ModelsMaxAge, FetchOptions{ClientVersion: testPin}); err != nil || !got {
		t.Fatalf("a stale list: fetched %v, %v", got, err)
	}
	rewrite(func(m *Models) { m.Subject = "user-another-account" })
	if got, err := RefreshModels(ctx, src, ModelsMaxAge, FetchOptions{ClientVersion: testPin}); err != nil || !got {
		t.Fatalf("another account's list: fetched %v, %v", got, err)
	}
	if _, _, _, n := f.counts(); n != 3 {
		t.Fatalf("list requests = %d, want 3", n)
	}

	if err := os.Remove(ModelsFile(dir)); err != nil {
		t.Fatal(err)
	}
	arrived, hold := make(chan struct{}, 2), make(chan struct{})
	f.mu.Lock()
	f.modelsArrived, f.holdModels = arrived, hold
	f.mu.Unlock()
	busy := onBusy(t)
	first := make(chan error, 1)
	go func() {
		_, err := RefreshModels(ctx, src, ModelsMaxAge, FetchOptions{ClientVersion: testPin})
		first <- err
	}()
	await(t, arrived, "the first refresh's request")
	second := make(chan bool, 1)
	go func() {
		got, _ := RefreshModels(ctx, src, ModelsMaxAge, FetchOptions{ClientVersion: testPin})
		second <- got
	}()
	await(t, busy, "the second refresh to wait for the list's lock")
	close(hold)
	if err := await(t, first, "the first refresh"); err != nil {
		t.Fatal(err)
	}
	if await(t, second, "the second refresh") {
		t.Fatal("the second refresh fetched again")
	}
	if _, _, _, n := f.counts(); n != 4 {
		t.Fatalf("list requests = %d, want one more", n)
	}
}

// TestReadModelsRefusesOtherFiles: a list of another version, or not JSON,
// is an error; none is fs.ErrNotExist.
func TestReadModelsRefusesOtherFiles(t *testing.T) {
	dir := nativeDir(t)
	if _, err := ReadModels(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no file: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"version":2,"models":[]}`, `not json`} {
		if err := os.WriteFile(ModelsFile(dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadModels(dir); err == nil {
			t.Fatalf("%q was read", body)
		}
	}
	if err := os.WriteFile(ModelsFile(dir), []byte(`{"version":1,"models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadModels(dir); err != nil {
		t.Fatalf("a version-1 list: %v", err)
	}
}

// versionsSeen is the client_version of every model list request so far.
func (f *fakeOpenAI) versionsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.versions...)
}

// TestFetchModelsRequiresAClientVersion (plan 034 Q1, A1): an empty version
// is an error before any request; every list request carries exactly one
// client_version, built into the query; and the control is the fake itself,
// which refuses a request without one (a 400 it records), so the test above
// would fail on a fetch that dropped the parameter.
func TestFetchModelsRequiresAClientVersion(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	signIn(t, f, dir)
	src := newSource(dir)
	ctx := context.Background()
	if _, err := FetchModels(ctx, src, FetchOptions{}); !errors.Is(err, ErrClientVersionRequired) {
		t.Fatalf("FetchModels with no version = %v, want ErrClientVersionRequired", err)
	}
	if _, err := RefreshModels(ctx, src, ModelsMaxAge, FetchOptions{}); !errors.Is(err, ErrClientVersionRequired) {
		t.Fatalf("RefreshModels with no version = %v, want ErrClientVersionRequired", err)
	}
	if _, _, _, n := f.counts(); n != 0 {
		t.Fatalf("an empty version still sent %d request(s)", n)
	}
	if _, err := os.Stat(ModelsFile(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an empty version wrote a list: %v", err)
	}
	// A version that needs escaping is escaped, and arrives whole.
	if _, err := FetchModels(ctx, src, FetchOptions{ClientVersion: "0.160.0&x=1 y"}); err != nil {
		t.Fatal(err)
	}
	if got := f.versionsSeen(); len(got) != 1 || got[0] != "0.160.0&x=1 y" {
		t.Fatalf("an unusual version arrived as %q", got)
	}
	// Control: the fake refuses a request without the parameter.
	ends, err := currentEndpoints()
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _, err := src.token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, _, _, err := ends.apiGET(ctx, ends.api+"/models", tok)
	if err != nil || status != 400 {
		t.Fatalf("control: a request with no client_version = %d, %v; want the fake's 400", status, err)
	}
	if n := f.takeNoVersion(); n != 1 {
		t.Fatalf("control: the fake recorded %d parameterless requests, want 1", n)
	}
}

// TestFakeModelListGatesLikeTheServer (plan 034 Q1, A1): 0.0.1 is an empty
// list, the pin is the eight, and a version between is the models whose
// minimum it meets — the live server's behaviour the fetch is built around.
func TestFakeModelListGatesLikeTheServer(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	signIn(t, f, dir)
	src := newSource(dir)
	slugs := func(v string) string {
		t.Helper()
		m, err := FetchModels(context.Background(), src, FetchOptions{ClientVersion: v})
		if err != nil && !errors.Is(err, ErrModelsEmpty) {
			t.Fatal(err)
		}
		var out []string
		if err == nil {
			for _, x := range m.Models {
				out = append(out, x.Slug)
			}
		}
		return strings.Join(out, ",")
	}
	if got := slugs("9.0.0"); strings.Count(got, ",") != 7 || !strings.Contains(got, "gpt-6.1-sol") {
		t.Fatalf("9.0.0 = %q, want the eight", got)
	}
	if got := slugs("0.154.0"); got != "gpt-6.1-sol,gpt-6-astra,gpt-5.6-sol,gpt-5.6-terra,gpt-5.6-luna,gpt-5.5" {
		t.Fatalf("0.154.0 = %q, want the models whose minimum 0.154.0 meets", got)
	}
	if got := slugs("0.0.1"); got != "" {
		t.Fatalf("0.0.1 = %q, want none (an empty reply keeps the cache: ErrModelsEmpty)", got)
	}
}

// TestRefreshModelsRefetchesAStaleVersion (plan 034 Q3, A1): a cache fetched
// with another client_version, or none (a legacy file), is refetched by
// RefreshModels however fresh it is; one with the pin and fresh is not. The
// control is the request count.
func TestRefreshModelsRefetchesAStaleVersion(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	signIn(t, f, dir)
	src := newSource(dir)
	ctx := context.Background()
	opts := FetchOptions{ClientVersion: testPin}
	rewrite := func(version string) {
		t.Helper()
		m, err := ReadModels(dir)
		if err != nil {
			t.Fatal(err)
		}
		m.ClientVersion = version
		b, _ := json.Marshal(m)
		if err := os.WriteFile(ModelsFile(dir), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := RefreshModels(ctx, src, ModelsMaxAge, opts); err != nil || !got {
		t.Fatalf("a missing list: fetched %v, %v", got, err)
	}
	if got, err := RefreshModels(ctx, src, ModelsMaxAge, opts); err != nil || got {
		t.Fatalf("a fresh list at the pin: fetched %v, %v", got, err)
	}
	for _, other := range []string{"0.159.0", ""} {
		rewrite(other)
		if got, err := RefreshModels(ctx, src, ModelsMaxAge, opts); err != nil || !got {
			t.Fatalf("a fresh list fetched with %q: fetched %v, %v; want it fetched again", other, got, err)
		}
		if m, err := ReadModels(dir); err != nil || m.ClientVersion != testPin {
			t.Fatalf("after the refetch the cache's client_version = %q, %v", m.ClientVersion, err)
		}
	}
	if _, _, _, n := f.counts(); n != 3 {
		t.Fatalf("list requests = %d, want 3 (missing, and two stale versions)", n)
	}
}

// TestAnEmptyReplyKeepsTheCache (plan 034 Q3, A2): a reply with no visible
// model, when the account's cache has some, writes nothing and is
// ErrModelsEmpty naming the pin; with no cache, or another account's, the
// empty list is written as it always was. The control is the file's bytes.
func TestAnEmptyReplyKeepsTheCache(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	signIn(t, f, dir)
	src := newSource(dir)
	ctx := context.Background()

	// No cache: the empty list is written.
	m, err := FetchModels(ctx, src, FetchOptions{ClientVersion: "0.0.1"})
	if err != nil || len(m.Models) != 0 || m.ClientVersion != "0.0.1" {
		t.Fatalf("an empty reply with no cache = %+v, %v; want the empty list written", m, err)
	}
	if read, err := ReadModels(dir); err != nil || len(read.Models) != 0 {
		t.Fatalf("the empty list on disk = %+v, %v", read, err)
	}

	// An empty cache is no cache: a second empty reply writes again.
	if _, err := FetchModels(ctx, src, FetchOptions{ClientVersion: "0.0.2"}); err != nil {
		t.Fatalf("an empty reply over an empty cache: %v", err)
	}

	// A non-empty cache survives an empty reply, and the error names the pin.
	if _, err := FetchModels(ctx, src, FetchOptions{ClientVersion: testPin}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(ModelsFile(dir))
	_, err = FetchModels(ctx, src, FetchOptions{ClientVersion: "0.0.1"})
	if !errors.Is(err, ErrModelsEmpty) || !strings.Contains(err.Error(), "0.0.1") {
		t.Fatalf("an empty reply over a cache = %v, want ErrModelsEmpty naming 0.0.1", err)
	}
	after, _ := os.ReadFile(ModelsFile(dir))
	if string(before) != string(after) {
		t.Fatal("an empty reply rewrote the cache")
	}
	// RefreshModels reports it too: a cache at another version is stale, the
	// refetch comes back empty, and the list stays.
	if got, err := RefreshModels(ctx, src, ModelsMaxAge, FetchOptions{ClientVersion: "0.0.1"}); got || !errors.Is(err, ErrModelsEmpty) {
		t.Fatalf("RefreshModels = %v, %v; want not fetched, ErrModelsEmpty", got, err)
	}
	if after, _ = os.ReadFile(ModelsFile(dir)); string(before) != string(after) {
		t.Fatal("a refresh's empty reply rewrote the cache")
	}

	// Another account's non-empty cache is not this account's to keep.
	other, err := ReadModels(dir)
	if err != nil {
		t.Fatal(err)
	}
	other.Subject = "user-another-account"
	b, _ := json.Marshal(other)
	if err := os.WriteFile(ModelsFile(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if m, err := FetchModels(ctx, src, FetchOptions{ClientVersion: "0.0.1"}); err != nil || len(m.Models) != 0 || m.Subject != testSubject {
		t.Fatalf("an empty reply over another account's cache = %+v, %v; want this account's empty list", m, err)
	}
}

// TestParseModelsKeepsOnlyShortPrintableMinimums (plan 034 §3.1): a model's
// minimal_client_version is kept when it is short printable text, and a
// longer or unprintable one is dropped without dropping the model.
func TestParseModelsKeepsOnlyShortPrintableMinimums(t *testing.T) {
	body := `{"models":[
{"slug":"a","visibility":"list","priority":0,"minimal_client_version":"0.153.0"},
{"slug":"b","visibility":"list","priority":1,"minimal_client_version":"` + strings.Repeat("9", 40) + `"},
{"slug":"c","visibility":"list","priority":2,"minimal_client_version":"1 2\u001b[0m"},
{"slug":"d","visibility":"list","priority":3}]}`
	list, err := parseModels([]byte(body))
	if err != nil || len(list) != 4 {
		t.Fatalf("parseModels = %+v, %v; want all four models", list, err)
	}
	for i, want := range []string{"0.153.0", "", "", ""} {
		if list[i].MinClientVersion != want {
			t.Errorf("model %s: minimum %q, want %q", list[i].Slug, list[i].MinClientVersion, want)
		}
	}
}
