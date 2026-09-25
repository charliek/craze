package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// A client's lifecycle in the receipts table (plan 027 §3.6, SF-11, the
// engine's half of A12): released when its connection goes, claimed back by a
// resume, and retired only once it has been released for the age bound AND
// holds no entry, open or completed. The server's binding table — generations,
// tokens, compare-and-release — is internal/control's and is tested there.
//
// Every clock here is the table's own, injected (newClockedRig): retirement is
// measured on real elapsed time, and a test that slept ten minutes to prove it
// would be slower than the bound it proves.

// bystander is another client whose commands apply the table's age bound:
// every command runs evictAgedLocked first (admit), and retirement happens
// there. Its command is a Disarm with nothing armed — a gate refusal, which is
// forgotten — so it touches nothing but the pass, and the same id serves again.
type bystander struct {
	e      *Engine
	client string
}

func newBystander(e *Engine) *bystander { return &bystander{e: e, client: e.NewClientID()} }

func (b *bystander) pass(t *testing.T) {
	t.Helper()
	if err := b.e.Disarm(Command{Client: b.client, ID: "1"}); !errors.Is(err, ErrNotAccepting) {
		t.Fatalf("the bystander's disarm: %v, want the gate refusal that stores nothing", err)
	}
}

// isClient reports whether the table still keeps name, without touching its
// lifecycle: a claim would un-release it, which is not a question a test may
// ask of a client whose release it is watching.
func isClient(rt *receiptTable, name string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	_, ok := rt.clients[name]
	return ok
}

// wantTableConsistent holds the table's bookkeeping against itself: every
// client's count of entries is byKey's own, every entry belongs to a client
// the table keeps, and the released set is exactly the clients marked
// released. A count that drifted would let a client with an open reservation
// be retired, or keep one alive for good.
func wantTableConsistent(t *testing.T, rt *receiptTable) {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	counted := map[string]int{}
	for key := range rt.byKey {
		if _, ok := rt.clients[key.client]; !ok {
			t.Errorf("entry %s belongs to a client the table does not keep", key)
		}
		counted[key.client]++
	}
	for name, st := range rt.clients {
		if st.entries != counted[name] {
			t.Errorf("%s counts %d entries, byKey holds %d", name, st.entries, counted[name])
		}
		if _, in := rt.released[name]; in != st.released {
			t.Errorf("%s is released=%v, and in the released set=%v", name, st.released, in)
		}
	}
	for name, st := range rt.released {
		if rt.clients[name] != st {
			t.Errorf("%s is in the released set and not the table's client", name)
		}
	}
}

// TestAReleasedClientIsReclaimed: a released client is kept exactly as it was
// — its answers replay and its commands run — and a claim makes it live again,
// after which no amount of time retires it. Its mark survives the whole round
// trip, so an id of its that aged out while it was released is still unknown
// and never a fresh execution.
func TestAReleasedClientIsReclaimed(t *testing.T) {
	clock := newTestClock(epoch())
	r := newClockedRig(t, Options{}, clock.now)
	by := newBystander(r.e)
	client := r.e.NewClientID()
	first := Command{Client: client, ID: "1"}
	if err := r.e.SetTitle(first, "before"); err != nil {
		t.Fatal(err)
	}

	r.e.ReleaseClient(client)
	r.e.ReleaseClient(client) // idempotent
	if err := r.e.SetTitle(first, "before"); err != nil {
		t.Fatalf("a released client's stored answer: %v, want it replayed", err)
	}
	clock.advance(receiptAge / 2)
	if err := r.e.ClaimClient(client); err != nil {
		t.Fatalf("claiming a released client: %v", err)
	}

	// Live again: however long passes now, the pass never retires it.
	for range 5 {
		clock.advance(2 * receiptAge)
		by.pass(t)
	}
	if !isClient(r.e.receipts, client) {
		t.Fatal("a reclaimed client was retired")
	}
	if err := r.e.ClaimClient(client); err != nil {
		t.Fatalf("claiming it again: %v, want the no-op a live client's claim is", err)
	}
	if err := r.e.SetTitle(first, "before"); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("its aged-out id: %v, want ErrUnknownCommand — the mark kept, never a re-execution", err)
	}
	if err := r.e.SetTitle(Command{Client: client, ID: "2"}, "after"); err != nil {
		t.Fatalf("a new command of the reclaimed client: %v", err)
	}
	wantTableConsistent(t, r.e.receipts)
}

