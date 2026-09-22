package drift

import (
	"path/filepath"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func TestPathsCollapseNamesButKeepFields(t *testing.T) {
	p := Paths([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],
	 "tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
	 "usage":{"attribution":{"items":{"msg_01a0c758abcdef":{"input_tokens":1}}}},"n":1,"stop":null}`))
	for _, want := range []string{"model", "messages[].content[].type", "tools[].function.parameters.properties.{*}.type",
		"usage.attribution.items.{*}.input_tokens", "n"} {
		if _, ok := p[want]; !ok {
			t.Errorf("missing %s in %v", want, p)
		}
	}
	if p["n"] != "number" || p["stop"] != "null" {
		t.Errorf("types: n=%s stop=%s", p["n"], p["stop"])
	}
	for k := range p {
		if k == "tools[].function.parameters.properties.city" {
			t.Errorf("a parameter name leaked into the paths")
		}
	}
}

func TestSplitSSEByType(t *testing.T) {
	ev := SplitSSE([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"a\":1}\n\ndata: {\"choices\":[]}\n\ndata: [DONE]\n\n"))
	if len(ev) != 2 || ev[0].Name != "response.created" || ev[1].Name != "" {
		t.Fatalf("events = %+v", ev)
	}
}

func TestObserverRecordsAddedTypeAndRemoved(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	o := New(s)
	send := func(body string) { o.Observe(Request, "groq", "chat/completions", []byte(body), false); o.Drain() }
	for i := 0; i < learnObservations; i++ {
		send(`{"model":"m","messages":[],"temperature":1}`)
	}
	if c, _ := s.ListShapeChanges(store.ShapeChangeFilter{}); len(c) != 0 {
		t.Fatalf("changes while learning: %+v", c)
	}
	send(`{"model":"m","messages":[],"temperature":1,"x_new":true}`)
	send(`{"model":"m","messages":[],"temperature":"hot"}`)
	for i := 0; i < goneMinSeen+goneAfter; i++ {
		send(`{"model":"m","messages":[]}`)
	}
	c, _ := s.ListShapeChanges(store.ShapeChangeFilter{})
	kinds := map[string]string{}
	for _, x := range c {
		kinds[x.Path+":"+x.Kind] = x.NewType
	}
	if _, ok := kinds["x_new:added"]; !ok {
		t.Errorf("added not recorded: %v", kinds)
	}
	if kinds["temperature:type"] != "string" {
		t.Errorf("type change not recorded: %v", kinds)
	}
	// temperature was in 5 of 5 early documents but only 5 times: too few to
	// call it gone; the rule needs goneMinSeen sightings first.
	if _, ok := kinds["temperature:removed"]; ok {
		t.Errorf("removed too early: %v", kinds)
	}
	o.Flush()
	o2 := New(s)
	if f := o2.Fields(Request, "groq", ""); len(f) < 3 {
		t.Errorf("fields not persisted: %+v", f)
	}
}

func TestObserverRecordsRemovedAfterARun(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	o := New(s)
	for i := 0; i < goneMinSeen+2; i++ {
		o.Observe(Response, "codex", "responses", []byte(`{"id":"r","usage":{"input_tokens":1}}`), false)
	}
	for i := 0; i < goneAfter+1; i++ {
		o.Observe(Response, "codex", "responses", []byte(`{"id":"r"}`), false)
	}
	o.Drain()
	c, _ := s.ListShapeChanges(store.ShapeChangeFilter{Direction: Response})
	found := false
	for _, x := range c {
		if x.Path == "usage.input_tokens" && x.Kind == "removed" {
			found = true
		}
	}
	if !found {
		t.Errorf("removal not recorded: %+v", c)
	}
}
