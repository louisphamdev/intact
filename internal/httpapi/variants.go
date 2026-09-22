package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/translate"
)

// Antigravity lists one model several times, once per thinking level:
// gemini-3.8-flash-high, -medium, -low, and -tiered (the one the IDE uses
// without a level). intact lists them as one model, gemini-3.8-flash, and
// picks the variant from the effort the client asks for.

// variantSuffixes are the levels a model id can end with, longest first so
// "-extra-low" is not read as "-low".
var variantSuffixes = []string{"extra-low", "tiered", "medium", "high", "low"}

// variantRank orders the thinking levels, for the nearest match.
var variantRank = map[string]int{"extra-low": 0, "low": 1, "medium": 2, "high": 3}

// variantSet is the variants of one base model: level → upstream id. The
// level "" is the id without a level, when the provider lists one.
type variantSet struct {
	ids map[string]string
	def string
}

// Variants returns the levels in a stable order, the default first.
func (v variantSet) Variants() []string {
	out := make([]string, 0, len(v.ids))
	for l := range v.ids {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i] == v.def) != (out[j] == v.def) {
			return out[i] == v.def
		}
		ri, iok := variantRank[out[i]]
		rj, jok := variantRank[out[j]]
		if iok != jok {
			return iok
		}
		if ri != rj {
			return ri > rj
		}
		return out[i] < out[j]
	})
	return out
}

func splitVariant(id string) (base, level string) {
	for _, s := range variantSuffixes {
		if b, ok := strings.CutSuffix(id, "-"+s); ok && b != "" {
			return b, s
		}
	}
	return id, ""
}

// groupVariants folds the ids that differ only by a level into their base.
// The default variant is the id without a level, else -tiered, else the
// highest level.
func groupVariants(ids []string) ([]string, map[string]variantSet) {
	groups := map[string]variantSet{}
	for _, id := range ids {
		base, level := splitVariant(id)
		g, ok := groups[base]
		if !ok {
			g = variantSet{ids: map[string]string{}}
		}
		g.ids[level] = id
		groups[base] = g
	}
	bases := make([]string, 0, len(groups))
	for base, g := range groups {
		bases = append(bases, base)
		switch {
		case g.ids[""] != "":
			g.def = ""
		case g.ids["tiered"] != "":
			g.def = "tiered"
		default:
			best := -1
			for l := range g.ids {
				if r, ok := variantRank[l]; ok && r > best {
					best, g.def = r, l
				}
			}
		}
		groups[base] = g
		if len(g.ids) == 1 && g.def == "" {
			delete(groups, base) // a plain model, nothing folded
		}
	}
	sort.Strings(bases)
	return bases, groups
}

// pick returns the upstream id for an effort: a level the model has, the
// nearest one it has, or the default for none.
func (v variantSet) pick(effort string) string {
	switch effort {
	case "", "none", "auto", "default":
		return v.ids[v.def]
	case "minimal":
		effort = "extra-low"
	case "xhigh", "max":
		effort = "high"
	}
	if id, ok := v.ids[effort]; ok {
		return id
	}
	want, ok := variantRank[effort]
	if !ok {
		return v.ids[v.def]
	}
	best, bestD := "", 99
	for l := range v.ids {
		if r, ok := variantRank[l]; ok {
			d := r - want
			if d < 0 {
				d = -d
			}
			// On a tie, prefer the higher level.
			if d < bestD || (d == bestD && r > variantRank[best]) {
				best, bestD = l, d
			}
		}
	}
	if best == "" {
		return v.ids[v.def]
	}
	return v.ids[best]
}

// effortOf reads the thinking effort a request asks for, in any of the
// shapes intact accepts: OpenAI reasoning_effort, Responses reasoning.effort,
// Anthropic output_config.effort or thinking.budget_tokens, Gemini
// thinkingConfig. "" means the client did not say.
func effortOf(body []byte) string {
	var b struct {
		ReasoningEffort string `json:"reasoning_effort"`
		Reasoning       struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
		Thinking struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
		GenerationConfig struct {
			ThinkingConfig struct {
				ThinkingLevel  string `json:"thinkingLevel"`
				ThinkingBudget *int   `json:"thinkingBudget"`
			} `json:"thinkingConfig"`
		} `json:"generationConfig"`
	}
	if json.NewDecoder(bytes.NewReader(body)).Decode(&b) != nil {
		return ""
	}
	budget := func(n int) string {
		switch {
		case n == 0:
			return "none"
		case n < 0:
			return "high"
		case n <= 1500:
			return "low"
		case n <= 6000:
			return "medium"
		}
		return "high"
	}
	tc := b.GenerationConfig.ThinkingConfig
	for _, e := range []string{b.ReasoningEffort, b.Reasoning.Effort, b.OutputConfig.Effort, tc.ThinkingLevel} {
		if e != "" {
			return strings.ToLower(e)
		}
	}
	switch b.Thinking.Type {
	case "disabled":
		return "none"
	case "enabled":
		return budget(b.Thinking.BudgetTokens)
	}
	if tc.ThinkingBudget != nil {
		return budget(*tc.ThinkingBudget)
	}
	return ""
}

// variants returns a provider's folded models (nil for a provider without).
func (a *api) variants(prov string) map[string]variantSet {
	a.cat.mu.Lock()
	defer a.cat.mu.Unlock()
	return a.cat.m[prov].groups
}

// variantBase returns the listed model an upstream id belongs to: its base
// when it is a folded variant, else the id itself.
func (a *api) variantBase(prov, model string) string {
	if base, level := splitVariant(model); level != "" {
		if g, ok := a.variants(prov)[base]; ok && g.ids[level] == model {
			return base
		}
	}
	return model
}

// resolveVariant turns a listed base model into the upstream id for the
// effort the request asks; any other model is left as it is.
func (a *api) resolveVariant(prov, model string, body []byte) string {
	if g, ok := a.variants(prov)[model]; ok {
		if id := g.pick(effortOf(body)); id != "" {
			return id
		}
	}
	return model
}

// attempt is one try of a request: an account, and the upstream model when
// intact picks it ("" keeps the request's own).
type attempt struct {
	conn  store.Connection
	model string
}

// attemptsFor orders the tries of a request: every account in rotation
// order; then, for a level variant that every account refused as busy, every
// account again on the model's default variant (a -high is refused for
// capacity far more often than the -tiered one).
func (a *api) attemptsFor(ctx context.Context, targets []store.Connection, start int, body []byte) []attempt {
	model, _ := bodyModel(body)
	out := make([]attempt, 0, len(targets))
	var fallback []attempt
	for i := range targets {
		c := targets[(start+i)%len(targets)]
		p, ok := a.providerFor(c)
		if !ok || p.API != translate.Antigravity {
			out = append(out, attempt{conn: c})
			continue
		}
		a.catalogIDs(ctx, c.Provider) // the variants come with the list
		picked := a.resolveVariant(c.Provider, model, body)
		out = append(out, attempt{conn: c, model: picked})
		if g, ok := a.variants(c.Provider)[a.variantBase(c.Provider, picked)]; ok {
			if def := g.ids[g.def]; def != "" && def != picked {
				fallback = append(fallback, attempt{conn: c, model: def})
			}
		}
	}
	return append(out, fallback...)
}
