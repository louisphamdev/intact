package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// TypeSafe's System One models answer typed questions, not chat, so their
// test is one /v1/systemone call that asks one question of each type about a
// state whose answers are not in doubt, and checks both the shape of every
// answer and that it is right.
const typeSafeTestState = "I was charged twice for my subscription this month. Refund the extra charge today, I need that money back urgently."

var typeSafeTestQuestions = map[string]any{
	"urgent":  map[string]any{"type": "noul", "instructions": "Does this message express urgency?"},
	"bug":     map[string]any{"type": "noul", "instructions": "Does this message report a software bug or crash?"},
	"team":    map[string]any{"type": "choice", "instructions": "Which team should handle this", "criteria": map[string]string{"billing": "Payment, charge or refund issues", "technical": "Bugs or integration problems", "sales": "Pricing questions from new customers"}},
	"feeling": map[string]any{"type": "score", "instructions": "How upset is the customer?", "criteria": []string{"Calm", "Frustrated", "Very angry"}},
}

type typeSafeAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// checkTypeSafe reads a System One answer to the test questions and returns
// a short summary, or what is wrong with it.
func checkTypeSafe(raw []byte) (string, error) {
	var d struct {
		Model   string                    `json:"model"`
		Answers map[string]typeSafeAnswer `json:"answers"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", fmt.Errorf("answer is not JSON")
	}
	get := func(k, typ string) (typeSafeAnswer, error) {
		a, ok := d.Answers[k]
		if !ok {
			return a, fmt.Errorf("no answer to %q", k)
		}
		if a.Type != "" && a.Type != typ {
			return a, fmt.Errorf("%q answered as %s, asked as %s", k, a.Type, typ)
		}
		return a, nil
	}
	var problems []string
	urgent, err := get("urgent", "noul")
	switch {
	case err != nil:
		problems = append(problems, err.Error())
	case urgent.Noul == nil || *urgent.Noul < 0 || *urgent.Noul > 1:
		problems = append(problems, "urgent: noul missing or out of 0..1")
	case *urgent.Noul < 0.5:
		problems = append(problems, fmt.Sprintf("urgent: %.2f, expected yes", *urgent.Noul))
	}
	bug, err := get("bug", "noul")
	switch {
	case err != nil:
		problems = append(problems, err.Error())
	case bug.Noul == nil || *bug.Noul < 0 || *bug.Noul > 1:
		problems = append(problems, "bug: noul missing or out of 0..1")
	case *bug.Noul >= 0.5:
		problems = append(problems, fmt.Sprintf("bug: %.2f, expected no", *bug.Noul))
	}
	team, err := get("team", "choice")
	switch {
	case err != nil:
		problems = append(problems, err.Error())
	case team.Confidence == nil || len(team.Probabilities) != 3:
		problems = append(problems, "team: confidence or probabilities missing")
	case team.Choice != "billing":
		problems = append(problems, fmt.Sprintf("team: %q, expected billing", team.Choice))
	}
	feeling, err := get("feeling", "score")
	switch {
	case err != nil:
		problems = append(problems, err.Error())
	case feeling.Score == nil || *feeling.Score < 0 || *feeling.Score > 2 || feeling.Confidence == nil:
		problems = append(problems, "feeling: score out of 0..2 or confidence missing")
	case *feeling.Score < 0.5:
		problems = append(problems, fmt.Sprintf("feeling: %.2f, expected upset", *feeling.Score))
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return fmt.Sprintf("%s: urgent %.2f · bug %.2f · %s · upset %.2f", d.Model, *urgent.Noul, *bug.Noul, team.Choice, *feeling.Score), nil
}

// runTypeSafeTest sends the System One test through /v1, as a client would.
func (a *api) runTypeSafeTest(ctx context.Context, prov, model string) modelTest {
	body, _ := json.Marshal(map[string]any{"model": prov + "/" + model, "state": typeSafeTestState, "questions": typeSafeTestQuestions})
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/systemone", strings.NewReader(string(body)))
	req.SetPathValue("path", "systemone")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	start := time.Now()
	a.v1(rec, req)
	res := modelTest{Status: rec.Code, Ms: time.Since(start).Milliseconds()}
	raw, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK {
		res.Message = strings.TrimSpace(string(raw))
	} else if sum, err := checkTypeSafe(raw); err != nil {
		res.Message = err.Error()
	} else {
		res.OK, res.Message = true, sum
	}
	if len(res.Message) > 300 {
		res.Message = res.Message[:300]
	}
	return res
}
