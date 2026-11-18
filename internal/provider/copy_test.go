package provider

import "testing"

// A copy calls the same upstream as the built-in provider it copies, under its own id, so
// its accounts, rotation and models stay apart from the original's.
func TestCopyProviderIsTheBuiltinUnderItsOwnID(t *testing.T) {
	d := Def{ID: "antigravity-lite", Name: "Antigravity Lite", Of: "antigravity",
		BaseURL: "https://ignored.example", Kind: KindOAuthCode}
	if err := d.Normalize(); err != nil {
		t.Fatal(err)
	}
	if d.BaseURL != "" || d.Kind != "" || d.OAuth != nil {
		t.Errorf("a copy keeps no fields of its own: %+v", d)
	}
	SetDeclared([]Def{d})
	defer SetDeclared(nil)

	got, ok := Lookup("antigravity-lite")
	want := registry["antigravity"]
	if !ok || got.ID != "antigravity-lite" || got.BaseURL != want.BaseURL || got.API != want.API ||
		got.Setup != want.Setup || got.Identity["User-Agent"] != want.Identity["User-Agent"] {
		t.Errorf("Lookup(copy) = %+v, want antigravity under id antigravity-lite", got)
	}
	if Base("antigravity-lite") != "antigravity" || Base("antigravity") != "antigravity" || Base("groq") != "groq" {
		t.Error("Base names the built-in provider of a copy, and a provider's own id otherwise")
	}
}

func TestCopyProviderNeedsABuiltin(t *testing.T) {
	for _, of := range []string{"nope", "antigravity-lite"} {
		d := Def{ID: "x-copy", Of: of}
		if err := d.Normalize(); err == nil {
			t.Errorf("of %q was accepted", of)
		}
	}
}