// TestAClientWithARunningCommandIsNotRetired is GLM 8 (plan 027 §3.6): a
// command still running when its connection went keeps its client alive,
// released and past the age bound, until the command finishes AND its answer
// ages out — so a resend after a resume always finds the answer. The open
// reservation is in byKey and never in the completed order the age pass walks,
// which is the whole of why the predicate counts byKey.
//
// Twice: a blocking Set parked in the settings worker at the provider, and a
// synchronous SetTitle held at the session's door.
func TestAClientWithARunningCommandIsNotRetired(t *testing.T) {
	type running struct {
		// start runs the command on a goroutine and returns once it is parked
		// with its reservation open; finish lets it through.
		start func(t *testing.T, r *rig, c Command) (finish func(), done <-chan error)
		// resend is the same command again, with a context already dead: an
		// open reservation answers ErrCommandInProgress at once, a completed one
		// replays its answer, and a retired client is ErrBadRequest.
		resend func(r *rig, c Command) error
	}
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		running
	}{
		{"a blocking Set parked in the settings worker", running{
			start: func(t *testing.T, r *rig, c Command) (func(), <-chan error) {
				release := r.s.holdNextSets()
				done := make(chan error, 1)
				go func() {
					_, err := r.e.Set(context.Background(), c, modeSetting("plan"))
					done <- err
				}()
				waitFor(t, r.heldSets)
				return release, done
			},
			resend: func(r *rig, c Command) error {
				_, err := r.e.Set(dead, c, modeSetting("plan"))
				return err
			},
		}},
		{"a synchronous SetTitle held at the session", running{
			start: func(t *testing.T, r *rig, c Command) (func(), <-chan error) {
				entered, release := r.s.holdNextTitle()
				done := make(chan error, 1)
				go func() { done <- r.e.SetTitle(c, "held") }()
				await(t, entered, "the SetTitle to reach the session")
				return release, done
			},
			resend: func(r *rig, c Command) error { return r.e.SetTitle(c, "held") },
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newTestClock(epoch())
			r := newClockedRig(t, Options{}, clock.now)
			by := newBystander(r.e)
			client := r.e.NewClientID()
			c := Command{Client: client, ID: "1"}

			finish, done := tc.start(t, r, c)
			r.e.ReleaseClient(client)
			clock.advance(receiptAge + time.Minute)
			by.pass(t)
			if !isClient(r.e.receipts, client) {
				t.Fatal("a released client was retired while its command was still running")
			}
			if err := tc.resend(r, c); !errors.Is(err, ErrCommandInProgress) {
				t.Fatalf("a resend while it runs: %v, want ErrCommandInProgress — the reservation found", err)
			}

			finish()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the command: %v", err)
				}
			case <-time.After(watchdog):
				t.Fatal("the command never finished")
			}
			// Finished now, a moment ago on the table's clock: its answer is
			// fresh, so the client stays, and a resend replays the answer.
			by.pass(t)
			if err := tc.resend(r, c); err != nil {
				t.Fatalf("a resend once it finished: %v, want its stored answer", err)
			}
			if !isClient(r.e.receipts, client) {
				t.Fatal("a released client was retired while it held a fresh answer")
			}

			// The answer ages out, and nothing of the client's is left: retired.
			clock.advance(receiptAge + time.Second)
			by.pass(t)
			if isClient(r.e.receipts, client) {
				t.Fatal("a released client holding nothing was not retired past the bound")
			}
			if err := tc.resend(r, c); !errors.Is(err, ErrBadRequest) {
				t.Fatalf("a retired client's resend: %v, want ErrBadRequest", err)
			}
			wantTableConsistent(t, r.e.receipts)
		})
	}
}

