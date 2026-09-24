package httpapi

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/louisphamdev/intact/internal/filter"
	"github.com/louisphamdev/intact/internal/store"
)

// A false 429 is the provider refusing a client fingerprint in the system
// prompt. A model cannot see which sentence it is; replays can. The search
// halves the system sentences, then the words of the one it finds.
const (
	bisectMaxReplays = 16
	bisectMinWords   = 3
)

var (
	sentenceRe = regexp.MustCompile(`[^\n.!?]+[.!?]*`)
	wordRe     = regexp.MustCompile(`\w`)
)

var errBisectBudget = errors.New("the search needs more replays than it may spend")

// systemSentences splits the system prompt of a request into its sentences.
func systemSentences(body []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, text := range filter.SystemTexts(body) {
		for _, s := range sentenceRe.FindAllString(text, -1) {
			s = strings.TrimSpace(s)
			if len(s) < 4 || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// textPattern matches any of the texts, with any run of spaces between words.
func textPattern(texts []string) string {
	alts := make([]string, len(texts))
	for i, t := range texts {
		words := strings.Fields(t)
		for j, w := range words {
			words[j] = regexp.QuoteMeta(w)
		}
		alts[i] = strings.Join(words, `\s+`)
		// A text found by halving words can end inside a word: "Codex, a" must not eat the "a" of "an".
		if wordRe.MatchString(t[:1]) {
			alts[i] = `\b` + alts[i]
		}
		if wordRe.MatchString(t[len(t)-1:]) {
			alts[i] += `\b`
		}
	}
	if len(alts) == 1 {
		return alts[0]
	}
	return "(?:" + strings.Join(alts, "|") + ")"
}

// bisectSystem returns the smallest system text whose removal turns the
// failing replay into an accepted one, as a system rule pattern. It returns ""
// when removing the whole system prompt does not help.
func (a *api) bisectSystem(ctx context.Context, e store.UpstreamError, req []byte, status int) (string, int, error) {
	replays := 0
	accepted := func(texts []string) (bool, error) {
		rule, err := filter.Compile(filter.System, textPattern(texts))
		if err != nil {
			return false, err
		}
		out, changed := filter.Apply(req, []filter.Rule{rule})
		if !changed {
			return false, nil
		}
		if replays == bisectMaxReplays {
			return false, errBisectBudget
		}
		replays++
		st, msg, err := a.replay(ctx, e, out)
		switch {
		case err != nil:
			return false, err
		case st < 400:
			return true, nil
		case st != status:
			return false, fmt.Errorf("a replay failed differently: %d %s", st, truncate(msg, 120))
		}
		return false, nil
	}
	cand := systemSentences(req)
	if ok, err := accepted(cand); !ok || err != nil {
		return "", replays, err
	}
	for len(cand) > 1 {
		h := len(cand) / 2
		if ok, err := accepted(cand[:h]); err != nil {
			return "", replays, err
		} else if ok {
			cand = cand[:h]
			continue
		}
		if ok, err := accepted(cand[h:]); err != nil {
			return "", replays, err
		} else if ok {
			cand = cand[h:]
			continue
		}
		break
	}
	if len(cand) > 1 {
		// Two sentences refused only together: the rule removes both.
		return textPattern(cand), replays, nil
	}
	words := strings.Fields(cand[0])
	for len(words) > bisectMinWords {
		h := len(words) / 2
		narrowed := false
		for _, part := range [][]string{words[:h], words[h:]} {
			if len(part) < bisectMinWords {
				continue
			}
			ok, err := accepted([]string{strings.Join(part, " ")})
			if err != nil {
				return "", replays, err
			}
			if ok {
				words, narrowed = part, true
				break
			}
		}
		if !narrowed {
			break
		}
	}
	return textPattern([]string{strings.Join(words, " ")}), replays, nil
}

// settleFake429 installs the rule a bisect found. No model reads the request,
// so key traffic needs no approval: the rule removes only text the provider
// was seen to refuse.
func (a *api) settleFake429(ctx context.Context, v *store.ErrorVerdict, g ErrorGroup, e store.UpstreamError, req []byte) bool {
	pattern, n, err := a.bisectSystem(ctx, e, req, g.Status)
	if err != nil || pattern == "" || catchAllSystem(pattern) {
		return false
	}
	v.Action, v.By, v.Verified, v.Cause = "blacklist", "intact", true, "system prompt text the provider refuses"
	v.Detail = filter.System + ": " + pattern
	v.Reason = fmt.Sprintf("the replay is refused with this text and accepted without it (%d replays)", n)
	if a.filterInPlace(e.Provider, filter.System, pattern) {
		v.Applied, v.Note = true, "the rule is already in place"
		return true
	}
	if _, err := a.putFilter(store.Filter{Provider: e.Provider, Kind: filter.System, Pattern: pattern,
		Note: "error review, found by replay: " + truncate(e.Signature, 80), Enabled: true}); err != nil {
		v.Note = "installing the rule failed: " + err.Error()
		return true
	}
	v.Applied = true
	return true
}
