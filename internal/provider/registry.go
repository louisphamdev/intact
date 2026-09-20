// Package provider holds the class B table: providers with a documented API
// reached with a plain credential. Class A providers, which impersonate a real
// tool, are deliberately absent until their traffic has been captured.
package provider

// Provider describes how to reach one upstream.
type Provider struct {
	ID         string
	BaseURL    string
	AuthHeader string
	AuthPrefix string
}

var registry = map[string]Provider{
	"groq": {
		ID:         "groq",
		BaseURL:    "https://api.groq.com/openai/v1",
		AuthHeader: "Authorization",
		AuthPrefix: "Bearer ",
	},
}

// Lookup returns the provider with this id.
func Lookup(id string) (Provider, bool) {
	p, ok := registry[id]
	return p, ok
}
