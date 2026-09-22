package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// modelPolicy is how a provider's models are switched on without the
// operator: with AutoTest, a model is on only when a test call works; with
// OnlyFree, only models whose name says free are on (and tested).
type modelPolicy struct {
	AutoTest   bool   `json:"autoTest"`
	OnlyFree   bool   `json:"onlyFree"`
	LastRun    string `json:"lastRun,omitempty"`
	LastResult string `json:"lastResult,omitempty"`
}

const (
	autoTestEvery   = 6 * time.Hour
	fetchEvery      = time.Hour
	autoLoopTick    = 10 * time.Minute
	autoTestWorkers = 3
	autoTestRetry   = 15 * time.Second
)

// autoState tracks the providers being auto-tested, one run at a time each.
// A run can be cancelled (the policy changed under it); cancel waits until
// it has stopped, so the next run or switch change is not undone by it.
type autoState struct {
	mu      sync.Mutex
	running map[string]*autoRun
}

type autoRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *autoState) start(prov string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[prov] != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	s.running[prov] = &autoRun{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	return true
}

func (s *autoState) run(prov string) *autoRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[prov]
}

func (s *autoState) done(prov string) {
	s.mu.Lock()
	if r := s.running[prov]; r != nil {
		r.cancel()
		close(r.done)
	}
	delete(s.running, prov)
	s.mu.Unlock()
}

// stop cancels a provider's run and waits for it to end.
func (s *autoState) stop(prov string) {
	if r := s.run(prov); r != nil {
		r.cancel()
		<-r.done
	}
}

func (s *autoState) isRunning(prov string) bool { return s.run(prov) != nil }

func policyKey(prov string) string { return "model-policy:" + prov }

func (a *api) modelPolicy(prov string) modelPolicy {
	var p modelPolicy
	if v, _ := a.store.GetSetting(policyKey(prov)); v != "" {
		json.Unmarshal([]byte(v), &p)
	}
	return p
}

func (a *api) saveModelPolicy(prov string, p modelPolicy) error {
	b, _ := json.Marshal(p)
	return a.store.SetSetting(policyKey(prov), string(b))
}

// isFree reports whether a model's name says it is free (OpenRouter's
// ":free" models, "…-free" variants).
func isFree(model string) bool { return strings.Contains(strings.ToLower(model), "free") }

// wanted reports whether a policy lets a model be on at all.
func (p modelPolicy) wanted(model string) bool { return !p.OnlyFree || isFree(model) }

// startsOn decides the switch of a model seen for the first time: on, unless
// it waits for its test or is not free under OnlyFree.
func (p modelPolicy) startsOn(model string) bool { return !p.AutoTest && p.wanted(model) }

// applyPolicy sets every model's switch from the policy: without AutoTest,
// on when wanted; with AutoTest, a full test run in the background.
func (a *api) applyPolicy(prov string, p modelPolicy) error {
	if p.AutoTest {
		// Marked running before the reply, so a caller polling sees the run.
		if a.auto.start(prov) {
			go a.autoTestHeld(prov, nil)
		}
		return nil
	}
	rows, err := a.store.ListModels(prov)
	if err != nil {
		return err
	}
	var on, off []string
	for _, r := range rows {
		if p.wanted(r.Model) {
			on = append(on, r.Model)
		} else {
			off = append(off, r.Model)
		}
	}
	if _, err := a.store.SetModelsActive(prov, on, true); err != nil {
		return err
	}
	_, err = a.store.SetModelsActive(prov, off, false)
	return err
}

// autoTest fetches the provider's list again (which drops the models it no
// longer lists), tests each wanted model and switches it on when it answers,
// off when it does not. A model refused for rate limit keeps its switch. With
// models, only those are tested and the list is not fetched again.
func (a *api) autoTest(prov string, models []string) {
	if a.auto.start(prov) {
		a.autoTestHeld(prov, models)
	}
}

