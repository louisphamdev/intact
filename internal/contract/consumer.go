package contract

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// Consumer processes sealed traces in a single goroutine.
type Consumer struct {
	store            *store.Store
	judgeMgr         *JudgeManager
	queue            chan string
	droppedCount     atomic.Int64
	stop             chan struct{}
	wg               sync.WaitGroup
	alertFn          func(model, tool, alertText string)
	releaseQueueSlot func(keyID string, trusted bool)

	mu             sync.Mutex
	lastAlertAt    map[string]time.Time       // model or tool -> time
	changedModels  map[string]int             // model -> remaining high-sampling requests
	changedTools   map[string]bool            // tool -> changed
	learnedPaths   map[string]map[string]bool // model:direction -> path set
	pathGoneCounts map[string]map[string]int  // model:direction -> path -> consecutive gone count
	knownVersions  map[string]map[string]bool // tool -> version set
	knownEvents    map[string]map[string]bool // format -> event set
}

// NewConsumer initializes the consumer with a 256-slot trace queue.
func NewConsumer(s *store.Store, jm *JudgeManager, alertFn func(model, tool, alertText string)) *Consumer {
	return &Consumer{
		store:          s,
		judgeMgr:       jm,
		queue:          make(chan string, 256),
		stop:           make(chan struct{}),
		alertFn:        alertFn,
		lastAlertAt:    make(map[string]time.Time),
		changedModels:  make(map[string]int),
		changedTools:   make(map[string]bool),
		learnedPaths:   make(map[string]map[string]bool),
		pathGoneCounts: make(map[string]map[string]int),
		knownVersions:  make(map[string]map[string]bool),
		knownEvents:    make(map[string]map[string]bool),
	}
}

// Start spawns the consumer goroutine.
func (c *Consumer) Start() {
	c.wg.Add(1)
	go c.run()
}

// Stop stops the consumer and waits for it to exit.
func (c *Consumer) Stop() {
	close(c.stop)
	c.wg.Wait()
}

// Enqueue enqueues a sealed trace id for processing.
// When full, the trace is marked dropped, counted, and excluded from counters.
func (c *Consumer) Enqueue(traceID string) bool {
	select {
	case c.queue <- traceID:
		return true
	default:
		c.droppedCount.Add(1)
		_ = c.store.IncrementContractCounter("dropped_traces", 1)
		_, _ = c.store.UpdateContractTraceStatusCAS(traceID, "queued", "dropped")
		return false
	}
}

// DroppedCount returns the in-memory dropped count.
func (c *Consumer) DroppedCount() int64 {
	return c.droppedCount.Load()
}

// GetJudgeManager returns the JudgeManager instance.
func (c *Consumer) GetJudgeManager() *JudgeManager {
	return c.judgeMgr
}

