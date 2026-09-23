package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// fakeModels serves a model list and answers chat for the models in works.
func fakeModels(list *atomic.Value, works map[string]bool, auth *atomic.Value) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth != nil {
			auth.Store(r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/models" {
			if l := list.Load().(string); l != "" {
				w.Write([]byte(l))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var b struct{ Model string }
		json.NewDecoder(r.Body).Decode(&b)
		if strings.HasPrefix(b.Model, "busy") {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"Provider returned error"}}`))
			return
		}
		if !works[b.Model] {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"no such model"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
}

func tableOf(t *testing.T, h http.Handler, prov string) map[string]store.ProviderModel {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/providers/"+prov+"/model-table", nil))
	var d struct{ Models []store.ProviderModel }
	json.Unmarshal(rec.Body.Bytes(), &d)
	out := map[string]store.ProviderModel{}
	for _, m := range d.Models {
		out[m.Model] = m
	}
	return out
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return rec
}

func waitIdle(t *testing.T, h http.Handler, prov string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/providers/"+prov+"/model-policy", nil))
		if strings.Contains(rec.Body.String(), `"running":false`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("auto test still running")
}

func TestOnlyFreeWithoutAutoTest(t *testing.T) {
	var list atomic.Value
	list.Store(`{"data":[{"id":"a:free"},{"id":"b"}]}`)
	up := fakeModels(&list, nil, nil)
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	h := New(s, map[string]string{"groq": up.URL})
	if m := tableOf(t, h, "groq"); !m["a:free"].Active || !m["b"].Active {
		t.Fatalf("new models start on: %+v", m)
	}
	post(h, "/providers/groq/model-policy", `{"onlyFree":true}`)
	if m := tableOf(t, h, "groq"); !m["a:free"].Active || m["b"].Active {
		t.Errorf("only free: %+v", m)
	}
	// A new model under only free starts on only when free.
	list.Store(`{"data":[{"id":"a:free"},{"id":"b"},{"id":"c-free"},{"id":"d"}]}`)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/providers/groq/model-table?refresh=1", nil))
	if m := tableOf(t, h, "groq"); !m["c-free"].Active || m["d"].Active {
		t.Errorf("new under only free: %+v", m)
	}
	// Off again: everything back on.
	post(h, "/providers/groq/model-policy", `{"onlyFree":false}`)
	if m := tableOf(t, h, "groq"); !m["b"].Active || !m["d"].Active {
		t.Errorf("only free off: %+v", m)
	}
}

func TestFailedFetchDeletesNothing(t *testing.T) {
	var list atomic.Value
	list.Store(`{"data":[{"id":"a"},{"id":"b"}]}`)
	up := fakeModels(&list, nil, nil)
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	h := New(s, map[string]string{"groq": up.URL})
	tableOf(t, h, "groq")
	list.Store("")
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/providers/groq/model-table?refresh=1", nil))
	if m := tableOf(t, h, "groq"); len(m) != 2 {
		t.Errorf("a failed fetch deleted models: %+v", m)
	}
}

func TestAutoTestSwitchesByResult(t *testing.T) {
	var list atomic.Value
	list.Store(`{"data":[{"id":"good"},{"id":"bad"},{"id":"good:free"},{"id":"bad-free"}]}`)
	up := fakeModels(&list, map[string]bool{"good": true, "good:free": true}, nil)
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	h := New(s, map[string]string{"groq": up.URL})
	tableOf(t, h, "groq")
	post(h, "/providers/groq/model-policy", `{"autoTest":true}`)
	waitIdle(t, h, "groq")
	m := tableOf(t, h, "groq")
	if !m["good"].Active || m["bad"].Active || !m["good:free"].Active || m["bad-free"].Active {
		t.Errorf("auto test: %+v", m)
	}
	if m["good"].TestConn == "" || m["bad"].TestAt == "" {
		t.Errorf("results not recorded: %+v", m["good"])
	}
	// Only free too: the paid ones go off untested, the free ones are tested.
	post(h, "/providers/groq/model-policy", `{"autoTest":true,"onlyFree":true}`)
	waitIdle(t, h, "groq")
	m = tableOf(t, h, "groq")
	if m["good"].Active || !m["good:free"].Active || m["bad-free"].Active {
		t.Errorf("auto test, only free: %+v", m)
	}
	// A new model is tested before it is switched on.
	list.Store(`{"data":[{"id":"good"},{"id":"good:free"},{"id":"new:free"}]}`)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/providers/groq/model-table?refresh=1", nil))
	waitIdle(t, h, "groq")
	m = tableOf(t, h, "groq")
	if b, ok := m["bad"]; !ok || !b.Stale {
		t.Errorf("a dropped model should be kept as stale, not deleted: %+v", m)
	}
	if n := m["new:free"]; n.Active || n.TestAt == "" {
		t.Errorf("new model: %+v", n)
	}
}

func TestAccountTestIsPinned(t *testing.T) {
	var list, auth atomic.Value
	list.Store(`{"data":[{"id":"m"}]}`)
	up := fakeModels(&list, map[string]bool{"m": true}, &auth)
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "one", "k1")
	two, _ := s.CreateConnection("groq", "two", "k2")
	s.SetActive(two.ID, false)
	h := New(s, map[string]string{"groq": up.URL})
	for i := 0; i < 3; i++ {
		rec := post(h, "/accounts/"+two.ID+"/test", `{}`)
		if !strings.Contains(rec.Body.String(), `"ok":true`) || auth.Load() != "Bearer k2" {
			t.Fatalf("account test went to %v: %s", auth.Load(), rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/account-tests", nil))
	if !strings.Contains(rec.Body.String(), two.ID) || !strings.Contains(rec.Body.String(), `"model":"m"`) {
		t.Errorf("account tests = %s", rec.Body.String())
	}
	// A model test says which account answered.
	var res modelTest
	json.Unmarshal(post(h, "/providers/groq/models/test", `{"model":"m"}`).Body.Bytes(), &res)
	if res.Account == "" || res.Account == two.ID {
		t.Errorf("model test answered by %q", res.Account)
	}
}

func TestTypeSafeTestSuite(t *testing.T) {
	answer := `{"model":"jev-1.13.0","answers":{
		"urgent":{"type":"noul","noul":0.97},
		"bug":{"type":"noul","noul":0.04},
		"team":{"type":"choice","choice":"billing","confidence":0.9,"probabilities":{"billing":0.95,"technical":0.03,"sales":0.02}},
		"feeling":{"type":"score","score":1.3,"confidence":0.6,"legend":{"0":"Calm","1":"Frustrated","2":"Very angry"},"probabilities":{"0":0.05,"1":0.6,"2":0.35}}},
		"usage":{"input_tokens":90,"output_tokens":8}}`
	var gotPath, gotType string
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotType = r.URL.Path, r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(answer))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("typesafe", "Jev", "k")
	h := New(s, map[string]string{"typesafe": up.URL})
	rec := post(h, "/accounts/"+c.ID+"/test", `{}`)
	// TypeSafe reads a body without a JSON content type as a string (422).
	if gotPath != "/systemone" || gotType != "application/json" || gotBody["model"] != "jev-latest" || gotBody["questions"] == nil {
		t.Fatalf("upstream got %s %q %v", gotPath, gotType, gotBody)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) || !strings.Contains(rec.Body.String(), "billing") {
		t.Errorf("good answer: %s", rec.Body.String())
	}
	for name, bad := range map[string]string{
		"wrong choice":   strings.Replace(answer, `"choice":"billing"`, `"choice":"sales"`, 1),
		"missing answer": strings.Replace(answer, `"bug"`, `"other"`, 1),
		"wrong noul":     strings.Replace(answer, `"noul":0.97`, `"noul":0.1`, 1),
		"out of range":   strings.Replace(answer, `"score":1.3`, `"score":7`, 1),
		"not json":       `oops`,
	} {
		if _, err := checkTypeSafe([]byte(bad)); err == nil {
			t.Errorf("%s: passed", name)
		}
	}
}

func TestPolicyChangeStopsTheRunningTest(t *testing.T) {
	var paidAfter atomic.Int32
	var switched atomic.Bool
	var ids []string
	for i := 0; i < 30; i++ {
		ids = append(ids, `{"id":"paid-`+string(rune('a'+i%26))+string(rune('a'+i/26))+`"}`)
	}
	ids = append(ids, `{"id":"x:free"}`)
	list := `{"data":[` + strings.Join(ids, ",") + `]}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(list))
			return
		}
		var b struct{ Model string }
		json.NewDecoder(r.Body).Decode(&b)
		if switched.Load() && !strings.Contains(b.Model, "free") {
			paidAfter.Add(1)
		}
		time.Sleep(20 * time.Millisecond)
		w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("openrouter", "o", "k")
	h := New(s, map[string]string{"openrouter": up.URL})
	tableOf(t, h, "openrouter")
	post(h, "/providers/openrouter/model-policy", `{"autoTest":true}`)
	time.Sleep(30 * time.Millisecond)
	post(h, "/providers/openrouter/model-policy", `{"autoTest":true,"onlyFree":true}`)
	switched.Store(true)
	waitIdle(t, h, "openrouter")
	// A request already in flight when the policy changed may land; no new one.
	if n := paidAfter.Load(); n > 3 {
		t.Errorf("%d paid models tested after only free was switched on", n)
	}
	m := tableOf(t, h, "openrouter")
	if !m["x:free"].Active || m["paid-aa"].Active {
		t.Errorf("after the change: free=%+v paid=%+v", m["x:free"], m["paid-aa"])
	}
}

func TestAutoTestRateLimitedGoesOffAndManualTestSetsSwitch(t *testing.T) {
	old := autoTestRetry
	autoTestRetry = time.Millisecond
	defer func() { autoTestRetry = old }()
	var list atomic.Value
	list.Store(`{"data":[{"id":"good"},{"id":"busy"}]}`)
	works := map[string]bool{"good": true}
	up := fakeModels(&list, works, nil)
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("groq", "g", "k")
	h := New(s, map[string]string{"groq": up.URL})
	tableOf(t, h, "groq")
	post(h, "/providers/groq/model-policy", `{"autoTest":true}`)
	waitIdle(t, h, "groq")
	m := tableOf(t, h, "groq")
	if !m["good"].Active || m["busy"].Active || !strings.HasPrefix(m["busy"].TestMsg, "rate limited") {
		t.Errorf("rate limited: %+v", m["busy"])
	}
	// A manual test under auto test sets the switch.
	works["good"] = false
	post(h, "/providers/groq/models/test", `{"model":"good"}`)
	if tableOf(t, h, "groq")["good"].Active {
		t.Error("a failed manual test left the model on")
	}
}