// TestAClientWithAnUnexpiredReceiptIsNotRetired: released for longer than the
// bound, a client whose last answer is younger than it is kept — its answer
// is still answerable — and goes in the very pass that ages the answer out.
func TestAClientWithAnUnexpiredReceiptIsNotRetired(t *testing.T) {
	clock := newTestClock(epoch())
	r := newClockedRig(t, Options{}, clock.now)
	by := newBystander(r.e)
	client := r.e.NewClientID()
	r.e.ReleaseClient(client)

	// Released and not yet retirable, its commands still run: one a handler
	// had already dispatched when the connection went.
	clock.advance(receiptAge / 2)
	late := Command{Client: client, ID: "1"}
	if err := r.e.SetTitle(late, "late"); err != nil {
		t.Fatalf("a released client's command: %v", err)
	}

	clock.advance(receiptAge/2 + time.Minute)
	by.pass(t)
	if err := r.e.SetTitle(late, "late"); err != nil {
		t.Fatalf("its answer, released for longer than the bound: %v, want it replayed", err)
	}

	clock.advance(receiptAge / 2)
	by.pass(t)
	if isClient(r.e.receipts, client) {
		t.Fatal("the client outlived its last answer")
	}
	wantTableConsistent(t, r.e.receipts)
}

// TestARetiredClientCannotBeClaimed: once retired, a client is gone for good.
// A claim is ErrUnknownClient — the claim applies the age pass first, so it
// retires a client that has met the predicate rather than reviving it, whether
// or not a command ran the pass since — and every command naming it, an old id
// or a new one, is ErrBadRequest, the answer for a client never minted. A
// second release never postpones the retirement, and the name is never minted
// again.
func TestARetiredClientCannotBeClaimed(t *testing.T) {
	clock := newTestClock(epoch())
	r := newClockedRig(t, Options{}, clock.now)
	client := r.e.NewClientID()
	if err := r.e.SetTitle(Command{Client: client, ID: "1"}, "t"); err != nil {
		t.Fatal(err)
	}
	r.e.ReleaseClient(client)
	clock.advance(receiptAge / 2)
	r.e.ReleaseClient(client) // keeps the first release's time
	clock.advance(receiptAge/2 + time.Second)

	err := r.e.ClaimClient(client)
	if !errors.Is(err, ErrUnknownClient) || Code(err) != "bad_request" || Reason(err) != "bad_request" {
		t.Fatalf("claiming a retired client: %v (%s/%s), want ErrUnknownClient, bad_request/bad_request",
			err, Code(err), Reason(err))
	}
	for _, id := range []string{"1", "2"} {
		if err := r.e.SetTitle(Command{Client: client, ID: id}, "t"); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("a retired client's command %s: %v, want ErrBadRequest", id, err)
		}
	}
	r.e.ReleaseClient(client) // a no-op for a name the table no longer keeps
	if err := r.e.ClaimClient(client); !errors.Is(err, ErrUnknownClient) {
		t.Fatalf("a second claim: %v, want ErrUnknownClient", err)
	}
	if next := r.e.NewClientID(); next == client {
		t.Fatalf("the retired name %q was minted again", client)
	}
	if err := r.e.ClaimClient("c-999"); !errors.Is(err, ErrUnknownClient) {
		t.Fatalf("claiming a name never minted: %v, want ErrUnknownClient", err)
	}
	wantTableConsistent(t, r.e.receipts)
}

// TestALiveClientIsNeverRetired: a client that is never released — the
// in-process TUI's — is kept however old it grows, idle and holding nothing.
// Its mark is what keeps its aged-out ids unknown, and it stays.
func TestALiveClientIsNeverRetired(t *testing.T) {
	clock := newTestClock(epoch())
	r := newClockedRig(t, Options{}, clock.now)
	by := newBystander(r.e)
	live := r.e.NewClientID()
	if err := r.e.SetTitle(Command{Client: live, ID: "1"}, "t"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		clock.advance(2 * receiptAge)
		by.pass(t)
	}
	if !isClient(r.e.receipts, live) {
		t.Fatal("a client that was never released was retired")
	}
	if err := r.e.SetTitle(Command{Client: live, ID: "1"}, "t"); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("its aged-out id: %v, want ErrUnknownCommand", err)
	}
	if err := r.e.SetTitle(Command{Client: live, ID: "2"}, "t"); err != nil {
		t.Fatalf("its next command: %v", err)
	}
	wantTableConsistent(t, r.e.receipts)
}

