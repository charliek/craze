package cli

import (
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/modelcache"
	"github.com/charliek/craze/internal/rundir"
)

// catalogRecorder is craze serve's half of the model catalog cache (plan 030
// §3.14): after each catalog install its session reports — the engine's
// Options.CatalogChanged, a kick from inside the log's publishing boundary —
// the host reads the session's snapshot and records the catalog it advertises
// (agent.CatalogModels) in <Home>/.cache/craze/catalogs/<provider>.json
// (internal/modelcache, under the cache tree rundir.CatalogDir validates). All
// of it on a goroutine of its own: no lock of the engine's or the session's is
// held across the snapshot's read, the per-provider lock, or the write, and
// the observer is never kept waiting on any of them.
//
// The observation time is taken as the snapshot is read, so of two hosts of
// one provider the later reading is the one the cache keeps, whichever
// finishes writing last (modelcache.Write, R2-12). A catalog the same as the
// last one this host recorded — an install that moved an effort, not the
// models — is not written again.
//
// Only a provider craze runs over ACP has one: native's models are its model
// table, read where they are needed, and it never reaches the cache.
type catalogRecorder struct {
	env      rundir.Env
	provider string
	snapshot func() agent.Snapshot
	now      func() time.Time
	log      io.Writer

	kick chan struct{}
	stop chan struct{}
	done chan struct{}
	once sync.Once

	// last is the catalog this host last recorded: written, or found no
	// newer than the file already there. refused is the last catalog it
	// would not record (record), so an install that leaves it as it was says
	// so again on the log no more than it writes a catalog again. Both the
	// goroutine's alone.
	last    []modelcache.Model
	refused []modelcache.Model
}

// newCatalogRecorder starts p's recorder, reading the session's catalog
// through snapshot and saying on log what it could not write; nil for a
// provider whose catalog is not cached (in process, or an id that cannot
// name a file).
func newCatalogRecorder(env rundir.Env, p agent.Provider, snapshot func() agent.Snapshot, log io.Writer) *catalogRecorder {
	if p.InProcess() || !modelcache.ValidProvider(p.Name()) {
		return nil
	}
	r := &catalogRecorder{
		env: env, provider: p.Name(), snapshot: snapshot, now: time.Now, log: log,
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	go r.run()
	return r
}

// changed is the engine's CatalogChanged: a kick for the goroutine, never a
// wait — a kick already pending covers this one too, since the goroutine
// reads the snapshot as it stands when it gets there. nil does nothing.
func (r *catalogRecorder) changed() {
	if r == nil {
		return
	}
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// close stops the recorder — once it has recorded a kick still pending, so a
// catalog installed just before the host stopped is not lost — and returns
// when its goroutine has. nil, and every call after the first, only waits.
func (r *catalogRecorder) close() {
	if r == nil {
		return
	}
	r.once.Do(func() { close(r.stop) })
	<-r.done
}

func (r *catalogRecorder) run() {
	defer close(r.done)
	for {
		select {
		case <-r.kick:
			r.record()
		case <-r.stop:
			select {
			case <-r.kick:
				r.record()
			default:
			}
			return
		}
	}
}

// record reads the session's catalog and writes it to the cache when it has
// models and they are not the ones last recorded. A catalog with any model
// modelcache would not write — no id, text that is not one line — is not
// recorded at all (plan 030 C15r, sol r31-c16 2): with that model left out,
// the list's /model would later offer the rest as though it were the whole
// catalog, so the cache keeps what it had, and one line on the host's log
// says which model it was (once for a catalog unchanged since). A write that
// fails is one line on the log too, and the next install tries again.
func (r *catalogRecorder) record() {
	snap := r.snapshot()
	at := r.now()
	all := agent.CatalogModels(snap)
	models := make([]modelcache.Model, 0, len(all))
	bad := -1
	for i, m := range all {
		mc := modelcache.Model{ID: m.ID}
		if m.Name != m.ID {
			mc.Name = m.Name
		}
		if bad < 0 && !mc.Valid() {
			bad = i
		}
		models = append(models, mc)
	}
	if bad >= 0 {
		if !slices.Equal(models, r.refused) {
			r.refused = models
			fmt.Fprintf(r.log, "craze serve: the model catalog cache: not recorded: model %d of %d (id %q, name %q) cannot be cached\n",
				bad+1, len(models), logClip(models[bad].ID), logClip(models[bad].Name))
		}
		return
	}
	r.refused = nil
	if len(models) == 0 || slices.Equal(models, r.last) {
		return
	}
	dir, err := rundir.CatalogDir(r.env, true)
	if err == nil {
		_, err = modelcache.Write(dir, modelcache.Catalog{ObservedAt: at, Provider: r.provider, Models: models})
	}
	if err != nil {
		fmt.Fprintf(r.log, "craze serve: the model catalog cache: %v\n", err)
		return
	}
	r.last = models
}

// logClipMax bounds an agent's own text quoted on a line of the host's log.
const logClipMax = 80

// logClip is s cut to logClipMax bytes, for the host's log: an agent's text
// is its own, and any length.
func logClip(s string) string {
	if len(s) <= logClipMax {
		return s
	}
	return s[:logClipMax] + "…"
}
