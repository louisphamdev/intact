package drift

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// Tuning. A key learns silently for its first observations; a field counts
// as gone when a field that was nearly always there has been missing for a
// run of observations.
const (
	learnObservations = 3
	goneAfter         = 20
	goneMinSeen       = 20
	goneMinRatio      = 0.9
	queueSize         = 256
	flushEvery        = 30 * time.Second
	sampleLimit       = 300
)

// Directions of an observed document.
const (
	Request  = "request"  // what a client sent to intact
	Response = "response" // what a provider answered
)

type field struct {
	typ      string
	seen     int64
	firstObs int64
	lastObs  int64
	gone     bool
	lastAt   string
	dirty    bool
}

type key struct {
	obs    int64
	fields map[string]*field
	dirty  bool
}

type job struct {
	dir, provider, endpoint string
	body                    []byte
	sse                     bool
}

// Observer learns structures and records their changes. Observe never blocks
// a request: when the queue is full, the document is skipped.
type Observer struct {
	store *store.Store
	q     chan job
	mu    sync.Mutex
	keys  map[string]*key
}

// New starts an observer that loads what was learned before.
func New(s *store.Store) *Observer {
	o := &Observer{store: s, q: make(chan job, queueSize), keys: map[string]*key{}}
	if counts, fields, err := s.LoadShapes(); err == nil {
		for k, n := range counts {
			o.keys[k] = &key{obs: n, fields: map[string]*field{}}
		}
		for _, f := range fields {
			kk := o.keys[f.Key]
			if kk == nil {
				kk = &key{fields: map[string]*field{}}
				o.keys[f.Key] = kk
			}
			kk.fields[f.Path] = &field{typ: f.Type, seen: f.Seen, firstObs: f.FirstObs, lastObs: f.LastObs, gone: f.Gone, lastAt: f.LastAt}
		}
	} else {
		log.Printf("drift: load: %v", err)
	}
	go o.run()
	return o
}

// Observe queues one document. body is copied by the caller or not reused.
func (o *Observer) Observe(dir, provider, endpoint string, body []byte, sse bool) {
	if o == nil || len(body) == 0 {
		return
	}
	select {
	case o.q <- job{dir, provider, endpoint, body, sse}:
	default:
	}
}

func (o *Observer) run() {
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case j := <-o.q:
			o.process(j)
		case <-t.C:
			o.Flush()
		}
	}
}

// keyOf joins the parts of a structure key.
func keyOf(dir, provider, endpoint, event string) string {
	return dir + "|" + provider + "|" + endpoint + "|" + event
}

func (o *Observer) process(j job) {
	var events []Event
	if j.sse {
		events = SplitSSE(j.body)
	} else {
		events = []Event{{Body: j.body}}
	}
	for _, ev := range events {
		paths := Paths(ev.Body)
		if paths == nil {
			continue
		}
		o.observe(j.dir, j.provider, j.endpoint, ev.Name, paths, ev.Body)
	}
}

func (o *Observer) observe(dir, provider, endpoint, event string, paths map[string]string, body []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	k := keyOf(dir, provider, endpoint, event)
	kk := o.keys[k]
	if kk == nil {
		kk = &key{fields: map[string]*field{}}
		o.keys[k] = kk
	}
	kk.obs++
	kk.dirty = true
	now := time.Now().UTC().Format(time.RFC3339)
	learning := kk.obs <= learnObservations
	var changes []store.ShapeChange
	change := func(path, kind, oldT, newT string) {
		if learning {
			return
		}
		changes = append(changes, store.ShapeChange{At: now, Direction: dir, Provider: provider, Endpoint: endpoint,
			Event: event, Path: path, Kind: kind, OldType: oldT, NewType: newT, Sample: sample(body)})
	}
	for p, t := range paths {
		f := kk.fields[p]
		if f == nil {
			kk.fields[p] = &field{typ: t, seen: 1, firstObs: kk.obs, lastObs: kk.obs, lastAt: now, dirty: true}
			change(p, "added", "", t)
			continue
		}
		if f.gone {
			f.gone = false
			change(p, "returned", "", t)
		}
		if t != f.typ && t != "null" && !strings.Contains(f.typ, t) {
			change(p, "type", f.typ, t)
			f.typ = t
		}
		f.seen++
		f.lastObs = kk.obs
		f.lastAt = now
		f.dirty = true
	}
	for p, f := range kk.fields {
		if f.gone || kk.obs-f.lastObs < goneAfter || f.seen < goneMinSeen {
			continue
		}
		span := f.lastObs - f.firstObs + 1
		if float64(f.seen)/float64(span) >= goneMinRatio {
			f.gone = true
			f.dirty = true
			change(p, "removed", f.typ, "")
		}
	}
	for _, c := range changes {
		if err := o.store.AddShapeChange(c); err != nil {
			log.Printf("drift: %v", err)
		}
	}
}

// sample keeps the start of the document a change was seen in.
func sample(b []byte) string {
	if len(b) > sampleLimit {
		return string(b[:sampleLimit]) + "…"
	}
	return string(b)
}

// Flush writes the learned counters to the database.
func (o *Observer) Flush() {
	if o == nil {
		return
	}
	o.mu.Lock()
	counts := map[string]int64{}
	var fields []store.ShapeField
	for k, kk := range o.keys {
		if kk.dirty {
			counts[k] = kk.obs
			kk.dirty = false
		}
		for p, f := range kk.fields {
			if f.dirty {
				fields = append(fields, store.ShapeField{Key: k, Path: p, Type: f.typ, Seen: f.seen,
					FirstObs: f.firstObs, LastObs: f.lastObs, Gone: f.gone, LastAt: f.lastAt})
				f.dirty = false
			}
		}
	}
	o.mu.Unlock()
	if len(counts) == 0 && len(fields) == 0 {
		return
	}
	if err := o.store.SaveShapes(counts, fields); err != nil {
		log.Printf("drift: save: %v", err)
	}
}

// Fields returns what is known of the keys matching a direction and provider
// ("" matches any), for the API and MCP.
func (o *Observer) Fields(dir, provider, endpoint string) []store.ShapeField {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []store.ShapeField
	for k, kk := range o.keys {
		parts := strings.SplitN(k, "|", 4)
		if len(parts) != 4 || (dir != "" && parts[0] != dir) || (provider != "" && parts[1] != provider) || (endpoint != "" && parts[2] != endpoint) {
			continue
		}
		for p, f := range kk.fields {
			out = append(out, store.ShapeField{Key: k, Path: p, Type: f.typ, Seen: f.seen, FirstObs: f.firstObs,
				LastObs: f.lastObs, Gone: f.gone, LastAt: f.lastAt})
		}
	}
	return out
}

// Drain processes what is queued; tests use it to wait for the observer.
func (o *Observer) Drain() {
	for {
		select {
		case j := <-o.q:
			o.process(j)
		default:
			return
		}
	}
}