// TestReleaseClaimAndRetireRaceCommands drives the lifecycle against commands
// on every side at once, under -race. Each worker is a connection: it issues
// commands under its client — some that run, some a gate refuses (forgotten),
// some resent — and now and then drops, stays away for a while, and resumes
// with a claim; a claim that finds its client retired starts over with a fresh
// one, as the server's `resumed: false` does. Meanwhile other connections'
// releases and claims land on any client at all, racing its owner's commands,
// and the table's clock runs past the bound again and again, moved by the work
// rather than the scheduler.
//
// What must hold throughout: a claim is nil or ErrUnknownClient; a retired
// client never comes back — nothing that names it succeeds again — and at the
// end every client's count is byKey's own and every entry belongs to a client
// the table keeps. The dedicated tests above pin the predicate's schedules;
// this one is for the race detector and the bookkeeping.
func TestReleaseClaimAndRetireRaceCommands(t *testing.T) {
	clock := newTestClock(epoch())
	rt := newReceiptTable(&receiptHooks{now: clock.now})
	const connections, steps = 6, 400

	var (
		mu      sync.Mutex
		minted  []string
		retired = map[string]bool{}
	)
	mint := func() string {
		name := rt.newClient()
		mu.Lock()
		defer mu.Unlock()
		minted = append(minted, name)
		return name
	}
	anyClient := func(rng *rand.Rand) string {
		mu.Lock()
		defer mu.Unlock()
		return minted[rng.IntN(len(minted))]
	}
	wasRetired := func(name string) bool {
		mu.Lock()
		defer mu.Unlock()
		return retired[name]
	}
	markRetired := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		retired[name] = true
	}
	var ran, resumed, restarted atomic.Int64
	// claim is a resume of name, checked: it may find the client gone, and a
	// client found gone must stay gone.
	claim := func(name string) bool {
		before := wasRetired(name)
		switch err := rt.claim(name); {
		case err == nil:
			if before {
				t.Errorf("%s was claimed back after it was retired", name)
			}
			return true
		case errors.Is(err, ErrUnknownClient):
			markRetired(name)
			return false
		default:
			t.Errorf("claim(%s): %v", name, err)
			return false
		}
	}

	var workers sync.WaitGroup
	for w := range connections {
		workers.Add(1)
		go func() {
			defer workers.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 7))
			name, n, away := mint(), 0, 0
			for step := range steps {
				clock.advance(receiptAge / 100)
				if step%20 == 0 {
					// Mid-run as well as at the end: an entry orphaned by a
					// client retired too soon ages out if nobody looks in time.
					wantTableConsistent(t, rt)
				}
				if rng.IntN(25) == 0 {
					// Another connection's release or resume, on any client.
					if other := anyClient(rng); rng.IntN(2) == 0 {
						rt.release(other)
					} else {
						claim(other)
					}
				}
				if away > 0 {
					if away--; away == 0 {
						if claim(name) {
							resumed.Add(1)
						} else {
							name, n = mint(), 0
							restarted.Add(1)
						}
					}
					continue
				}
				if rng.IntN(20) == 0 {
					rt.release(name)
					away = 1 + rng.IntN(80)
					continue
				}
				n++
				id := n
				if n > 1 && rng.IntN(4) == 0 {
					id = n - 1 // a resend
				}
				c := Command{Client: name, ID: strconv.Itoa(id)}
				before := wasRetired(name)
				refuse := rng.IntN(3) == 0
				_, err := withSyncReceipt(rt, c, receiptHash("SetTitle", "t"), func() (string, error) {
					runtime.Gosched()
					if refuse {
						return "", ErrNotAccepting
					}
					return "ran", nil
				})
				switch {
				case err == nil, errors.Is(err, ErrNotAccepting), errors.Is(err, ErrUnknownCommand):
					if before {
						t.Errorf("%s answered %v after its client was retired", c.Cause(), err)
					}
					if err == nil {
						ran.Add(1)
					}
				case errors.Is(err, ErrBadRequest):
					// Another connection released it, and it aged out holding
					// nothing while its own commands were all refused: gone, and
					// this connection starts over.
					markRetired(name)
					name, n = mint(), 0
					restarted.Add(1)
				default:
					t.Errorf("%s: %v", c.Cause(), err)
				}
			}
		}()
	}
	workers.Wait()
	wantTableConsistent(t, rt)
	if ran.Load() == 0 {
		t.Fatal("no command ran: the schedule exercised nothing")
	}
	t.Logf("%d commands ran; %d resumes; %d clients found retired and replaced", ran.Load(), resumed.Load(), restarted.Load())
}

