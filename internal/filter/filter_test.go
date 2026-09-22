package filter

import (
	"strings"
	"testing"
)

func rules(t *testing.T, kv ...string) []Rule {
	var out []Rule
	for i := 0; i < len(kv); i += 2 {
		r, err := Compile(kv[i], kv[i+1])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestApplyLeavesAnUntouchedBodyByteForByte(t *testing.T) {
	in := `{"model":"m",  "n":1.50}`
	out, changed := Apply([]byte(in), rules(t, Field, "thinking", Schema, "$id"))
	if changed || string(out) != in {
		t.Errorf("out=%s changed=%v", out, changed)
	}
}

func TestFieldPaths(t *testing.T) {
	in := `{"thinking":{"a":1},"messages":[{"role":"user","content":"x","cache_control":{"t":1}},{"role":"user"}],"tools":[{"function":{"name":"f","strict":true}}]}`
	out, changed := Apply([]byte(in), rules(t, Field, "thinking", Field, "messages.*.cache_control", Field, "tools.*.function.strict"))
	s := string(out)
	if !changed || strings.Contains(s, "thinking") || strings.Contains(s, "cache_control") || strings.Contains(s, "strict") {
		t.Errorf("out=%s", s)
	}
	if !strings.Contains(s, `"name":"f"`) {
		t.Errorf("removed too much: %s", s)
	}
}

func TestSchemaKeyAtEveryDepthButNotAParameterName(t *testing.T) {
	in := `{"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","$id":"x",
	 "properties":{"example":{"type":"string","example":"hi"},"deep":{"type":"object","properties":{"a":{"type":"string","example":1}}}}}}},
	 {"name":"g","input_schema":{"type":"object","example":{}}}]}`
	out, _ := Apply([]byte(in), rules(t, Schema, "example", Schema, "$id"))
	s := string(out)
	if strings.Contains(s, `"$id"`) || strings.Contains(s, `"example":"hi"`) || strings.Contains(s, `"example":1`) || strings.Contains(s, `"example":{}`) {
		t.Errorf("schema key left: %s", s)
	}
	if !strings.Contains(s, `"example":{"type":"string"}`) {
		t.Errorf("the parameter named example was removed: %s", s)
	}
}

func TestSystemLines(t *testing.T) {
	re := `^x-anthropic-billing-header:.*$`
	in := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=1\nYou are Claude."}],"messages":[{"role":"system","content":"x-anthropic-billing-header: a\n\nBe kind"},{"role":"user","content":"x-anthropic-billing-header: keep"}]}`
	out, changed := Apply([]byte(in), rules(t, System, re))
	s := string(out)
	if !changed || strings.Count(s, "billing-header") != 1 || !strings.Contains(s, `"You are Claude."`) || !strings.Contains(s, `"content":"Be kind"`) {
		t.Errorf("out=%s", s)
	}
}

func TestCompileRejectsBadRules(t *testing.T) {
	for _, c := range [][2]string{{Field, ""}, {Field, "a..b"}, {System, "("}, {"nope", "x"}} {
		if _, err := Compile(c[0], c[1]); err == nil {
			t.Errorf("Compile(%q, %q) accepted", c[0], c[1])
		}
	}
	r, _ := Compile(Header, "anthropic-beta")
	if Headers([]Rule{r})[0] != "Anthropic-Beta" {
		t.Errorf("header not canonical: %v", Headers([]Rule{r}))
	}
}

// Every shape a provider receives: the Antigravity envelope (Gemini inside
// "request"), Gemini, and Responses.
func TestSystemLinesAndSchemasInEveryShape(t *testing.T) {
	re := `^x-anthropic-billing-header:.*$`
	for name, in := range map[string]string{
		"antigravity": `{"model":"m","project":"p","request":{"systemInstruction":{"role":"user","parts":[{"text":"x-anthropic-billing-header: cc=1\nBe kind"}]},"contents":[{"role":"user","parts":[{"text":"x-anthropic-billing-header: keep"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"object","$id":"x","properties":{"a":{"type":"string"}}}}]}]}}`,
		"gemini":      `{"systemInstruction":{"parts":[{"text":"x-anthropic-billing-header: cc=1\nBe kind"}]},"contents":[{"role":"user","parts":[{"text":"x-anthropic-billing-header: keep"}]}],"tools":[{"functionDeclarations":[{"name":"f","parametersJsonSchema":{"$id":"x"}}]}]}`,
		"responses":   `{"instructions":"x-anthropic-billing-header: cc=1\nBe kind","input":[{"role":"developer","content":[{"type":"input_text","text":"x-anthropic-billing-header: z"}]},{"role":"user","content":"x-anthropic-billing-header: keep"}],"tools":[{"type":"function","name":"f","parameters":{"$id":"x"}}]}`,
	} {
		out, changed := Apply([]byte(in), rules(t, System, re, Schema, "$id"))
		s := string(out)
		if !changed || strings.Count(s, "billing-header") != 1 || !strings.Contains(s, "keep") || !strings.Contains(s, "Be kind") || strings.Contains(s, "$id") {
			t.Errorf("%s: %s", name, s)
		}
	}
}