// StartupRecovery recovers startup traces and feeds queued traces skipping the drop path.
func (c *Consumer) StartupRecovery() error {
	if err := c.store.ResetStartupTraces(); err != nil {
		return err
	}
	if c.judgeMgr != nil {
		if err := c.judgeMgr.ResetLeasesOnStartup(); err != nil {
			return err
		}
	}

	rows, err := c.store.DB.Query(`SELECT id FROM contract_traces WHERE status = 'queued' ORDER BY created ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var traceIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			traceIDs = append(traceIDs, id)
		}
	}
	for _, id := range traceIDs {
		c.processTraceSafe(id)
	}
	return nil
}

func (c *Consumer) run() {
	defer c.wg.Done()
	for {
		select {
		case <-c.stop:
			return
		case traceID := <-c.queue:
			c.processTraceSafe(traceID)
		}
	}
}

// ProcessTraceSync processes one trace synchronously (useful for tests and startup).
func (c *Consumer) ProcessTraceSync(traceID string) {
	c.processTraceSafe(traceID)
}

func (c *Consumer) processTraceSafe(traceID string) {
	defer func() {
		if r := recover(); r != nil {
			_ = c.store.UpdateContractTraceStatus(traceID, "failed")
		}
	}()
	c.processTrace(traceID)
}

// SetReleaseQueueSlot sets the callback to release untrusted queue slots.
func (c *Consumer) SetReleaseQueueSlot(fn func(keyID string, trusted bool)) {
	c.releaseQueueSlot = fn
}

func (c *Consumer) processTrace(traceID string) {
	trace, err := c.store.GetContractTrace(traceID)
	if err != nil || trace.Status != "queued" {
		return
	}
	defer func() {
		if c.releaseQueueSlot != nil {
			c.releaseQueueSlot(trace.KeyID, trace.Trusted)
		}
	}()

	// 1. Golden trace handling
	isGolden := trace.Tool != "" && (trace.Tool == "claude" || trace.Tool == "codex") && trace.SwitcherVersion == ""
	if isGolden {
		c.processGoldenTrace(trace)
		_ = c.store.UpdateContractTraceStatus(traceID, "done")
		return
	}

	// 2. Untrusted traces
	trusted := trace.Trusted && (trace.KeyID == "" || c.store.IsKeyTrusted(trace.KeyID))
	if !trusted && trace.Trusted {
		_ = c.store.UpdateContractTraceTrusted(traceID, false)
	}
	if !trusted {
		// Trust over time: untrusted trace creates no learned row, no judge call and no finding
		_ = c.store.UpdateContractTraceStatus(traceID, "done")
		return
	}

	// 3. Trusted traced capture
	shapes, err := c.store.ListContractShapesForTrace(traceID)
	if err != nil {
		_ = c.store.UpdateContractTraceStatus(traceID, "failed")
		return
	}

	var intactReq, intactResp, switcherReq, switcherResp *ShapeRecord
	for _, s := range shapes {
		var rec ShapeRecord
		if json.Unmarshal([]byte(s.Record), &rec) != nil {
			continue
		}
		if s.Half == "intact" {
			if s.Direction == "request" {
				intactReq = &rec
			} else if s.Direction == "response" {
				intactResp = &rec
			}
		} else if s.Half == "switcher" {
			if s.Direction == "request" {
				switcherReq = &rec
			} else if s.Direction == "response" {
				switcherResp = &rec
			}
		}
	}

	var allCandidates []Candidate
	var allUnmatchedOut []string

	reqMappings, reqNoise := c.getApprovedMappingsAndNoise(trace.Model, "request")
	respMappings, respNoise := c.getApprovedMappingsAndNoise(trace.Model, "response")

	// Request direction diff: in = switcherReq (toolRequest), out = intactReq (intact received)
	if switcherReq != nil && intactReq != nil {
		cands, unOut := Diff(*switcherReq, *intactReq, reqMappings, reqNoise, trace.Model, trace.ClientFormat, "request")
		allCandidates = append(allCandidates, cands...)
		allUnmatchedOut = append(allUnmatchedOut, unOut...)
		c.updateCleanTraces(trace.Model, "request", *intactReq)
	}

	// Response direction diff: in = intactResp (upstream answer), out = switcherResp (toolResponse)
	if intactResp != nil && switcherResp != nil {
		cands, unOut := Diff(*intactResp, *switcherResp, respMappings, respNoise, trace.Model, trace.ClientFormat, "response")
		allCandidates = append(allCandidates, cands...)
		allUnmatchedOut = append(allUnmatchedOut, unOut...)
		c.updateCleanTraces(trace.Model, "response", *switcherResp)
	}

	// Judge candidates
	if len(allCandidates) > 0 && c.judgeMgr != nil {
		allApprovedMappings := append(reqMappings, respMappings...)
		_ = c.judgeMgr.ProcessCandidates(context.Background(), trace.KeyID, trace.ID, allCandidates, allUnmatchedOut, allApprovedMappings, nil)
	}

	// Learning and Change Detection on response direction
	if intactResp != nil {
		c.learnAndDetectChanges(trace, *intactResp, "response")
	}

	_ = c.store.UpdateContractTraceStatus(traceID, "done")
}

func (c *Consumer) processGoldenTrace(trace store.ContractTrace) {
	shapes, err := c.store.ListContractShapesForTrace(trace.ID)
	if err != nil {
		return
	}
	for _, s := range shapes {
		if s.Half != "intact" {
			continue
		}
		var rec ShapeRecord
		if json.Unmarshal([]byte(s.Record), &rec) != nil {
			continue
		}
		c.learnAndDetectChanges(trace, rec, s.Direction)
	}
}

func (c *Consumer) getApprovedMappingsAndNoise(model, direction string) ([]Mapping, []string) {
	var mappings []Mapping
	var noise []string

	// 1. From contract_signatures
	rows, err := c.store.DB.Query(`SELECT path, verdict FROM contract_signatures
		WHERE model = ? AND direction = ? AND verdict_state = 'approved'`, model, direction)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var p, v string
			if rows.Scan(&p, &v) == nil {
				if strings.HasPrefix(v, "renamed:") {
					outPath := strings.TrimPrefix(v, "renamed:")
					mappings = append(mappings, Mapping{InPath: p, OutPath: outPath})
				} else if v == "noise" {
					noise = append(noise, p)
				}
			}
		}
	}

	// 2. From contract_findings with mapping
	fRows, err := c.store.DB.Query(`SELECT path, mapping FROM contract_findings
		WHERE model = ? AND direction = ? AND mapping != ''`, model, direction)
	if err == nil {
		defer fRows.Close()
		for fRows.Next() {
			var p, m string
			if fRows.Scan(&p, &m) == nil && m != "" {
				mappings = append(mappings, Mapping{InPath: p, OutPath: m})
			}
		}
	}

	return mappings, noise
}

func (c *Consumer) updateCleanTraces(model, direction string, out ShapeRecord) {
	// A clean trace for a finding is a trusted trace whose diff ran on finding's direction and matched path
	findings, err := c.store.ListContractFindings("open", 0, true)
	if err != nil {
		return
	}
	outPaths := make(map[string]bool)
	for _, rec := range out.Records {
		for _, leaf := range rec.Leaves {
			outPaths[leaf.Path] = true
		}
	}

	for _, f := range findings {
		if f.Model == model && f.Direction == direction && outPaths[f.Path] {
			_ = c.store.IncrementFindingCleanTraces(f.ID)
		}
	}
}

func hasCutLeaf(rec ShapeRecord) bool {
	for _, r := range rec.Records {
		for _, leaf := range r.Leaves {
			if leaf.Type == "cut" || leaf.Path == "{cut}" || strings.HasSuffix(leaf.Path, "{cut}") {
				return true
			}
		}
	}
	return false
}

func (c *Consumer) learnAndDetectChanges(trace store.ContractTrace, rec ShapeRecord, direction string) {
	if hasCutLeaf(rec) {
		return
	}

	kind := "model"
	subject := trace.Model
	isGolden := trace.Tool != "" && (trace.Tool == "claude" || trace.Tool == "codex") && trace.SwitcherVersion == ""
	if isGolden {
		kind = "tool"
		subject = trace.Tool
	}
	if subject == "" {
		return
	}

	now := time.Now().UnixMilli()
	mdKey := kind + ":" + subject + ":" + direction

	c.mu.Lock()
	defer c.mu.Unlock()

	knownPaths := c.learnedPaths[mdKey]
	hadKnownPaths := false
	if knownPaths == nil {
		knownPaths = make(map[string]bool)
		if rows, err := c.store.ListContractLearned(kind, subject); err == nil {
			for _, row := range rows {
				if row.Direction == direction {
					knownPaths[row.Path] = true
				}
			}
		}
		if len(knownPaths) > 0 {
			hadKnownPaths = true
		}
		c.learnedPaths[mdKey] = knownPaths
	} else {
		hadKnownPaths = len(knownPaths) > 0
	}

	goneMap := c.pathGoneCounts[mdKey]
	if goneMap == nil {
		goneMap = make(map[string]int)
		c.pathGoneCounts[mdKey] = goneMap
	}

	fmtEvents := c.knownEvents[trace.UpstreamFormat]
	hadEvents := false
	if fmtEvents == nil {
		fmtEvents = make(map[string]bool)
		c.knownEvents[trace.UpstreamFormat] = fmtEvents
	} else {
		hadEvents = len(fmtEvents) > 0
	}

	seenThisTrace := make(map[string]bool)
	changesDetected := 0

	for _, r := range rec.Records {
		// Stream event change detection
		if r.Event != "" {
			if hadEvents && !fmtEvents[r.Event] {
				changesDetected++
			}
			fmtEvents[r.Event] = true
		}

		for _, leaf := range r.Leaves {
			if leaf.Type == "cut" || leaf.Type == "invalid" {
				continue
			}
			seenThisTrace[leaf.Path] = true

			// Check new path
			if hadKnownPaths && !knownPaths[leaf.Path] {
				changesDetected++
			}
			knownPaths[leaf.Path] = true
			goneMap[leaf.Path] = 0 // Reset gone count

			// Write contract_learned
			_ = c.store.UpsertContractLearned(store.ContractLearned{
				Kind:           kind,
				Subject:        subject,
				Direction:      direction,
				Half:           "intact",
				Format:         trace.UpstreamFormat,
				Event:          r.Event,
				Path:           leaf.Path,
				Type:           leaf.Type,
				Seen:           1,
				FirstSeen:      now,
				LastSeen:       now,
				Gone:           0,
				ReducerVersion: ReducerVersion,
				KeyID:          trace.KeyID,
				FirstTraceID:   trace.ID,
			})
		}
	}

	// Check paths gone in 20 records in a row
	if hadKnownPaths {
		for p := range knownPaths {
			if !seenThisTrace[p] {
				goneMap[p]++
				if goneMap[p] == 20 {
					changesDetected++
				}
			}
		}
	}

	// Tool version check
	if trace.Tool != "" && trace.ToolVersion != "" {
		toolVers := c.knownVersions[trace.Tool]
		hadVersions := false
		if toolVers == nil {
			toolVers = make(map[string]bool)
			c.knownVersions[trace.Tool] = toolVers
		} else {
			hadVersions = len(toolVers) > 0
		}
		if hadVersions && !toolVers[trace.ToolVersion] {
			changesDetected++
			c.changedTools[trace.Tool] = true
		}
		toolVers[trace.ToolVersion] = true
	}

	if changesDetected > 0 {
		if kind == "model" {
			c.changedModels[trace.Model] = 20
		}
		// Check 6-hour alert throttle
		alertKey := subject
		lastAlert := c.lastAlertAt[alertKey]
		if time.Since(lastAlert) >= 6*time.Hour {
			c.lastAlertAt[alertKey] = time.Now()
			if c.alertFn != nil {
				alertText := FormatChangeAlert(changesDetected, "")
				c.alertFn(trace.Model, trace.Tool, alertText)
			}
		}
	} else {
		if kind == "model" && c.changedModels[trace.Model] > 0 {
			c.changedModels[trace.Model]--
		}
	}
}

// PolicyInfo holds policy rates and change counts.
type PolicyInfo struct {
	Models        map[string]float64 `json:"models"`
	Default       float64            `json:"default"`
	ChangedModels int                `json:"changedModels"`
	ChangedTools  int                `json:"changedTools"`
}

// GetPolicy calculates the current sampling policy.
func (c *Consumer) GetPolicy() PolicyInfo {
	c.mu.Lock()
	defer c.mu.Unlock()

	info := PolicyInfo{
		Models:        make(map[string]float64),
		Default:       0.02,
		ChangedModels: 0,
		ChangedTools:  len(c.changedTools),
	}

	for m, reqs := range c.changedModels {
		if reqs > 0 {
			info.Models[m] = 1.0
			info.ChangedModels++
		}
	}

	// Check models with open lost findings with < 10 clean traces
	findings, err := c.store.ListContractFindings("open", 0, true)
	if err == nil {
		for _, f := range findings {
			if f.Class == "lost" && f.CleanTraces < 10 {
				info.Models[f.Model] = 1.0
			}
		}
	}

	return info
}