// TestASubmitResultCarriesTheTextItStarted is plan 027 §3.13 (astra 19): the
// result of a submit that started a turn names the text the turn started
// with — for a queued row, the row's text as the section that took it read it,
// which another client may have edited after this one looked — so a client
// draws the sent row from what was sent. A queued or armed result has no
// started text, and a resend replays the whole result, Text included.
func TestASubmitResultCarriesTheTextItStarted(t *testing.T) {
	r := newRig(t, Options{})
	a, b := r.e.NewClientID(), r.e.NewClientID()

	// A row this client read as "draft", edited by another client since. It
	// stays queued: Queue never starts a turn, and nothing has woken the driver.
	row := r.queue("draft")
	if err := r.e.EditQueued(Command{Client: b, ID: "1"}, row.ID, "edited by b", nil); err != nil {
		t.Fatal(err)
	}
	c := Command{Client: a, ID: "1"}
	res, err := r.e.Submit(c, "draft", SubmitQueue, row.ID)
	if err != nil || res.Turn == "" || res.Text != "edited by b" {
		t.Fatalf("a submit of an edited row: %+v, %v; want a turn and the row's own text", res, err)
	}
	if again, err := r.e.Submit(c, "draft", SubmitQueue, row.ID); err != nil || again != res {
		t.Fatalf("the resend: %+v, %v; want the first result, Text included", again, err)
	}
	r.until(lastEnding)

	typed, err := r.e.Submit(Command{Client: a, ID: "2"}, "typed", SubmitQueue, "")
	if err != nil || typed.Turn == "" || typed.Text != "typed" {
		t.Fatalf("a typed submit: %+v, %v; want its own text", typed, err)
	}
	r.until(lastEnding)

	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	queued, err := r.e.Submit(Command{Client: a, ID: "3"}, "later", SubmitQueue, "")
	if err != nil || queued.Queued == nil || queued.Text != "" {
		t.Fatalf("a submit behind a turn: %+v, %v; want a queued row and no started text", queued, err)
	}
	armed, err := r.e.Submit(Command{Client: a, ID: "4"}, "now", SubmitSendNow, "")
	if err != nil || !armed.Armed || armed.Text != "" {
		t.Fatalf("a send-now behind a turn: %+v, %v; want it armed with no started text", armed, err)
	}
	r.until(lastEnding)
	r.wantPrompts("edited by b", "typed", "one", "now", "later")
}

// TestClearQueueAnswersTheRowsItRemoved is plan 027 §3.12 (astra r2 10): a
// clear answers with the rows it took, in queue order and as they stood —
// another client's row this one had not folded among them — so a client's
// overlay can hide exactly those. The slice is the caller's own: writing
// through it reaches neither the table's stored answer nor a resend's copy.
func TestClearQueueAnswersTheRowsItRemoved(t *testing.T) {
	r := newRig(t, Options{})
	a, b := r.e.NewClientID(), r.e.NewClientID()
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")

	x, err := r.e.Queue(Command{Client: a, ID: "1"}, "x")
	if err != nil {
		t.Fatal(err)
	}
	y, err := r.e.Queue(Command{Client: b, ID: "1"}, "y")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.e.EditQueued(Command{Client: a, ID: "2"}, x.ID, "x, edited", nil); err != nil {
		t.Fatal(err)
	}
	clear := Command{Client: a, ID: "3"}
	rows, err := r.e.ClearQueue(clear)
	if err != nil {
		t.Fatal(err)
	}
	want := []agent.QueuedPrompt{{ID: x.ID, Text: "x, edited", QueuedAt: x.QueuedAt, Version: 1}, y}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("clear answered %+v, want %+v", rows, want)
	}
	r.wantRows()

	rows[0].Text = "MUTATED"
	again, err := r.e.ClearQueue(clear)
	if err != nil || fmt.Sprint(again) != fmt.Sprint(want) {
		t.Fatalf("the resend: %+v, %v; want the stored rows, untouched by the first caller", again, err)
	}
	again[1].Text = "MUTATED"
	third, err := r.e.ClearQueue(clear)
	if err != nil || fmt.Sprint(third) != fmt.Sprint(want) {
		t.Fatalf("a second resend: %+v, %v; want the stored rows, untouched by the first resend", third, err)
	}

	empty, err := r.e.ClearQueue(Command{Client: a, ID: "4"})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("clearing an empty queue: %#v, %v; want an empty, non-nil slice", empty, err)
	}
	turn.release()
	r.until(lastEnding)
}
