package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/louisphamdev/intact/internal/contract"
	"github.com/louisphamdev/intact/internal/store"
)

func (a *api) contractTraceHalf(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := principalOf(r)
	isTrusted := a.isCallerTrusted(p)

	// Decompress and read body (up to 4 MiB)
	b, err := io.ReadAll(io.LimitReader(r.Body, contract.MaxHalfBytes+1))
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	isGzip := r.Header.Get("Content-Encoding") == "gzip"
	decompressed, err := contract.DecompressHalf(b, isGzip)
	if err != nil {
		if errors.Is(err, contract.ErrPayloadTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeError(w, http.StatusBadRequest, "cannot decompress body: "+err.Error())
		return
	}

	var half contract.HalfPayload
	if err := json.Unmarshal(decompressed, &half); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	// Input bounds validation (Section 7)
	if err := contract.ValidateToolVersion(half.ToolVersion); err != nil {
		writeError(w, http.StatusBadRequest, "invalid toolVersion")
		return
	}
	if err := contract.ValidateSwitcherVersion(half.SwitcherVersion, time.Now().UTC()); err != nil {
		writeError(w, http.StatusBadRequest, "invalid switcherVersion: "+err.Error())
		return
	}
	if !contract.IsValidFormat(half.Converter.InFormat) || !contract.IsValidFormat(half.Converter.OutFormat) {
		writeError(w, http.StatusBadRequest, "invalid converter formats")
		return
	}

	statusCode, err := a.contractMgr.SubmitHalf(id, p.keyID, isTrusted, &half)
	if err != nil {
		if statusCode == http.StatusNotFound {
			writeError(w, http.StatusNotFound, "trace not found")
			return
		}
		if statusCode == http.StatusConflict {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if statusCode == http.StatusGone {
			writeError(w, http.StatusGone, err.Error())
			return
		}
		writeError(w, statusCode, err.Error())
		return
	}
	w.WriteHeader(statusCode)
}

func (a *api) contractTraceStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := principalOf(r)
	isTrusted := a.isCallerTrusted(p)

	trace, err := a.store.GetContractTrace(id)
	if err != nil || trace.ID == "" {
		writeError(w, http.StatusNotFound, "trace not found")
		return
	}

	// Opener or trusted caller only
	if !isTrusted && (trace.KeyID == "" || trace.KeyID != p.keyID) {
		writeError(w, http.StatusNotFound, "trace not found")
		return
	}

	writeJSON(w, map[string]string{
		"status": trace.Status,
	})
}

func (a *api) contractPolicy(w http.ResponseWriter, r *http.Request) {
	if a.consumer == nil {
		writeJSON(w, map[string]any{
			"models":        map[string]float64{},
			"default":       0.02,
			"changedModels": 0,
			"changedTools":  0,
		})
		return
	}
	pol := a.consumer.GetPolicy()
	writeJSON(w, pol)
}

func (a *api) contractIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.contractsIndexData(""))
}

func (a *api) contractsIndexData(modelFilter string) []map[string]any {
	rows, err := a.store.DB.Query(`SELECT DISTINCT subject FROM contract_learned WHERE kind = 'model'`)
	var models []string
	if err == nil {
		for rows.Next() {
			var m string
			if rows.Scan(&m) == nil && m != "" {
				if modelFilter == "" || modelFilter == m {
					models = append(models, m)
				}
			}
		}
		rows.Close()
	}

	// Also check distinct models in contract_findings and contract_traces
	findingModels, _ := a.store.DB.Query(`SELECT DISTINCT model FROM contract_findings`)
	if findingModels != nil {
		for findingModels.Next() {
			var m string
			if findingModels.Scan(&m) == nil && m != "" {
				if modelFilter == "" || modelFilter == m {
					found := false
					for _, existing := range models {
						if existing == m {
							found = true
							break
						}
					}
					if !found {
						models = append(models, m)
					}
				}
			}
		}
		findingModels.Close()
	}

	sort.Strings(models)
	var out []map[string]any
	for _, m := range models {
		// Calculate contract version hash from learned paths
		learned, _ := a.store.ListContractLearned("model", m)
		var paths []string
		for _, l := range learned {
			paths = append(paths, l.Path+":"+l.Type)
		}
		sort.Strings(paths)
		h := sha256.Sum256([]byte(sortStringsJoin(paths)))
		verHash := hex.EncodeToString(h[:12])

		var lastSample int64
		_ = a.store.DB.QueryRow(`SELECT created FROM contract_traces WHERE model = ? ORDER BY created DESC LIMIT 1`, m).Scan(&lastSample)

		var openFindings int
		_ = a.store.DB.QueryRow(`SELECT count(*) FROM contract_findings WHERE model = ? AND status = 'open'`, m).Scan(&openFindings)

		out = append(out, map[string]any{
			"kind":            "model",
			"subject":         m,
			"model":           m,
			"contractVersion": verHash,
			"lastSample":      lastSample,
			"openFindings":    openFindings,
		})
	}
	return out
}

