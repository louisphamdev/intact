package contract

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// Judge provides contract judgement for candidate drift signatures.
type Judge interface {
	JudgeCandidates(ctx context.Context, keyID string, candidates []Candidate, unmatchedOutputPaths []string, approvedMappings []Mapping, goldenPaths []string) (map[string]string, error)
}

// JudgeManager coordinates leasing, rate limits, and verification of judge verdicts.
type JudgeManager struct {
	store *store.Store
	judge Judge

	mu       sync.Mutex
	dayCount map[string]int // keyID:YYYY-MM-DD -> count
}

// NewJudgeManager initializes a JudgeManager.
func NewJudgeManager(s *store.Store, j Judge) *JudgeManager {
	return &JudgeManager{
		store:    s,
		judge:    j,
		dayCount: make(map[string]int),
	}
}

// ResetLeasesOnStartup resets all leased signatures back to 'pending'.
func (jm *JudgeManager) ResetLeasesOnStartup() error {
	_, err := jm.store.DB.Exec(`UPDATE contract_signatures SET lease_until = 0 WHERE verdict_state = 'pending' AND lease_until > 0`)
	return err
}

// GetDayCount returns the judge attempts for the given key and day.
func (jm *JudgeManager) GetDayCount(limitKey string) int {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	return jm.dayCount[limitKey]
}

func (jm *JudgeManager) hasDecisionModel() bool {
	if jm.judge == nil {
		return false
	}
	if checker, ok := jm.judge.(interface{ HasDecisionModel() bool }); ok {
		return checker.HasDecisionModel()
	}
	return true
}

// ProcessCandidates processes candidates from a trace, acquiring leases and calling Judge.
func (jm *JudgeManager) ProcessCandidates(ctx context.Context, keyID, traceID string, candidates []Candidate, unmatchedOutput []string, approvedMappings []Mapping, goldenPaths []string) error {
	if len(candidates) == 0 {
		return nil
	}

	today := time.Now().UTC().Format("2006-01-02")
	limitKey := fmt.Sprintf("%s:%s", keyID, today)

	var toJudge []Candidate
	nowMs := time.Now().UnixMilli()
	lease5Min := nowMs + 5*60*1000

	hasModel := jm.hasDecisionModel()

	for _, cand := range candidates {
		sig, err := jm.store.GetContractSignature(cand.Model, cand.ClientFormat, cand.Direction, cand.Path, cand.Type)
		if err != nil || sig.Model == "" {
			leaseTime := int64(0)
			if hasModel {
				leaseTime = lease5Min
			}
			// New signature: insert with pending
			err = jm.store.InsertContractSignature(store.ContractSignature{
				Model:          cand.Model,
				ClientFormat:   cand.ClientFormat,
				Direction:      cand.Direction,
				Path:           cand.Path,
				Type:           cand.Type,
				VerdictState:   "pending",
				LeaseUntil:     leaseTime,
				KeyID:          keyID,
				FirstTraceID:   traceID,
				ReducerVersion: ReducerVersion,
			})
			if err == nil && hasModel {
				toJudge = append(toJudge, cand)
			}
			continue
		}

		// If verdict is already resolved or error, do not judge again
		if sig.VerdictState == "approved" || sig.VerdictState == "proposed" || sig.VerdictState == "rejected" || sig.VerdictState == "error" {
			if sig.VerdictState == "approved" && sig.Verdict == "lost" {
				trace, err := jm.store.GetContractTrace(traceID)
				if err == nil {
					_ = HandleLostCandidate(jm.store, trace, cand, "")
				}
			}
			continue
		}

		if !hasModel {
			continue
		}

		// If pending with no live lease
		if sig.VerdictState == "pending" && sig.LeaseUntil < nowMs {
			ok, _ := jm.store.LeaseContractSignature(cand.Model, cand.ClientFormat, cand.Direction, cand.Path, cand.Type, lease5Min)
			if ok {
				toJudge = append(toJudge, cand)
			}
		}
	}

	if !hasModel || len(toJudge) == 0 {
		return nil
	}

	// Check 50 attempts per key per day
	jm.mu.Lock()
	if jm.dayCount[limitKey] >= 50 {
		jm.mu.Unlock()
		return nil
	}
	jm.dayCount[limitKey]++
	jm.mu.Unlock()

	if jm.judge == nil {
		return nil
	}

	// Prepare offered options
	validOptions := map[string]bool{
		"lost":       true,
		"normalized": true,
		"noise":      true,
	}
	for i := range unmatchedOutput {
		validOptions["renamed:"+strconv.Itoa(i)] = true
	}

	answers, err := jm.judge.JudgeCandidates(ctx, keyID, toJudge, unmatchedOutput, approvedMappings, goldenPaths)
	if err != nil {
		// Judge call failed: increment error count on each candidate
		for _, cand := range toJudge {
			_ = jm.store.IncrementSignatureError(cand.Model, cand.ClientFormat, cand.Direction, cand.Path, cand.Type)
		}
		return err
	}

	// Validate answers
	for _, cand := range toJudge {
		candKey := cand.Path
		ans, ok := answers[candKey]
		if !ok || !validOptions[ans] {
			// Invalid answer counts toward error limit
			_ = jm.store.IncrementSignatureError(cand.Model, cand.ClientFormat, cand.Direction, cand.Path, cand.Type)
			continue
		}

		verdict := ans
		verdictState := "proposed"
		var mappedPath string

		if strings.HasPrefix(ans, "renamed:") {
			idxStr := strings.TrimPrefix(ans, "renamed:")
			idx, err := strconv.Atoi(idxStr)
			if err != nil || idx < 0 || idx >= len(unmatchedOutput) {
				_ = jm.store.IncrementSignatureError(cand.Model, cand.ClientFormat, cand.Direction, cand.Path, cand.Type)
				continue
			}
			mappedPath = unmatchedOutput[idx]
			verdict = "renamed:" + mappedPath
			verdictState = "proposed"
		} else if ans == "lost" {
			verdict = "lost"
			verdictState = "approved"
		} else {
			// noise or normalized
			verdict = ans
			verdictState = "proposed"
		}

		_ = jm.store.UpdateSignatureVerdict(cand.Model, cand.ClientFormat, cand.Direction, cand.Path, cand.Type, verdict, verdictState, time.Now().UnixMilli())

		// If lost, open or update finding
		if verdict == "lost" {
			trace, err := jm.store.GetContractTrace(traceID)
			if err == nil {
				_ = HandleLostCandidate(jm.store, trace, cand, mappedPath)
			}
		}
	}

	return nil
}

// StubJudge is a test double that returns pre-configured answers.
type StubJudge struct {
	mu      sync.Mutex
	Answers map[string]string
	Err     error
	Calls   int
}

// NewStubJudge creates a StubJudge.
func NewStubJudge(answers map[string]string) *StubJudge {
	return &StubJudge{
		Answers: answers,
	}
}

// JudgeCandidates returns configured answers or error.
func (s *StubJudge) JudgeCandidates(ctx context.Context, keyID string, candidates []Candidate, unmatchedOutputPaths []string, approvedMappings []Mapping, goldenPaths []string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls++
	if s.Err != nil {
		return nil, s.Err
	}
	res := make(map[string]string)
	for _, c := range candidates {
		if ans, ok := s.Answers[c.Path]; ok {
			res[c.Path] = ans
		} else if ans, ok := s.Answers["*"]; ok {
			res[c.Path] = ans
		} else {
			res[c.Path] = "lost"
		}
	}
	return res, nil
}
