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
	// newer than the file already there. The goroutine's alone.
	last []modelcache.Model
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
// models and they are not the ones last recorded. A model modelcache would not
// write — no id, text that is not one line — is left out of it. A failure is
// one line on the host's log, and the next install tries again.
func (r *catalogRecorder) record() {
	snap := r.snapshot()
	at := r.now()
	var models []modelcache.Model
	for _, m := range agent.CatalogModels(snap) {
		mc := modelcache.Model{ID: m.ID}
		if m.Name != m.ID {
			mc.Name = m.Name
		}
		if mc.Valid() {
			models = append(models, mc)
		}
	}
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