func sortStringsJoin(ss []string) string {
	var b []byte
	for _, s := range ss {
		b = append(b, s...)
		b = append(b, ';')
	}
	return string(b)
}

func (a *api) contractModelDetails(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	data, err := a.contractModelDetailsData(model)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, data)
}

func (a *api) contractModelDetailsData(model string) (map[string]any, error) {
	learned, err := a.store.ListContractLearned("model", model)
	if err != nil {
		return nil, err
	}

	// No hash, and no len for strings (Spec Section 7 & 14)
	var learnedEntries []map[string]any
	for _, l := range learned {
		entry := map[string]any{
			"path":      l.Path,
			"type":      l.Type,
			"seen":      l.Seen,
			"firstSeen": l.FirstSeen,
			"lastSeen":  l.LastSeen,
			"format":    l.Format,
			"direction": l.Direction,
		}
		if l.Event != "" {
			entry["event"] = l.Event
		}
		learnedEntries = append(learnedEntries, entry)
	}

	// Golden contract for each tool
	var goldenEntries []map[string]any
	goldenTools := []string{"claude", "codex"}
	for _, tool := range goldenTools {
		toolLearned, _ := a.store.ListContractLearned("tool", tool)
		for _, tl := range toolLearned {
			entry := map[string]any{
				"tool":      tool,
				"path":      tl.Path,
				"type":      tl.Type,
				"seen":      tl.Seen,
				"direction": tl.Direction,
			}
			goldenEntries = append(goldenEntries, entry)
		}
	}

	return map[string]any{
		"model":   model,
		"learned": learnedEntries,
		"golden":  goldenEntries,
	}, nil
}

func (a *api) contractFindings(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if !a.isCallerTrusted(p) {
		http.NotFound(w, r)
		return
	}
	status := r.URL.Query().Get("status")
	var since int64
	if s := r.URL.Query().Get("since"); s != "" {
		since, _ = strconv.ParseInt(s, 10, 64)
	}
	findings, err := a.store.ListContractFindings(status, since, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, a.formatFindingsJSON(findings))
}

func (a *api) contractFixture(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if !a.isCallerTrusted(p) {
		http.NotFound(w, r)
		return
	}
	traceID := r.PathValue("traceId")
	shapes, err := a.store.ListContractShapesForTrace(traceID)
	if err != nil || len(shapes) == 0 {
		http.NotFound(w, r)
		return
	}
	// The reduced halves of one trace, with len and delta counts, without hash (Spec Section 7 & 14)
	var stripped []map[string]any
	for _, s := range shapes {
		var rec contract.ShapeRecord
		if err := json.Unmarshal([]byte(s.Record), &rec); err != nil {
			continue
		}
		var strippedRecords []map[string]any
		for _, r := range rec.Records {
			var strippedLeaves []map[string]any
			for _, l := range r.Leaves {
				leafMap := map[string]any{
					"path": l.Path,
					"type": l.Type,
				}
				if l.Len > 0 {
					leafMap["len"] = l.Len
				}
				if l.Enum != "" {
					leafMap["enum"] = l.Enum
				}
				if l.Deltas > 0 {
					leafMap["deltas"] = l.Deltas
				}
				// Explicitly NO hash!
				strippedLeaves = append(strippedLeaves, leafMap)
			}
			recMap := map[string]any{
				"leaves": strippedLeaves,
			}
			if r.Event != "" {
				recMap["event"] = r.Event
			}
			strippedRecords = append(strippedRecords, recMap)
		}
		stripped = append(stripped, map[string]any{
			"half":      s.Half,
			"direction": s.Direction,
			"records":   strippedRecords,
		})
	}
	writeJSON(w, map[string]any{
		"traceId": traceID,
		"shapes":  stripped,
	})
}

