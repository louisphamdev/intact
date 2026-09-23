package drift

import (
	"path/filepath"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func TestPathsKeepRealSnakeCaseFieldNames(t *testing.T) {
	p := Paths([]byte(`{"top_logprobs":2,"safety_identifier":"u","stop_sequence":"x","system_instruction":{}}`))
	for _, want := range []string{"top_logprobs", "safety_identifier", "stop_sequence", "system_instruction"} {
		if _, ok := p[want]; !ok {
			t.Errorf("missing %s in %v", want, p)
		}
	}
	for k := range p {
		if k == "{*}" {
			t.Errorf("a real field name collapsed: %v", p)
		}
	}
}

func TestPathsStillCollapseGeneratedIDs(t *testing.T) {
	for _, doc := range []string{`{"msg_01AbCdEfGh":1}`, `{"call_abc12345":1}`} {
		p := Paths([]byte(doc))
		if _, ok := p["{*}"]; !ok || len(p) != 1 {
			t.Errorf("%s gave %v", doc, p)
		}
	}
}

// oldShapeFields replaces the table with the one the code before the field-name
// fix wrote: no legacy column. Opening the store again must migrate it.
func oldShapeFields(t *testing.T, s *store.Store, key string) {
	t.Helper()
	for _, q := range []string{
		`DROP TABLE shape_fields`,
		`CREATE TABLE shape_fields (key TEXT NOT NULL, path TEXT NOT NULL, type TEXT NOT NULL,
			seen INTEGER NOT NULL DEFAULT 0, first_obs INTEGER NOT NULL, last_obs INTEGER NOT NULL,
			gone INTEGER NOT NULL DEFAULT 0, last_at TEXT NOT NULL DEFAULT '', PRIMARY KEY (key, path))`,
	} {
		if _, err := s.DB.Exec(q); err != nil {
			t.Fatalf("old table: %v", err)
		}
	}
	if _, err := s.DB.Exec(`INSERT INTO shape_fields (key, path, type, seen, first_obs, last_obs, gone, last_at)
		VALUES (?, '{*}', 'number', 100, 1, 100, 0, '')`, key); err != nil {
		t.Fatalf("old field: %v", err)
	}
	if _, err := s.DB.Exec(`INSERT INTO shape_keys (key, observations) VALUES (?, 100)`, key); err != nil {
		t.Fatalf("old key: %v", err)
	}
}

func TestLearnedGeneratedKeyMigratesToItsRealFieldName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	key := keyOf(Request, "anthropic", "v1/messages", "")
	oldShapeFields(t, s, key)
	s.Close()

	s, err = store.Open(path) // the migration runs here
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	o := newObserver(s, false)
	for i := 0; i < 25; i++ {
		o.Observe(Request, "anthropic", "v1/messages", []byte(`{"top_logprobs":2}`), false)
		o.Drain()
	}
	if c, _ := s.ListShapeChanges(store.ShapeChangeFilter{}); len(c) != 0 {
		t.Fatalf("the migration recorded changes: %+v", c)
	}
	o.Flush()

	o.Observe(Request, "anthropic", "v1/messages", []byte(`{"top_logprobs":2,"foo":1}`), false)
	o.Drain()
	c, _ := s.ListShapeChanges(store.ShapeChangeFilter{})
	if len(c) != 1 || c[0].Path != "foo" || c[0].Kind != "added" {
		t.Fatalf("a new field after the migration: %+v", c)
	}
}

func TestSecondOpenKeepsTheMigratedMark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, _ := store.Open(path)
	key := keyOf(Request, "anthropic", "v1/messages", "")
	oldShapeFields(t, s, key)
	s.Close()

	for i := 0; i < 2; i++ {
		s, err := store.Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		_, fields, err := s.LoadShapes()
		if err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
		if len(fields) != 1 || !fields[0].Legacy {
			t.Fatalf("open %d: fields = %+v", i, fields)
		}
		s.Close()
	}
}
