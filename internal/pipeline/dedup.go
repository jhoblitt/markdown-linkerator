package pipeline

import (
	"net/url"
	"sync"

	"github.com/jhoblitt/markdown-linkerator/internal/cache"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

// urlState is the shared per-URL dedup record. Its mutex makes "complete +
// snapshot occurrences" atomic against "append occurrence", which is what
// guarantees exactly-once emit under a streaming pipeline.
type urlState struct {
	mu     sync.Mutex
	done   bool
	result model.Result // canonical outcome; its Target is the sample occurrence
	occs   []model.Target
}

// dedup is a mutex-guarded seen-map. It deliberately holds no channels so the
// pipeline's channel graph stays acyclic (a result never flows back upstream).
type dedup struct {
	mu    sync.Mutex
	seen  map[string]*urlState
	cache *cache.Cache
}

func newDedup(c *cache.Cache) *dedup {
	return &dedup{seen: map[string]*urlState{}, cache: c}
}

// add registers an occurrence of key. On the first occurrence it either returns
// a job to enqueue (cache miss) or emits a cache-hit result; on a later
// occurrence it emits immediately if the URL is already done, else records the
// occurrence for the completing worker to fan out to.
func (d *dedup) add(key string, t model.Target) (job *model.CheckJob, emit []model.Result) {
	d.mu.Lock()
	st, ok := d.seen[key]
	if !ok {
		if ent, hit := d.cache.Get(key); hit {
			st = &urlState{done: true, result: resultFromCache(t, ent)}
			d.seen[key] = st
			d.mu.Unlock()
			return nil, []model.Result{resultFor(t, st.result)}
		}
		st = &urlState{occs: []model.Target{t}}
		d.seen[key] = st
		d.mu.Unlock()
		return &model.CheckJob{Key: key, Host: hostOf(t.URL), Sample: t}, nil
	}
	d.mu.Unlock()

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.done {
		return nil, []model.Result{resultFor(t, st.result)}
	}
	st.occs = append(st.occs, t)
	return nil, nil
}

// complete records the canonical result for key and returns every occurrence
// registered so far so the caller can emit one result per occurrence.
func (d *dedup) complete(key string, res model.Result) []model.Target {
	d.mu.Lock()
	st := d.seen[key]
	d.mu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.done = true
	st.result = res
	occs := st.occs
	st.occs = nil
	return occs
}

// resultFor projects a canonical result onto a specific occurrence, preserving
// the per-occurrence Target (file/line) while sharing the outcome fields.
func resultFor(occ model.Target, base model.Result) model.Result {
	return model.Result{
		Target:     occ,
		State:      base.State,
		StatusCode: base.StatusCode,
		Host:       base.Host,
		FromCache:  base.FromCache,
		Detail:     base.Detail,
	}
}

func resultFromCache(t model.Target, ent cache.Entry) model.Result {
	return model.Result{
		Target:     t,
		State:      ent.State,
		StatusCode: ent.StatusCode,
		Host:       hostOf(t.URL),
		FromCache:  true,
	}
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