func (a *api) contractResolveFinding(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if !a.isCallerTrusted(p) {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	var body struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	who := p.keyID
	if p.admin {
		who = "session"
	}
	err := contract.ResolveFinding(a.store, id, body.Status, who, body.Note)
	if err != nil {
		if errors.Is(err, contract.ErrConflict) {
			writeError(w, http.StatusConflict, "finding status conflict")
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "finding not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": body.Status})
}

func (a *api) formatFindingsJSON(findings []store.ContractFinding) []map[string]any {
	out := make([]map[string]any, 0, len(findings))
	for _, f := range findings {
		out = append(out, map[string]any{
			"id":                  f.ID,
			"model":               f.Model,
			"client_format":       f.ClientFormat,
			"clientFormat":        f.ClientFormat,
			"direction":           f.Direction,
			"path":                f.Path,
			"reducer_version":     f.ReducerVersion,
			"reducerVersion":      f.ReducerVersion,
			"tool":                f.Tool,
			"class":               f.Class,
			"mapping":             f.Mapping,
			"confidence":          f.Confidence,
			"first_trace":         f.FirstTrace,
			"firstTrace":          f.FirstTrace,
			"first_trace_trusted": f.FirstTraceTrusted,
			"firstTraceTrusted":   f.FirstTraceTrusted,
			"last_trace":          f.LastTrace,
			"lastTrace":           f.LastTrace,
			"exempt_trace":        f.ExemptTrace,
			"exemptTrace":         f.ExemptTrace,
			"count":               f.Count,
			"status":              f.Status,
			"fixed_in":            f.FixedIn,
			"fixedIn":             f.FixedIn,
			"clean_traces":        f.CleanTraces,
			"cleanTraces":         f.CleanTraces,
			"created_at":          f.CreatedAt,
			"updated_at":          f.UpdatedAt,
		})
	}
	return out
}

func (a *api) contractTracesList(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if !a.isCallerTrusted(p) {
		http.NotFound(w, r)
		return
	}
	traces, err := a.store.ListRecentTraces(100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dropped, _ := a.store.GetContractCounter("dropped_traces")
	if a.consumer != nil {
		if d := a.consumer.DroppedCount(); d > dropped {
			dropped = d
		}
	}
	writeJSON(w, map[string]any{
		"traces":  traces,
		"dropped": dropped,
	})
}

func (a *api) contractFindingHistory(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if !a.isCallerTrusted(p) {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	hist, err := a.store.ListFindingHistory(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var out []map[string]any
	for _, h := range hist {
		out = append(out, map[string]any{
			"id":         h.ID,
			"finding_id": h.FindingID,
			"at":         h.At,
			"who":        h.Who,
			"old_status": h.OldStatus,
			"new_status": h.NewStatus,
			"note":       h.Note,
		})
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, out)
}

func (a *api) contractSignaturesList(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if !a.isCallerTrusted(p) {
		http.NotFound(w, r)
		return
	}
	state := r.URL.Query().Get("state")
	sigs, err := a.store.ListContractSignatures(state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var out []map[string]any
	for _, s := range sigs {
		out = append(out, map[string]any{
			"model":           s.Model,
			"client_format":   s.ClientFormat,
			"clientFormat":    s.ClientFormat,
			"direction":       s.Direction,
			"path":            s.Path,
			"type":            s.Type,
			"verdict":         s.Verdict,
			"verdict_state":   s.VerdictState,
			"verdictState":    s.VerdictState,
			"error_count":     s.ErrorCount,
			"judged_at":       s.JudgedAt,
			"key_id":          s.KeyID,
			"first_trace_id":  s.FirstTraceID,
			"reducer_version": s.ReducerVersion,
		})
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, out)
}

func (a *api) contractReviewSignature(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if !p.admin || !a.isCallerTrusted(p) {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Model        string `json:"model"`
		ClientFormat string `json:"clientFormat"`
		Direction    string `json:"direction"`
		Path         string `json:"path"`
		Type         string `json:"type"`
		Action       string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Action != "approve" && body.Action != "reject" {
		writeError(w, http.StatusBadRequest, "action must be 'approve' or 'reject'")
		return
	}
	sig, err := a.store.ReviewSignatureVerdict(body.Model, body.ClientFormat, body.Direction, body.Path, body.Type, body.Action)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if sig.FirstTraceID != "" {
		trace, err := a.store.GetContractTrace(sig.FirstTraceID)
		if err == nil && trace.Trusted {
			cand := contract.Candidate{
				Model:        sig.Model,
				ClientFormat: sig.ClientFormat,
				Direction:    sig.Direction,
				Path:         sig.Path,
				Type:         sig.Type,
			}
			if body.Action == "reject" {
				_ = contract.HandleLostCandidate(a.store, trace, cand, "")
			} else if body.Action == "approve" && strings.HasPrefix(sig.Verdict, "renamed") {
				target := strings.TrimPrefix(sig.Verdict, "renamed:")
				_ = contract.HandleLostCandidate(a.store, trace, cand, target)
			}
		}
	}

	writeJSON(w, map[string]any{"ok": true, "verdictState": sig.VerdictState, "verdict": sig.Verdict})
}

func (a *api) contractPruneLoop() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		if err := a.store.PruneContracts(time.Now(), contract.ReducerVersion); err != nil {
			log.Printf("contract prune: %v", err)
		}
	}
}
