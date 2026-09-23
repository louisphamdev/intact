package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ContractTrace records metadata of one proxied or golden exchange.
type ContractTrace struct {
	ID              string `json:"id"`
	KeyID           string `json:"keyId"`
	Trusted         bool   `json:"trusted"`
	Created         int64  `json:"created"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ClientFormat    string `json:"clientFormat"`
	UpstreamFormat  string `json:"upstreamFormat"`
	Tool            string `json:"tool"`
	ToolVersion     string `json:"toolVersion"`
	SwitcherVersion string `json:"switcherVersion"`
	Status          string `json:"status"`
	ReducerVersion  int    `json:"reducerVersion"`
	Direction       string `json:"direction,omitempty"`
}

// ContractShape holds the reduced shape record of one half.
type ContractShape struct {
	TraceID   string `json:"traceId"`
	Half      string `json:"half"`
	Direction string `json:"direction"`
	Record    string `json:"record"`
	CreatedAt int64  `json:"createdAt"`
}

// ContractFinding records a field discrepancy between switcher and intact.
type ContractFinding struct {
	ID                string  `json:"id"`
	Model             string  `json:"model"`
	ClientFormat      string  `json:"clientFormat"`
	Direction         string  `json:"direction"`
	Path              string  `json:"path"`
	ReducerVersion    int     `json:"reducerVersion"`
	Tool              string  `json:"tool"`
	Class             string  `json:"class"`
	Mapping           string  `json:"mapping"`
	Confidence        float64 `json:"confidence"`
	FirstTrace        string  `json:"firstTrace"`
	FirstTraceTrusted bool    `json:"first_trace_trusted"`
	LastTrace         string  `json:"lastTrace"`
	ExemptTrace       string  `json:"exemptTrace"`
	Count             int     `json:"count"`
	Status            string  `json:"status"`
	FixedIn           string  `json:"fixedIn"`
	CleanTraces       int     `json:"cleanTraces"`
	CreatedAt         int64   `json:"createdAt"`
	UpdatedAt         int64   `json:"updatedAt"`
}

// ContractFindingHistory logs status transitions on a finding.
type ContractFindingHistory struct {
	ID        int64  `json:"id"`
	FindingID string `json:"findingId"`
	At        int64  `json:"at"`
	Who       string `json:"who"`
	OldStatus string `json:"oldStatus"`
	NewStatus string `json:"newStatus"`
	Note      string `json:"note"`
}

// ContractLearned records field structure observed from trusted traffic.
type ContractLearned struct {
	Kind           string `json:"kind"`
	Subject        string `json:"subject"`
	Direction      string `json:"direction"`
	Half           string `json:"half"`
	Format         string `json:"format"`
	Event          string `json:"event"`
	Path           string `json:"path"`
	Type           string `json:"type"`
	Seen           int    `json:"seen"`
	FirstSeen      int64  `json:"firstSeen"`
	LastSeen       int64  `json:"lastSeen"`
	Gone           int    `json:"gone"`
	ReducerVersion int    `json:"reducerVersion"`
	KeyID          string `json:"keyId"`
	FirstTraceID   string `json:"firstTraceId"`
}

// ContractSignature caches judge decisions per unique candidate signature.
type ContractSignature struct {
	Model          string `json:"model"`
	ClientFormat   string `json:"clientFormat"`
	Direction      string `json:"direction"`
	Path           string `json:"path"`
	Type           string `json:"type"`
	Verdict        string `json:"verdict"`
	VerdictState   string `json:"verdictState"`
	ErrorCount     int    `json:"errorCount"`
	JudgedAt       int64  `json:"judgedAt"`
	LeaseUntil     int64  `json:"leaseUntil"`
	KeyID          string `json:"keyId"`
	FirstTraceID   string `json:"firstTraceId"`
	ReducerVersion int    `json:"reducerVersion"`
}

const contractSchema = `
CREATE TABLE IF NOT EXISTS contract_traces (
	id               TEXT PRIMARY KEY,
	key_id           TEXT NOT NULL DEFAULT '',
	trusted          INTEGER NOT NULL DEFAULT 0,
	created          INTEGER NOT NULL,
	provider         TEXT NOT NULL DEFAULT '',
	model            TEXT NOT NULL DEFAULT '',
	client_format    TEXT NOT NULL DEFAULT '',
	upstream_format  TEXT NOT NULL DEFAULT '',
	tool             TEXT NOT NULL DEFAULT '',
	tool_version     TEXT NOT NULL DEFAULT '',
	switcher_version TEXT NOT NULL DEFAULT '',
	status           TEXT NOT NULL,
	reducer_version  INTEGER NOT NULL DEFAULT 1,
	direction        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS contract_shapes (
	trace_id   TEXT NOT NULL,
	half       TEXT NOT NULL,
	direction  TEXT NOT NULL,
	record     TEXT NOT NULL,
	created_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (trace_id, half, direction)
);

CREATE TABLE IF NOT EXISTS contract_learned (
	kind            TEXT NOT NULL,
	subject         TEXT NOT NULL,
	direction       TEXT NOT NULL,
	half            TEXT NOT NULL,
	format          TEXT NOT NULL,
	event           TEXT NOT NULL,
	path            TEXT NOT NULL,
	type            TEXT NOT NULL,
	seen            INTEGER NOT NULL DEFAULT 0,
	first_seen      INTEGER NOT NULL DEFAULT 0,
	last_seen       INTEGER NOT NULL DEFAULT 0,
	gone            INTEGER NOT NULL DEFAULT 0,
	reducer_version INTEGER NOT NULL DEFAULT 1,
	key_id          TEXT NOT NULL DEFAULT '',
	first_trace_id  TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (kind, subject, direction, half, format, event, path)
);

CREATE TABLE IF NOT EXISTS contract_findings (
	id                  TEXT PRIMARY KEY,
	model               TEXT NOT NULL,
	client_format       TEXT NOT NULL,
	direction           TEXT NOT NULL,
	path                TEXT NOT NULL,
	reducer_version     INTEGER NOT NULL DEFAULT 1,
	tool                TEXT NOT NULL DEFAULT '',
	class               TEXT NOT NULL,
	mapping             TEXT NOT NULL DEFAULT '',
	confidence          REAL NOT NULL DEFAULT 1.0,
	first_trace         TEXT NOT NULL,
	first_trace_trusted INTEGER NOT NULL DEFAULT 0,
	last_trace          TEXT NOT NULL,
	exempt_trace        TEXT NOT NULL DEFAULT '',
	count               INTEGER NOT NULL DEFAULT 1,
	status              TEXT NOT NULL DEFAULT 'open',
	fixed_in            TEXT NOT NULL DEFAULT '',
	clean_traces        INTEGER NOT NULL DEFAULT 0,
	created_at          INTEGER NOT NULL DEFAULT 0,
	updated_at          INTEGER NOT NULL DEFAULT 0,
	UNIQUE (model, client_format, direction, path)
);

CREATE TABLE IF NOT EXISTS contract_counters (
	name       TEXT PRIMARY KEY,
	count      INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS contract_finding_history (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	finding_id TEXT NOT NULL,
	at         INTEGER NOT NULL,
	who        TEXT NOT NULL,
	old_status TEXT NOT NULL,
	new_status TEXT NOT NULL,
	note       TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS contract_signatures (
	model           TEXT NOT NULL,
	client_format   TEXT NOT NULL,
	direction       TEXT NOT NULL,
	path            TEXT NOT NULL,
	type            TEXT NOT NULL,
	verdict         TEXT NOT NULL DEFAULT '',
	verdict_state   TEXT NOT NULL DEFAULT 'pending',
	error_count     INTEGER NOT NULL DEFAULT 0,
	judged_at       INTEGER NOT NULL DEFAULT 0,
	lease_until     INTEGER NOT NULL DEFAULT 0,
	key_id          TEXT NOT NULL DEFAULT '',
	first_trace_id  TEXT NOT NULL DEFAULT '',
	reducer_version INTEGER NOT NULL DEFAULT 1,
	PRIMARY KEY (model, client_format, direction, path, type)
);`

// GetContractKey retrieves the persistent 32-byte HMAC secret, creating it on first use.
func (s *Store) GetContractKey() ([]byte, error) {
	val, err := s.GetSetting("INTACT_CONTRACT_KEY")
	if err != nil {
		return nil, err
	}
	if val != "" {
		b, err := hex.DecodeString(val)
		if err == nil && len(b) == 32 {
			return b, nil
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate contract key: %w", err)
	}
	if err := s.SetSetting("INTACT_CONTRACT_KEY", hex.EncodeToString(key)); err != nil {
		return nil, err
	}
	return key, nil
}

// InsertContractTrace creates a new trace record.
func (s *Store) InsertContractTrace(t ContractTrace) error {
	tr := 0
	if t.Trusted {
		tr = 1
	}
	_, err := s.DB.Exec(`INSERT INTO contract_traces (
		id, key_id, trusted, created, provider, model, client_format, upstream_format,
		tool, tool_version, switcher_version, status, reducer_version, direction
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.KeyID, tr, t.Created, t.Provider, t.Model, t.ClientFormat, t.UpstreamFormat,
		t.Tool, t.ToolVersion, t.SwitcherVersion, t.Status, t.ReducerVersion, t.Direction,
	)
	return err
}

// GetContractTrace loads a trace by id.
func (s *Store) GetContractTrace(id string) (ContractTrace, error) {
	var t ContractTrace
	var tr int
	err := s.DB.QueryRow(`SELECT id, key_id, trusted, created, provider, model, client_format,
		upstream_format, tool, tool_version, switcher_version, status, reducer_version, direction
		FROM contract_traces WHERE id = ?`, id).Scan(
		&t.ID, &t.KeyID, &tr, &t.Created, &t.Provider, &t.Model, &t.ClientFormat,
		&t.UpstreamFormat, &t.Tool, &t.ToolVersion, &t.SwitcherVersion, &t.Status,
		&t.ReducerVersion, &t.Direction,
	)
	if err != nil {
		return ContractTrace{}, err
	}
	t.Trusted = tr != 0
	return t, nil
}

// UpdateContractTraceStatusCAS updates trace status if it currently matches fromStatus.
func (s *Store) UpdateContractTraceStatusCAS(id, fromStatus, toStatus string) (bool, error) {
	res, err := s.DB.Exec(`UPDATE contract_traces SET status = ? WHERE id = ? AND status = ?`, toStatus, id, fromStatus)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// UpdateContractTraceStatus sets the status of a trace unconditionally.
func (s *Store) UpdateContractTraceStatus(id, toStatus string) error {
	_, err := s.DB.Exec(`UPDATE contract_traces SET status = ? WHERE id = ?`, toStatus, id)
	return err
}

// UpdateContractTraceTrusted updates the trusted flag of a trace.
func (s *Store) UpdateContractTraceTrusted(id string, trusted bool) error {
	v := 0
	if trusted {
		v = 1
	}
	_, err := s.DB.Exec(`UPDATE contract_traces SET trusted = ? WHERE id = ?`, v, id)
	return err
}

// ResetStartupTraces moves uncompleted in-flight traces to 'lost' on process boot.
func (s *Store) ResetStartupTraces() error {
	_, err := s.DB.Exec(`UPDATE contract_traces SET status = 'lost' WHERE status IN ('capturing', 'open', 'reducing')`)
	return err
}

// CommitIntactShapesAndSeal inserts intact shape records and transitions trace status in one transaction.
func (s *Store) CommitIntactShapesAndSeal(traceID, statusTo string, reqShapeJSON, respShapeJSON string, now int64) (bool, error) {
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if reqShapeJSON != "" {
		_, err = tx.Exec(`INSERT INTO contract_shapes (trace_id, half, direction, record, created_at)
			VALUES (?, 'intact', 'request', ?, ?)
			ON CONFLICT(trace_id, half, direction) DO UPDATE SET record=excluded.record, created_at=excluded.created_at`,
			traceID, reqShapeJSON, now)
		if err != nil {
			return false, err
		}
	}
	if respShapeJSON != "" {
		_, err = tx.Exec(`INSERT INTO contract_shapes (trace_id, half, direction, record, created_at)
			VALUES (?, 'intact', 'response', ?, ?)
			ON CONFLICT(trace_id, half, direction) DO UPDATE SET record=excluded.record, created_at=excluded.created_at`,
			traceID, respShapeJSON, now)
		if err != nil {
			return false, err
		}
	}

	res, err := tx.Exec(`UPDATE contract_traces SET status = ? WHERE id = ? AND status = 'capturing'`, statusTo, traceID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, nil
	}
	return true, tx.Commit()
}

// UpdateContractTraceVersions updates version and format metadata on a trace.
func (s *Store) UpdateContractTraceVersions(id, toolVersion, switcherVersion, inFormat, outFormat string) error {
	_, err := s.DB.Exec(`UPDATE contract_traces SET
		tool_version = CASE WHEN ? != '' THEN ? ELSE tool_version END,
		switcher_version = CASE WHEN ? != '' THEN ? ELSE switcher_version END,
		client_format = CASE WHEN ? != '' THEN ? ELSE client_format END,
		upstream_format = CASE WHEN ? != '' THEN ? ELSE upstream_format END
		WHERE id = ?`,
		toolVersion, toolVersion,
		switcherVersion, switcherVersion,
		inFormat, inFormat,
		outFormat, outFormat,
		id)
	return err
}

// InsertContractShape persists a reduced shape JSON for one half and direction.
func (s *Store) InsertContractShape(traceID, half, direction, record string, createdAt int64) error {
	if createdAt == 0 {
		createdAt = time.Now().UnixMilli()
	}
	_, err := s.DB.Exec(`INSERT INTO contract_shapes (trace_id, half, direction, record, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(trace_id, half, direction) DO UPDATE SET record=excluded.record, created_at=excluded.created_at`,
		traceID, half, direction, record, createdAt)
	return err
}

// GetContractShape returns the recorded shape for one half.
func (s *Store) GetContractShape(traceID, half, direction string) (string, error) {
	var rec string
	err := s.DB.QueryRow(`SELECT record FROM contract_shapes WHERE trace_id = ? AND half = ? AND direction = ?`,
		traceID, half, direction).Scan(&rec)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return rec, err
}

// ListContractShapesForTrace returns all saved shapes for a trace.
func (s *Store) ListContractShapesForTrace(traceID string) ([]ContractShape, error) {
	rows, err := s.DB.Query(`SELECT trace_id, half, direction, record, created_at FROM contract_shapes WHERE trace_id = ?`, traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContractShape
	for rows.Next() {
		var cs ContractShape
		if err := rows.Scan(&cs.TraceID, &cs.Half, &cs.Direction, &cs.Record, &cs.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// CountContractShapes returns the number of saved shapes for a model and direction.
func (s *Store) CountContractShapes(model, direction string) (int, error) {
	var cnt int
	err := s.DB.QueryRow(`SELECT count(*) FROM contract_shapes s
		JOIN contract_traces t ON s.trace_id = t.id
		WHERE t.model = ? AND s.direction = ?`, model, direction).Scan(&cnt)
	return cnt, err
}

// UpsertContractFinding inserts or updates a finding row.
func (s *Store) UpsertContractFinding(f ContractFinding) error {
	now := time.Now().UnixMilli()
	if f.CreatedAt == 0 {
		f.CreatedAt = now
	}
	if f.UpdatedAt == 0 {
		f.UpdatedAt = now
	}
	tr := 0
	if f.FirstTraceTrusted {
		tr = 1
	}
	if f.ReducerVersion == 0 {
		f.ReducerVersion = 1
	}
	if f.Confidence == 0 {
		f.Confidence = 1.0
	}
	if f.Status == "" {
		f.Status = "open"
	}

	_, err := s.DB.Exec(`INSERT INTO contract_findings (
		id, model, client_format, direction, path, reducer_version, tool, class,
		mapping, confidence, first_trace, first_trace_trusted, last_trace,
		exempt_trace, count, status, fixed_in, clean_traces, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(model, client_format, direction, path) DO UPDATE SET
		reducer_version=excluded.reducer_version,
		tool=excluded.tool,
		class=excluded.class,
		mapping=excluded.mapping,
		confidence=excluded.confidence,
		last_trace=excluded.last_trace,
		exempt_trace=CASE WHEN excluded.exempt_trace != '' THEN excluded.exempt_trace ELSE contract_findings.exempt_trace END,
		count=contract_findings.count + 1,
		status=CASE WHEN contract_findings.status = 'fixed' AND excluded.status = 'open' THEN 'open' ELSE contract_findings.status END,
		updated_at=excluded.updated_at`,
		f.ID, f.Model, f.ClientFormat, f.Direction, f.Path, f.ReducerVersion, f.Tool, f.Class,
		f.Mapping, f.Confidence, f.FirstTrace, tr, f.LastTrace,
		f.ExemptTrace, f.Count, f.Status, f.FixedIn, f.CleanTraces, f.CreatedAt, f.UpdatedAt,
	)
	return err
}

// GetContractFinding loads a finding by id.
func (s *Store) GetContractFinding(id string) (ContractFinding, error) {
	var f ContractFinding
	var tr int
	err := s.DB.QueryRow(`SELECT id, model, client_format, direction, path, reducer_version,
		tool, class, mapping, confidence, first_trace, first_trace_trusted, last_trace,
		exempt_trace, count, status, fixed_in, clean_traces, created_at, updated_at
		FROM contract_findings WHERE id = ?`, id).Scan(
		&f.ID, &f.Model, &f.ClientFormat, &f.Direction, &f.Path, &f.ReducerVersion,
		&f.Tool, &f.Class, &f.Mapping, &f.Confidence, &f.FirstTrace, &tr, &f.LastTrace,
		&f.ExemptTrace, &f.Count, &f.Status, &f.FixedIn, &f.CleanTraces, &f.CreatedAt, &f.UpdatedAt,
	)
	if err != nil {
		return ContractFinding{}, err
	}
	f.FirstTraceTrusted = tr != 0
	return f, nil
}

// GetContractFindingByPath loads a finding by its unique composite path.
func (s *Store) GetContractFindingByPath(model, clientFormat, direction, path string) (ContractFinding, error) {
	var f ContractFinding
	var tr int
	err := s.DB.QueryRow(`SELECT id, model, client_format, direction, path, reducer_version,
		tool, class, mapping, confidence, first_trace, first_trace_trusted, last_trace,
		exempt_trace, count, status, fixed_in, clean_traces, created_at, updated_at
		FROM contract_findings WHERE model = ? AND client_format = ? AND direction = ? AND path = ?`,
		model, clientFormat, direction, path).Scan(
		&f.ID, &f.Model, &f.ClientFormat, &f.Direction, &f.Path, &f.ReducerVersion,
		&f.Tool, &f.Class, &f.Mapping, &f.Confidence, &f.FirstTrace, &tr, &f.LastTrace,
		&f.ExemptTrace, &f.Count, &f.Status, &f.FixedIn, &f.CleanTraces, &f.CreatedAt, &f.UpdatedAt,
	)
	if err != nil {
		return ContractFinding{}, err
	}
	f.FirstTraceTrusted = tr != 0
	return f, nil
}

// ListContractFindings returns findings matching the given filters.
func (s *Store) ListContractFindings(status string, since int64, trustedOnly bool) ([]ContractFinding, error) {
	q := `SELECT id, model, client_format, direction, path, reducer_version,
		tool, class, mapping, confidence, first_trace, first_trace_trusted, last_trace,
		exempt_trace, count, status, fixed_in, clean_traces, created_at, updated_at
		FROM contract_findings WHERE 1=1`
	var args []any
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if since > 0 {
		q += ` AND updated_at >= ?`
		args = append(args, since)
	}
	if trustedOnly {
		q += ` AND first_trace_trusted = 1`
	}
	q += ` ORDER BY updated_at DESC`

	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ContractFinding
	for rows.Next() {
		var f ContractFinding
		var tr int
		if err := rows.Scan(
			&f.ID, &f.Model, &f.ClientFormat, &f.Direction, &f.Path, &f.ReducerVersion,
			&f.Tool, &f.Class, &f.Mapping, &f.Confidence, &f.FirstTrace, &tr, &f.LastTrace,
			&f.ExemptTrace, &f.Count, &f.Status, &f.FixedIn, &f.CleanTraces, &f.CreatedAt, &f.UpdatedAt,
		); err != nil {
			return nil, err
		}
		f.FirstTraceTrusted = tr != 0
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpdateContractFindingStatus performs a conditional CAS update on finding status and logs history.
func (s *Store) UpdateContractFindingStatus(id, oldStatus, newStatus, who, note, fixedIn string) (bool, error) {
	now := time.Now().UnixMilli()
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE contract_findings SET status = ?, fixed_in = CASE WHEN ? != '' THEN ? ELSE fixed_in END, updated_at = ?
		WHERE id = ? AND status = ?`, newStatus, fixedIn, fixedIn, now, id, oldStatus)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}

	if _, err := tx.Exec(`INSERT INTO contract_finding_history (finding_id, at, who, old_status, new_status, note)
		VALUES (?, ?, ?, ?, ?, ?)`, id, now, who, oldStatus, newStatus, note); err != nil {
		return false, err
	}

	return true, tx.Commit()
}

// UpsertContractLearned writes learned path observations for trusted records.
func (s *Store) UpsertContractLearned(l ContractLearned) error {
	if l.ReducerVersion == 0 {
		l.ReducerVersion = 1
	}
	now := time.Now().UnixMilli()
	if l.FirstSeen == 0 {
		l.FirstSeen = now
	}
	if l.LastSeen == 0 {
		l.LastSeen = now
	}
	_, err := s.DB.Exec(`INSERT INTO contract_learned (
		kind, subject, direction, half, format, event, path, type, seen, first_seen, last_seen, gone, reducer_version, key_id, first_trace_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(kind, subject, direction, half, format, event, path) DO UPDATE SET
		type=excluded.type,
		seen=contract_learned.seen + excluded.seen,
		last_seen=excluded.last_seen,
		gone=0`,
		l.Kind, l.Subject, l.Direction, l.Half, l.Format, l.Event, l.Path, l.Type,
		l.Seen, l.FirstSeen, l.LastSeen, l.Gone, l.ReducerVersion, l.KeyID, l.FirstTraceID,
	)
	return err
}

// ListContractLearned returns learned paths for an endpoint/model.
func (s *Store) ListContractLearned(kind, subject string) ([]ContractLearned, error) {
	rows, err := s.DB.Query(`SELECT kind, subject, direction, half, format, event, path, type, seen, first_seen, last_seen, gone, reducer_version, key_id, first_trace_id
		FROM contract_learned WHERE kind = ? AND subject = ?`, kind, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContractLearned
	for rows.Next() {
		var l ContractLearned
		if err := rows.Scan(&l.Kind, &l.Subject, &l.Direction, &l.Half, &l.Format, &l.Event, &l.Path,
			&l.Type, &l.Seen, &l.FirstSeen, &l.LastSeen, &l.Gone, &l.ReducerVersion, &l.KeyID, &l.FirstTraceID); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ResetContract purges learned rows and signatures by key id or by reducer version.
func (s *Store) ResetContract(keyID string, reducerVersion int) error {
	if keyID != "" && reducerVersion > 0 {
		return errors.New("cannot specify both key and reducer version")
	}
	if keyID == "" && reducerVersion <= 0 {
		return errors.New("must specify either key or reducer version")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if keyID != "" {
		if _, err := tx.Exec(`DELETE FROM contract_learned WHERE key_id = ?`, keyID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM contract_signatures WHERE key_id = ?`, keyID); err != nil {
			return err
		}
	}
	if reducerVersion > 0 {
		if _, err := tx.Exec(`DELETE FROM contract_learned WHERE reducer_version = ?`, reducerVersion); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM contract_signatures WHERE reducer_version = ?`, reducerVersion); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneContracts executes the 5 daily cleanup steps in separate transactions.
func (s *Store) PruneContracts(now time.Time, currentReducerVersion int) error {
	nowMs := now.UnixMilli()
	day24hAgo := now.Add(-24 * time.Hour).UnixMilli()
	day30Ago := now.Add(-30 * 24 * time.Hour).UnixMilli()
	day90Ago := now.Add(-90 * 24 * time.Hour).UnixMilli()

	var errs []error

	// Step 1: Shapes retention.
	// Find exempt traces for unresolved findings (at most 20 per model).
	exemptTraces := map[string]bool{}
	rows, err := s.DB.Query(`SELECT model, exempt_trace FROM contract_findings
		WHERE status = 'open' AND exempt_trace != '' ORDER BY model, updated_at DESC`)
	if err == nil {
		counts := map[string]int{}
		for rows.Next() {
			var m, tr string
			if err := rows.Scan(&m, &tr); err == nil {
				if counts[m] < 20 {
					exemptTraces[tr] = true
					counts[m]++
				}
			}
		}
		rows.Close()
	}

	tx1, err := s.DB.Begin()
	if err != nil {
		errs = append(errs, fmt.Errorf("prune step 1 begin: %w", err))
	} else {
		// Untrusted shapes deleted after 24h.
		if _, err := tx1.Exec(`DELETE FROM contract_shapes WHERE trace_id IN (
			SELECT id FROM contract_traces WHERE trusted = 0 AND created < ?
		)`, day24hAgo); err != nil {
			errs = append(errs, fmt.Errorf("prune step 1 untrusted: %w", err))
		}

		// Shapes older than 30 days deleted unless exempt.
		shapeRows, qErr := tx1.Query(`SELECT DISTINCT trace_id FROM contract_shapes WHERE created_at < ?`, day30Ago)
		if qErr != nil {
			errs = append(errs, fmt.Errorf("prune step 1 query 30d shapes: %w", qErr))
		} else {
			var toDelTraces []string
			for shapeRows.Next() {
				var tr string
				if shapeRows.Scan(&tr) == nil {
					if !exemptTraces[tr] {
						toDelTraces = append(toDelTraces, tr)
					}
				}
			}
			shapeRows.Close()
			for _, tr := range toDelTraces {
				if _, err := tx1.Exec(`DELETE FROM contract_shapes WHERE trace_id = ?`, tr); err != nil {
					errs = append(errs, fmt.Errorf("prune step 1 delete 30d trace %s: %w", tr, err))
				}
			}
		}

		// Prune beyond 200 trusted shapes per (subject, direction) where subject is model or golden tool.
		mRows, mErr := tx1.Query(`SELECT DISTINCT COALESCE(NULLIF(t.model, ''), t.tool) as subject, s.direction
			FROM contract_shapes s
			JOIN contract_traces t ON s.trace_id = t.id
			WHERE t.trusted = 1 AND COALESCE(NULLIF(t.model, ''), t.tool) != ''`)
		if mErr != nil {
			errs = append(errs, fmt.Errorf("prune step 1 query subjects: %w", mErr))
		} else {
			type subjKey struct{ subject, dir string }
			var subjs []subjKey
			for mRows.Next() {
				var k subjKey
				if mRows.Scan(&k.subject, &k.dir) == nil {
					subjs = append(subjs, k)
				}
			}
			mRows.Close()

			for _, k := range subjs {
				var cnt int
				_ = tx1.QueryRow(`SELECT count(*) FROM contract_shapes s
					JOIN contract_traces t ON s.trace_id = t.id
					WHERE COALESCE(NULLIF(t.model, ''), t.tool) = ? AND s.direction = ? AND t.trusted = 1`,
					k.subject, k.dir).Scan(&cnt)
				if cnt > 200 {
					excess := cnt - 200
					tRows, tErr := tx1.Query(`SELECT s.trace_id, count(*) as num_shapes FROM contract_shapes s
						JOIN contract_traces t ON s.trace_id = t.id
						WHERE COALESCE(NULLIF(t.model, ''), t.tool) = ? AND s.direction = ? AND t.trusted = 1
						GROUP BY s.trace_id
						ORDER BY min(s.created_at) ASC`, k.subject, k.dir)
					if tErr != nil {
						errs = append(errs, fmt.Errorf("prune step 1 excess query for %s: %w", k.subject, tErr))
					} else {
						type traceToDel struct {
							id    string
							count int
						}
						var toDelete []traceToDel
						deletedCount := 0
						for tRows.Next() {
							var td traceToDel
							if tRows.Scan(&td.id, &td.count) == nil {
								if !exemptTraces[td.id] {
									toDelete = append(toDelete, td)
									deletedCount += td.count
									if deletedCount >= excess {
										break
									}
								}
							}
						}
						tRows.Close()

						for _, td := range toDelete {
							if _, err := tx1.Exec(`DELETE FROM contract_shapes WHERE trace_id = ? AND direction = ?`, td.id, k.dir); err != nil {
								errs = append(errs, fmt.Errorf("prune step 1 delete shapes %s: %w", td.id, err))
							}
						}
					}
				}
			}
		}
		if err := tx1.Commit(); err != nil {
			errs = append(errs, fmt.Errorf("prune step 1 commit: %w", err))
		}
	}

	// Step 2: Delete contract_traces in terminal states older than 30 days with no shapes.
	tx2, err := s.DB.Begin()
	if err != nil {
		errs = append(errs, fmt.Errorf("prune step 2 begin: %w", err))
	} else {
		if _, err := tx2.Exec(`DELETE FROM contract_traces
			WHERE status IN ('expired', 'lost', 'failed', 'aborted', 'dropped', 'revoked', 'done')
			AND created < ?
			AND id NOT IN (SELECT trace_id FROM contract_shapes)`, day30Ago); err != nil {
			_ = tx2.Rollback()
			errs = append(errs, fmt.Errorf("prune step 2 exec: %w", err))
		} else {
			if err := tx2.Commit(); err != nil {
				errs = append(errs, fmt.Errorf("prune step 2 commit: %w", err))
			}
		}
	}

	// Step 3: Keep last 50 history rows per finding.
	tx3, err := s.DB.Begin()
	if err != nil {
		errs = append(errs, fmt.Errorf("prune step 3 begin: %w", err))
	} else {
		if _, err := tx3.Exec(`DELETE FROM contract_finding_history WHERE id NOT IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY finding_id ORDER BY at DESC) as rn
				FROM contract_finding_history
			) WHERE rn <= 50
		)`); err != nil {
			_ = tx3.Rollback()
			errs = append(errs, fmt.Errorf("prune step 3 exec: %w", err))
		} else {
			if err := tx3.Commit(); err != nil {
				errs = append(errs, fmt.Errorf("prune step 3 commit: %w", err))
			}
		}
	}

	// Step 4: Unresolved findings with no new trace in 90 days get 'expired'.
	tx4, err := s.DB.Begin()
	if err != nil {
		errs = append(errs, fmt.Errorf("prune step 4 begin: %w", err))
	} else {
		fRows, qErr := tx4.Query(`SELECT id FROM contract_findings WHERE status = 'open' AND updated_at < ?`, day90Ago)
		if qErr != nil {
			_ = tx4.Rollback()
			errs = append(errs, fmt.Errorf("prune step 4 query findings: %w", qErr))
		} else {
			var expiringIDs []string
			for fRows.Next() {
				var fid string
				if err := fRows.Scan(&fid); err == nil {
					expiringIDs = append(expiringIDs, fid)
				}
			}
			fRows.Close()

			var step4Err error
			for _, fid := range expiringIDs {
				res, err := tx4.Exec(`UPDATE contract_findings SET status = 'expired', updated_at = ?
					WHERE id = ? AND status = 'open'`, nowMs, fid)
				if err != nil {
					step4Err = err
					break
				}
				if n, _ := res.RowsAffected(); n > 0 {
					if _, err := tx4.Exec(`INSERT INTO contract_finding_history (finding_id, at, who, old_status, new_status, note)
						VALUES (?, ?, 'system', 'open', 'expired', 'expired by retention policy')`,
						fid, nowMs); err != nil {
						step4Err = err
						break
					}
				}
			}
			if step4Err != nil {
				_ = tx4.Rollback()
				errs = append(errs, fmt.Errorf("prune step 4 exec: %w", step4Err))
			} else {
				if err := tx4.Commit(); err != nil {
					errs = append(errs, fmt.Errorf("prune step 4 commit: %w", err))
				}
			}
		}
	}

	// Step 5: Delete contract_learned rows of a reducer version not current.
	tx5, err := s.DB.Begin()
	if err != nil {
		errs = append(errs, fmt.Errorf("prune step 5 begin: %w", err))
	} else {
		if _, err := tx5.Exec(`DELETE FROM contract_learned WHERE reducer_version != ?`, currentReducerVersion); err != nil {
			_ = tx5.Rollback()
			errs = append(errs, fmt.Errorf("prune step 5 exec: %w", err))
		} else {
			if err := tx5.Commit(); err != nil {
				errs = append(errs, fmt.Errorf("prune step 5 commit: %w", err))
			}
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// MigrateContract ensures schema migrations for contract tables.
func (s *Store) MigrateContract() error {
	rows, err := s.DB.Query(`PRAGMA table_info(contract_findings)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasCleanTraces := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "clean_traces" {
			hasCleanTraces = true
			break
		}
	}
	if !hasCleanTraces {
		if _, err := s.DB.Exec(`ALTER TABLE contract_findings ADD COLUMN clean_traces INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}

// GetContractSignature loads a signature by primary key.
func (s *Store) GetContractSignature(model, clientFormat, direction, path, typeStr string) (ContractSignature, error) {
	var sig ContractSignature
	err := s.DB.QueryRow(`SELECT model, client_format, direction, path, type, verdict, verdict_state,
		error_count, judged_at, lease_until, key_id, first_trace_id, reducer_version
		FROM contract_signatures WHERE model = ? AND client_format = ? AND direction = ? AND path = ? AND type = ?`,
		model, clientFormat, direction, path, typeStr,
	).Scan(
		&sig.Model, &sig.ClientFormat, &sig.Direction, &sig.Path, &sig.Type, &sig.Verdict, &sig.VerdictState,
		&sig.ErrorCount, &sig.JudgedAt, &sig.LeaseUntil, &sig.KeyID, &sig.FirstTraceID, &sig.ReducerVersion,
	)
	if err != nil {
		return ContractSignature{}, err
	}
	return sig, nil
}

// InsertContractSignature inserts a new signature row if it doesn't already exist.
func (s *Store) InsertContractSignature(sig ContractSignature) error {
	if sig.ReducerVersion == 0 {
		sig.ReducerVersion = 1
	}
	if sig.VerdictState == "" {
		sig.VerdictState = "pending"
	}
	_, err := s.DB.Exec(`INSERT INTO contract_signatures (
		model, client_format, direction, path, type, verdict, verdict_state,
		error_count, judged_at, lease_until, key_id, first_trace_id, reducer_version
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(model, client_format, direction, path, type) DO NOTHING`,
		sig.Model, sig.ClientFormat, sig.Direction, sig.Path, sig.Type, sig.Verdict, sig.VerdictState,
		sig.ErrorCount, sig.JudgedAt, sig.LeaseUntil, sig.KeyID, sig.FirstTraceID, sig.ReducerVersion,
	)
	return err
}

// LeaseContractSignature acquires a 5-minute lease if currently pending and expired.
func (s *Store) LeaseContractSignature(model, clientFormat, direction, path, typeStr string, leaseUntil int64) (bool, error) {
	now := time.Now().UnixMilli()
	res, err := s.DB.Exec(`UPDATE contract_signatures SET lease_until = ?
		WHERE model = ? AND client_format = ? AND direction = ? AND path = ? AND type = ?
		AND verdict_state = 'pending' AND lease_until <= ?`,
		leaseUntil, model, clientFormat, direction, path, typeStr, now,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// IncrementSignatureError increments error_count and marks 'error' after 3 errors.
func (s *Store) IncrementSignatureError(model, clientFormat, direction, path, typeStr string) error {
	_, err := s.DB.Exec(`UPDATE contract_signatures SET
		error_count = error_count + 1,
		verdict_state = CASE WHEN error_count + 1 >= 3 THEN 'error' ELSE verdict_state END,
		lease_until = 0
		WHERE model = ? AND client_format = ? AND direction = ? AND path = ? AND type = ?`,
		model, clientFormat, direction, path, typeStr,
	)
	return err
}

// UpdateSignatureVerdict sets the verdict and resets the lease.
func (s *Store) UpdateSignatureVerdict(model, clientFormat, direction, path, typeStr, verdict, verdictState string, judgedAt int64) error {
	_, err := s.DB.Exec(`UPDATE contract_signatures SET
		verdict = ?, verdict_state = ?, judged_at = ?, lease_until = 0
		WHERE model = ? AND client_format = ? AND direction = ? AND path = ? AND type = ?`,
		verdict, verdictState, judgedAt, model, clientFormat, direction, path, typeStr,
	)
	return err
}

// ListContractSignatures returns signatures filtered by state.
func (s *Store) ListContractSignatures(state string) ([]ContractSignature, error) {
	q := `SELECT model, client_format, direction, path, type, verdict, verdict_state,
		error_count, judged_at, lease_until, key_id, first_trace_id, reducer_version
		FROM contract_signatures WHERE 1=1`
	var args []any
	if state != "" {
		q += ` AND verdict_state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY judged_at DESC`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContractSignature
	for rows.Next() {
		var sig ContractSignature
		if err := rows.Scan(
			&sig.Model, &sig.ClientFormat, &sig.Direction, &sig.Path, &sig.Type, &sig.Verdict, &sig.VerdictState,
			&sig.ErrorCount, &sig.JudgedAt, &sig.LeaseUntil, &sig.KeyID, &sig.FirstTraceID, &sig.ReducerVersion,
		); err != nil {
			return nil, err
		}
		out = append(out, sig)
	}
	return out, rows.Err()
}

// IncrementFindingCleanTraces increments the clean trace count of an open finding.
func (s *Store) IncrementFindingCleanTraces(id string) error {
	_, err := s.DB.Exec(`UPDATE contract_findings SET clean_traces = clean_traces + 1, updated_at = ? WHERE id = ?`, time.Now().UnixMilli(), id)
	return err
}

// ListTrustedSwitcherVersions returns all switcher versions for a model from trusted traces.
func (s *Store) ListTrustedSwitcherVersions(model string) ([]string, error) {
	rows, err := s.DB.Query(`SELECT switcher_version FROM contract_traces WHERE model = ? AND trusted = 1 AND switcher_version != ''`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ver string
		if err := rows.Scan(&ver); err == nil && ver != "" {
			out = append(out, ver)
		}
	}
	return out, rows.Err()
}

// GetLastFindingHistory returns the most recent history row for a finding.
func (s *Store) GetLastFindingHistory(findingID string) (ContractFindingHistory, error) {
	var h ContractFindingHistory
	err := s.DB.QueryRow(`SELECT id, finding_id, at, who, old_status, new_status, note
		FROM contract_finding_history WHERE finding_id = ? ORDER BY at DESC, id DESC LIMIT 1`, findingID).Scan(
		&h.ID, &h.FindingID, &h.At, &h.Who, &h.OldStatus, &h.NewStatus, &h.Note,
	)
	return h, err
}

// IncrementContractCounter increments a persistent counter.
func (s *Store) IncrementContractCounter(name string, delta int64) error {
	now := time.Now().UnixMilli()
	_, err := s.DB.Exec(`INSERT INTO contract_counters (name, count, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET count = count + excluded.count, updated_at = excluded.updated_at`,
		name, delta, now,
	)
	return err
}

// GetContractCounter reads a persistent counter value.
func (s *Store) GetContractCounter(name string) (int64, error) {
	var count int64
	err := s.DB.QueryRow(`SELECT count FROM contract_counters WHERE name = ?`, name).Scan(&count)
	if err != nil {
		return 0, nil
	}
	return count, nil
}

// ListFindingHistory returns all history rows for a finding ordered chronologically.
func (s *Store) ListFindingHistory(findingID string) ([]ContractFindingHistory, error) {
	rows, err := s.DB.Query(`SELECT id, finding_id, at, who, old_status, new_status, note
		FROM contract_finding_history WHERE finding_id = ? ORDER BY at ASC, id ASC`, findingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContractFindingHistory
	for rows.Next() {
		var h ContractFindingHistory
		if err := rows.Scan(&h.ID, &h.FindingID, &h.At, &h.Who, &h.OldStatus, &h.NewStatus, &h.Note); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListRecentTraces returns the most recent contract traces.
func (s *Store) ListRecentTraces(limit int) ([]ContractTrace, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.Query(`SELECT id, key_id, trusted, created, provider, model, client_format,
		upstream_format, tool, tool_version, switcher_version, status, reducer_version, direction
		FROM contract_traces ORDER BY created DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContractTrace
	for rows.Next() {
		var t ContractTrace
		var tr int
		if err := rows.Scan(
			&t.ID, &t.KeyID, &tr, &t.Created, &t.Provider, &t.Model, &t.ClientFormat,
			&t.UpstreamFormat, &t.Tool, &t.ToolVersion, &t.SwitcherVersion, &t.Status,
			&t.ReducerVersion, &t.Direction,
		); err != nil {
			return nil, err
		}
		t.Trusted = tr != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReviewSignatureVerdict approves or rejects a proposed signature verdict.
func (s *Store) ReviewSignatureVerdict(model, clientFormat, direction, path, typeStr, action string) (ContractSignature, error) {
	sig, err := s.GetContractSignature(model, clientFormat, direction, path, typeStr)
	if err != nil {
		return sig, err
	}
	now := time.Now().UnixMilli()
	if action == "approve" {
		sig.VerdictState = "approved"
		_, err = s.DB.Exec(`UPDATE contract_signatures SET verdict_state = 'approved', judged_at = ?
			WHERE model = ? AND client_format = ? AND direction = ? AND path = ? AND type = ?`,
			now, model, clientFormat, direction, path, typeStr)
	} else if action == "reject" {
		sig.Verdict = "lost"
		sig.VerdictState = "approved"
		_, err = s.DB.Exec(`UPDATE contract_signatures SET verdict = 'lost', verdict_state = 'approved', judged_at = ?
			WHERE model = ? AND client_format = ? AND direction = ? AND path = ? AND type = ?`,
			now, model, clientFormat, direction, path, typeStr)
	}
	return sig, err
}