// autoTestHeld is autoTest once the provider is marked running.
func (a *api) autoTestHeld(prov string, models []string) {
	defer a.auto.done(prov)
	ctx := a.auto.run(prov).ctx
	p := a.modelPolicy(prov)
	full := models == nil
	if full {
		a.refreshCatalog(ctx, prov)
		rows, err := a.store.ListModels(prov)
		if err != nil {
			log.Printf("autotest %s: %v", prov, err)
			return
		}
		for _, r := range rows {
			models = append(models, r.Model)
		}
	}
	var test, off []string
	for _, m := range models {
		if p.wanted(m) {
			test = append(test, m)
		} else {
			off = append(off, m)
		}
	}
	a.store.SetModelsActive(prov, off, false)
	var mu sync.Mutex
	works, next := 0, 0
	var wg sync.WaitGroup
	for w := 0; w < autoTestWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= len(test) || ctx.Err() != nil {
					mu.Unlock()
					return
				}
				m := test[next]
				next++
				mu.Unlock()
				res := a.runModelTest(ctx, prov, m, "")
				if res.Status == http.StatusTooManyRequests {
					select {
					case <-time.After(autoTestRetry):
						res = a.runModelTest(ctx, prov, m, "")
					case <-ctx.Done():
					}
				}
				if ctx.Err() != nil {
					return
				}
				a.store.RecordModelTest(prov, m, res.OK, res.Ms, res.Message, res.Account)
				if res.Status == http.StatusTooManyRequests {
					continue
				}
				a.store.SetModelsActive(prov, []string{m}, res.OK)
				if res.OK {
					mu.Lock()
					works++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if full && ctx.Err() == nil {
		p = a.modelPolicy(prov)
		p.LastRun = time.Now().UTC().Format(time.RFC3339)
		p.LastResult = fmt.Sprintf("%d of %d tested work", works, len(test))
		a.saveModelPolicy(prov, p)
	}
}

// autoTestLoop keeps model lists current without a client asking: every
// autoLoopTick it fetches the list of each provider with an active account
// whose last fetch is older than fetchEvery (a failed fetch is retried on the
// next tick), then re-tests every provider with AutoTest on once its last run
// is older than autoTestEvery. Model lists change over days, so an hourly
// fetch catches a new or dropped model soon enough for one GET per provider.
func (a *api) autoTestLoop() {
	time.Sleep(time.Minute)
	for {
		provs := map[string]bool{}
		if conns, err := a.store.ListConnections(); err == nil {
			for _, c := range conns {
				if _, ok := a.providerFor(c); ok && c.IsActive {
					provs[c.Provider] = true
				}
			}
		}
		for prov := range provs {
			a.cat.mu.Lock()
			e, hit := a.cat.m[prov]
			a.cat.mu.Unlock()
			if !hit || !e.ok || time.Since(e.at) >= fetchEvery {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				a.refreshCatalog(ctx, prov)
				cancel()
			}
			p := a.modelPolicy(prov)
			if !p.AutoTest {
				continue
			}
			if t, err := time.Parse(time.RFC3339, p.LastRun); err == nil && time.Since(t) < autoTestEvery {
				continue
			}
			a.autoTest(prov, nil)
		}
		time.Sleep(autoLoopTick)
	}
}

// getModelPolicy serves a provider's policy and whether a test run is going.
func (a *api) getModelPolicy(w http.ResponseWriter, r *http.Request) {
	prov := r.PathValue("id")
	writeJSON(w, map[string]any{"policy": a.modelPolicy(prov), "running": a.auto.isRunning(prov)})
}

// setModelPolicy stores {"autoTest":bool,"onlyFree":bool} and applies it.
func (a *api) setModelPolicy(w http.ResponseWriter, r *http.Request) {
	var b modelPolicy
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return
	}
	prov := r.PathValue("id")
	p, err := a.updatePolicy(prov, b.AutoTest, b.OnlyFree)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot save the policy")
		return
	}
	writeJSON(w, map[string]any{"policy": p, "running": a.auto.isRunning(prov)})
}

func (a *api) updatePolicy(prov string, autoTest, onlyFree bool) (modelPolicy, error) {
	// A run under the old policy would undo the new one.
	a.auto.stop(prov)
	p := a.modelPolicy(prov)
	p.AutoTest, p.OnlyFree = autoTest, onlyFree
	if err := a.saveModelPolicy(prov, p); err != nil {
		return p, err
	}
	return p, a.applyPolicy(prov, p)
}
