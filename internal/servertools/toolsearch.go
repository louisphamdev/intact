package servertools

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// searchable is one deferred tool as the search sees it: the text of its name,
// description, argument names and argument descriptions.
type searchable struct{ name, text string }

func searchableOf(def obj) searchable {
	var b strings.Builder
	b.WriteString(str(def["name"]))
	b.WriteString(" ")
	b.WriteString(str(def["description"]))
	if props := asObj(asObj(def["input_schema"])["properties"]); props != nil {
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(" ")
			b.WriteString(k)
			b.WriteString(" ")
			b.WriteString(str(asObj(props[k])["description"]))
		}
	}
	return searchable{name: str(def["name"]), text: b.String()}
}

// A toolSearchError is an error Anthropic's tool search reports in the result.
type toolSearchError struct{ code, message string }

func (e *toolSearchError) Error() string { return e.message }

const (
	maxRegexLen = 200
	maxQueryLen = 500
	defaultHits = 5
)

// regexSearch finds the tools whose text matches a pattern, in the order given.
// Anthropic runs Python's re.search, case-insensitive; RE2 takes the same
// patterns but for look-around and back-references, which it reports.
func regexSearch(items []searchable, pattern string, limit int) ([]string, error) {
	if len(pattern) > maxRegexLen {
		return nil, &toolSearchError{"invalid_tool_input", fmt.Sprintf("pattern is longer than %d characters", maxRegexLen)}
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, &toolSearchError{"invalid_tool_input", "Invalid regular expression pattern: " + err.Error()}
	}
	var hits []string
	for _, it := range items {
		if re.MatchString(it.text) {
			hits = append(hits, it.name)
			if len(hits) >= limit {
				break
			}
		}
	}
	return hits, nil
}

// bm25Search ranks the tools against a natural language query.
func bm25Search(items []searchable, query string, limit int) ([]string, error) {
	if len(query) > maxQueryLen {
		return nil, &toolSearchError{"invalid_tool_input", fmt.Sprintf("query is longer than %d characters", maxQueryLen)}
	}
	terms := tokens(query)
	if len(terms) == 0 || len(items) == 0 {
		return nil, nil
	}
	docs := make([][]string, len(items))
	total := 0
	df := map[string]int{}
	for i, it := range items {
		docs[i] = append(tokens(it.name), tokens(it.text)...) // the name counts twice
		total += len(docs[i])
		seen := map[string]bool{}
		for _, t := range docs[i] {
			if !seen[t] {
				seen[t] = true
				df[t]++
			}
		}
	}
	avg := float64(total) / float64(len(docs))
	const k1, b = 1.2, 0.75
	type scored struct {
		name  string
		score float64
		at    int
	}
	var ranked []scored
	for i, d := range docs {
		tf := map[string]int{}
		for _, t := range d {
			tf[t]++
		}
		score := 0.0
		for _, q := range terms {
			n := float64(tf[q])
			if n == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(docs))-float64(df[q])+0.5)/(float64(df[q])+0.5))
			score += idf * n * (k1 + 1) / (n + k1*(1-b+b*float64(len(d))/avg))
		}
		if score > 0 {
			ranked = append(ranked, scored{items[i].name, score, i})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	var hits []string
	for _, r := range ranked {
		hits = append(hits, r.name)
		if len(hits) >= limit {
			break
		}
	}
	return hits, nil
}

func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
