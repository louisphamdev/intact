package contract

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// ErrConflict indicates an optimistic concurrency conflict.
var ErrConflict = errors.New("conflict: finding status was modified concurrently")

// NewFindingID generates a url-safe finding ID matching ^[A-Za-z0-9_-]{1,64}$.
func NewFindingID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return "fnd_" + hex.EncodeToString(b)
}

// FormatChangeAlert formats the alert text holding count and finding id, never path or version.
func FormatChangeAlert(count int, findingID string) string {
	if findingID != "" {
		return fmt.Sprintf("Contract change detected: %d changes observed (finding: %s)", count, findingID)
	}
	return fmt.Sprintf("Contract change detected: %d changes observed", count)
}

// ResolveFinding resolves an existing finding to 'fixed' or 'wontfix'.
// A 'fixed' finding stores fixedIn: greatest switcherVersion among trusted traces of model.
// Untrusted half versions are never read.
func ResolveFinding(s *store.Store, findingID, targetStatus, who, note string) error {
	if targetStatus != "fixed" && targetStatus != "wontfix" {
		return errors.New("invalid status: must be fixed or wontfix")
	}

	finding, err := s.GetContractFinding(findingID)
	if err != nil {
		return err
	}
	if finding.Status == targetStatus {
		return ErrConflict
	}

	var fixedIn string
	if targetStatus == "fixed" || targetStatus == "wontfix" {
		versions, err := s.ListTrustedSwitcherVersions(finding.Model)
		if err != nil {
			return err
		}
		for _, v := range versions {
			if fixedIn == "" || CompareVersions(v, fixedIn) > 0 {
				fixedIn = v
			}
		}
	}

	ok, err := s.UpdateContractFindingStatus(findingID, finding.Status, targetStatus, who, note, fixedIn)
	if err != nil {
		return err
	}
	if !ok {
		return ErrConflict
	}
	return nil
}

// HandleLostCandidate handles finding creation or reopening for a lost candidate.
func HandleLostCandidate(s *store.Store, trace store.ContractTrace, cand Candidate, mappedPath string) error {
	if !trace.Trusted {
		return nil
	}

	existing, err := s.GetContractFindingByPath(cand.Model, cand.ClientFormat, cand.Direction, cand.Path)
	if err != nil || existing.ID == "" {
		// Create new finding
		findingID := NewFindingID()
		now := time.Now().UnixMilli()
		f := store.ContractFinding{
			ID:                findingID,
			Model:             cand.Model,
			ClientFormat:      cand.ClientFormat,
			Direction:         cand.Direction,
			Path:              cand.Path,
			ReducerVersion:    ReducerVersion,
			Tool:              trace.Tool,
			Class:             "lost",
			Mapping:           mappedPath,
			Confidence:        1.0,
			FirstTrace:        trace.ID,
			FirstTraceTrusted: true,
			LastTrace:         trace.ID,
			ExemptTrace:       trace.ID,
			Count:             1,
			Status:            "open",
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		if err := s.UpsertContractFinding(f); err != nil {
			return err
		}
		_, _ = s.DB.Exec(`INSERT INTO contract_finding_history (finding_id, at, who, old_status, new_status, note)
			VALUES (?, ?, ?, '', 'open', ?)`, findingID, now, trace.KeyID, "opened by trace "+trace.ID)
		return nil
	}

	// Finding exists: check status and reopening rules
	now := time.Now().UnixMilli()
	switch existing.Status {
	case "fixed":
		// Reopens only on a trusted trace with a greater switcherVersion that still shows it
		if trace.SwitcherVersion != "" && existing.FixedIn != "" && CompareVersions(trace.SwitcherVersion, existing.FixedIn) > 0 {
			ok, _ := s.UpdateContractFindingStatus(existing.ID, "fixed", "open", "system", "reopened by trace "+trace.ID, "")
			if ok {
				_, _ = s.DB.Exec(`UPDATE contract_findings SET count = count + 1, last_trace = ?, exempt_trace = ?, updated_at = ? WHERE id = ?`,
					trace.ID, trace.ID, now, existing.ID)
				return nil
			}
		}
		// Not reopened: still count it
		_, _ = s.DB.Exec(`UPDATE contract_findings SET count = count + 1, last_trace = ?, updated_at = ? WHERE id = ?`,
			trace.ID, now, existing.ID)

	case "wontfix":
		// A wontfix set by the session or master token never reopens. Set by trusted key can reopen.
		hist, err := s.GetLastFindingHistory(existing.ID)
		if err == nil && (hist.Who == "session" || hist.Who == "master" || strings.HasPrefix(hist.Who, "session") || strings.HasPrefix(hist.Who, "master")) {
			// Never reopens
			_, _ = s.DB.Exec(`UPDATE contract_findings SET count = count + 1, last_trace = ?, updated_at = ? WHERE id = ?`,
				trace.ID, now, existing.ID)
			return nil
		}
		// Can reopen if greater version
		if trace.SwitcherVersion != "" && existing.FixedIn != "" && CompareVersions(trace.SwitcherVersion, existing.FixedIn) > 0 {
			ok, _ := s.UpdateContractFindingStatus(existing.ID, "wontfix", "open", "system", "reopened by trace "+trace.ID, "")
			if ok {
				_, _ = s.DB.Exec(`UPDATE contract_findings SET count = count + 1, last_trace = ?, exempt_trace = ?, updated_at = ? WHERE id = ?`,
					trace.ID, trace.ID, now, existing.ID)
				return nil
			}
		}
		_, _ = s.DB.Exec(`UPDATE contract_findings SET count = count + 1, last_trace = ?, updated_at = ? WHERE id = ?`,
			trace.ID, now, existing.ID)

	case "expired":
		ok, _ := s.UpdateContractFindingStatus(existing.ID, "expired", "open", "system", "reopened by trace "+trace.ID, "")
		if ok {
			_, _ = s.DB.Exec(`UPDATE contract_findings SET count = count + 1, last_trace = ?, exempt_trace = ?, updated_at = ? WHERE id = ?`,
				trace.ID, trace.ID, now, existing.ID)
		}

	default: // "open"
		_, _ = s.DB.Exec(`UPDATE contract_findings SET count = count + 1, last_trace = ?, exempt_trace = ?, updated_at = ? WHERE id = ?`,
			trace.ID, trace.ID, now, existing.ID)
	}

	return nil
}
