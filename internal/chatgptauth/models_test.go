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
	m, err := FetchModels(context.Background(), newSource(dir))
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, x := range m.Models {
		slugs = append(slugs, x.Slug)
	}
	if strings.Join(slugs, ",") != "gpt-6-astra,gpt-5.6-sol,gpt-5.6-luna" {
		t.Fatalf("models = %v, want the listed ones in priority order", slugs)
	}
	astra := m.Models[0]
	if astra.DisplayName != "GPT-6-Astra" || astra.ContextWindow != 272000 || astra.DefaultEffort != "medium" ||
		strings.Join(astra.Efforts, ",") != "low,medium,high,xhigh,max,ultra" || strings.Join(astra.InputModalities, ",") != "text,image" ||
		astra.Priority != 2 || astra.ParallelToolCalls == nil || !*astra.ParallelToolCalls {
		t.Fatalf("astra = %+v", astra)
	}
	if luna := m.Models[2]; luna.ParallelToolCalls == nil || *luna.ParallelToolCalls {
		t.Fatalf("luna's parallel tool calls = %v, want false", luna.ParallelToolCalls)
	}
	if sol := m.Models[1]; sol.ParallelToolCalls != nil {
		t.Fatal("sol's unstated parallel tool calls became a value")
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
	for _, k := range []string{"version", "subject", "client_id", "models_etag", "fetched_at", "models"} {
		if _, ok := onDisk[k]; !ok {
			t.Fatalf("the cache lacks %q", k)
		}
	}
	read, err := ReadModels(dir)
	if err != nil || len(read.Models) != 3 || read.ClientID != testClient {
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
	if _, err := FetchModels(context.Background(), newSource(dir)); err != nil {
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

	if got, err := RefreshModels(ctx, src, ModelsMaxAge); err != nil || !got {
		t.Fatalf("a missing list: fetched %v, %v", got, err)
	}
	if got, err := RefreshModels(ctx, src, ModelsMaxAge); err != nil || got {
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
	if got, err := RefreshModels(ctx, src, ModelsMaxAge); err != nil || !got {
		t.Fatalf("a stale list: fetched %v, %v", got, err)
	}
	rewrite(func(m *Models) { m.Subject = "user-another-account" })
	if got, err := RefreshModels(ctx, src, ModelsMaxAge); err != nil || !got {
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
		_, err := RefreshModels(ctx, src, ModelsMaxAge)
		first <- err
	}()
	await(t, arrived, "the first refresh's request")
	second := make(chan bool, 1)
	go func() {
		got, _ := RefreshModels(ctx, src, ModelsMaxAge)
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
