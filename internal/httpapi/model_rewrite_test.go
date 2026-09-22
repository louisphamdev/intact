package httpapi

import "testing"

func TestSetModelSplicesOnlyTheTopLevelValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"model":"a","x":1}`, `{"model":"b","x":1}`},
		{`{ "stream" : true , "model" : "a" }`, `{ "stream" : true , "model" : "b" }`},
		{`{"messages":[{"model":"keep"}],"model":"a","n":1.50}`, `{"messages":[{"model":"keep"}],"model":"b","n":1.50}`},
		{`{"model":null}`, `{"model":"b"}`},
	}
	for _, c := range cases {
		got, ok := setModel([]byte(c.in), "b")
		if !ok || string(got) != c.want {
			t.Errorf("setModel(%s) = %s, %v; want %s", c.in, got, ok, c.want)
		}
	}
}

func TestSetModelLeavesOtherBodiesAlone(t *testing.T) {
	for _, in := range []string{``, `[1,2]`, `{"x":{"model":"a"}}`, `not json`} {
		got, ok := setModel([]byte(in), "b")
		if ok || string(got) != in {
			t.Errorf("setModel(%q) = %q, %v; want unchanged", in, got, ok)
		}
	}
}
