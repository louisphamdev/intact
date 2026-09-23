package contract

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// Limits on capture slots and body sizes.
const (
	MaxHalfBytes             = 4 * 1024 * 1024 // 4 MiB
	MaxTracedSlots           = 16
	MaxUntrustedSlotsPerKey  = 2
	MaxUntrustedQueuePerKey  = 8
	MaxGoldenSlots           = 4
	MaxGoldenSamplesPerDay   = 3
	MaxGoldenVersionsPerTool = 5
)

// ErrPayloadTooLarge is returned when uncompressed payload exceeds 4 MiB.
var ErrPayloadTooLarge = errors.New("payload too large (exceeds 4 MiB)")

var (
	traceIDRegex         = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
	modelIDRegex         = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{1,128}$`)
	toolVersionRegex     = regexp.MustCompile(`^\d{1,5}\.\d{1,5}\.\d{1,5}([-+][0-9A-Za-z.]{1,32})?$`)
	switcherVersionRegex = regexp.MustCompile(`^(\d{1,5}\.\d{1,5}\.\d{1,5})\+(\d{8}T\d{6}Z)$`)

	knownFormats = map[string]bool{
		"openai":       true,
		"openai-chat":  true,
		"anthropic":    true,
		"responses":    true,
		"responses-ws": true,
		"vertex":       true,
		"auto":         true,
		"antigravity":  true,
		"gemini":       true,
	}
)

// IsValidTraceID checks if traceID matches ^[A-Za-z0-9_-]{16,64}$.
func IsValidTraceID(id string) bool {
	return traceIDRegex.MatchString(id)
}

// IsValidModelID checks if model matches ^[A-Za-z0-9._:/@+-]{1,128}$.
func IsValidModelID(model string) bool {
	return modelIDRegex.MatchString(model)
}

// IsValidFormat checks if format is a known format name.
func IsValidFormat(fmt string) bool {
	return knownFormats[fmt]
}

// ConverterFormat describes the conversion formats.
type ConverterFormat struct {
	InFormat  string `json:"inFormat"`
	OutFormat string `json:"outFormat"`
}

// HalfPayload is the uploaded half from llm-switcher.
type HalfPayload struct {
	ToolRequest     string          `json:"toolRequest"`
	ToolResponse    string          `json:"toolResponse"`
	ToolVersion     string          `json:"toolVersion"`
	SwitcherVersion string          `json:"switcherVersion"`
	Converter       ConverterFormat `json:"converter"`
}

// Capture manages a single active capture.
type Capture struct {
	TraceID        string
	KeyID          string
	Trusted        bool
	Golden         bool
	SampleKey      string
	Model          string
	Provider       string
	Endpoint       string
	Tool           string
	ToolVersion    string
	ClientFormat   string
	UpstreamFormat string
	Status         string
	Truncated      bool
	ReqTruncated   bool
	RespTruncated  bool
	Aborted        bool

	ReqBuf       *bytes.Buffer
	RespBuf      *bytes.Buffer
	ProducerDone chan struct{}
	HeldHalf     *HalfPayload

	mu     sync.Mutex
	sealed bool
	mgr    *CaptureManager
}

// CaptureManager coordinates all active captures and enforces slots.
type CaptureManager struct {
	store *store.Store
	key   []byte

	mu                  sync.Mutex
	tracedSlots         int
	trustedSlots        int
	untrustedTotalSlots int
	untrustedSlots      map[string]int
	untrustedQueue      map[string]int
	goldenSlots         int
	goldenSamples       map[string]int
	goldenVersions      map[string]map[string]bool
	activeCaptures      map[string]*Capture

	enqueueFunc func(traceID string)
}

// NewCaptureManager initializes a capture manager.
func NewCaptureManager(s *store.Store, key []byte) *CaptureManager {
	return &CaptureManager{
		store:          s,
		key:            key,
		untrustedSlots: make(map[string]int),
		untrustedQueue: make(map[string]int),
		goldenSamples:  make(map[string]int),
		goldenVersions: make(map[string]map[string]bool),
		activeCaptures: make(map[string]*Capture),
	}
}

// SetEnqueue sets the callback to enqueue traces into the consumer queue.
func (m *CaptureManager) SetEnqueue(fn func(traceID string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enqueueFunc = fn
}

// ClaimTraced attempts to claim a slot for an inbound X-Intact-Trace.
func (m *CaptureManager) ClaimTraced(traceID, keyID string, trusted bool, model, provider, endpoint string) (*Capture, bool) {
	if !traceIDRegex.MatchString(traceID) || !modelIDRegex.MatchString(model) {
		return nil, false
	}

	// Reused trace id is not captured.
	if existing, err := m.store.GetContractTrace(traceID); err == nil && existing.ID != "" {
		return nil, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.activeCaptures[traceID]; exists {
		return nil, false
	}

	if !trusted {
		if m.untrustedTotalSlots >= MaxTracedSlots {
			return nil, false
		}
		if m.untrustedSlots[keyID] >= MaxUntrustedSlotsPerKey {
			return nil, false
		}
	} else {
		if m.trustedSlots >= MaxTracedSlots {
			return nil, false
		}
	}

	m.tracedSlots++
	if !trusted {
		m.untrustedTotalSlots++
		m.untrustedSlots[keyID]++
	} else {
		m.trustedSlots++
	}

	cap := &Capture{
		TraceID:      traceID,
		KeyID:        keyID,
		Trusted:      trusted,
		Golden:       false,
		Model:        model,
		Provider:     provider,
		Endpoint:     endpoint,
		Status:       "capturing",
		ReqBuf:       new(bytes.Buffer),
		RespBuf:      new(bytes.Buffer),
		ProducerDone: make(chan struct{}),
		mgr:          m,
	}
	m.activeCaptures[traceID] = cap

	_ = m.store.InsertContractTrace(store.ContractTrace{
		ID:             traceID,
		KeyID:          keyID,
		Trusted:        trusted,
		Created:        time.Now().UnixMilli(),
		Provider:       provider,
		Model:          model,
		Status:         "capturing",
		ReducerVersion: ReducerVersion,
	})

	return cap, true
}

// ClaimGolden attempts to claim a golden slot for native CLI traffic.
func (m *CaptureManager) ClaimGolden(keyID string, trusted bool, provider, userAgent, endpoint string) (*Capture, bool) {
	if !trusted {
		return nil, false
	}
	var tool, rest string
	switch {
	case strings.HasPrefix(userAgent, "claude-cli/"):
		tool = "claude"
		rest = strings.TrimPrefix(userAgent, "claude-cli/")
	case strings.HasPrefix(userAgent, "codex_cli_rs/"):
		tool = "codex"
		rest = strings.TrimPrefix(userAgent, "codex_cli_rs/")
	default:
		return nil, false
	}

	idx := strings.IndexAny(rest, " \t\r\n")
	toolVer := rest
	if idx > 0 {
		toolVer = rest[:idx]
	}
	if !toolVersionRegex.MatchString(toolVer) {
		return nil, false
	}

	today := time.Now().UTC().Format("2006-01-02")
	sampleKey := fmt.Sprintf("%s:%s:%s:%s", provider, endpoint, toolVer, today)
	toolDayKey := fmt.Sprintf("%s:%s", tool, today)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.goldenSlots >= MaxGoldenSlots {
		return nil, false
	}

	if m.goldenSamples[sampleKey] >= MaxGoldenSamplesPerDay {
		return nil, false
	}

	vers := m.goldenVersions[toolDayKey]
	if vers == nil {
		vers = make(map[string]bool)
		m.goldenVersions[toolDayKey] = vers
	}
	if !vers[toolVer] && len(vers) >= MaxGoldenVersionsPerTool {
		return nil, false
	}

	m.goldenSlots++
	m.goldenSamples[sampleKey]++
	vers[toolVer] = true

	b := make([]byte, 16)
	_, _ = rand.Read(b)
	traceID := "golden_" + hex.EncodeToString(b)

	cap := &Capture{
		TraceID:      traceID,
		KeyID:        keyID,
		Trusted:      true,
		Golden:       true,
		SampleKey:    sampleKey,
		Tool:         tool,
		ToolVersion:  toolVer,
		Provider:     provider,
		Endpoint:     endpoint,
		Status:       "capturing",
		ReqBuf:       new(bytes.Buffer),
		RespBuf:      new(bytes.Buffer),
		ProducerDone: make(chan struct{}),
		mgr:          m,
	}
	m.activeCaptures[traceID] = cap

	_ = m.store.InsertContractTrace(store.ContractTrace{
		ID:             traceID,
		KeyID:          keyID,
		Trusted:        true,
		Created:        time.Now().UnixMilli(),
		Provider:       provider,
		Tool:           tool,
		ToolVersion:    toolVer,
		Status:         "capturing",
		ReducerVersion: ReducerVersion,
	})

	return cap, true
}

// WriteReq appends request bytes up to 4 MiB.
func (c *Capture) WriteReq(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ReqBuf == nil {
		return
	}
	if c.ReqBuf.Len()+len(p) > MaxHalfBytes {
		c.ReqTruncated = true
		c.Truncated = true
		rem := MaxHalfBytes - c.ReqBuf.Len()
		if rem > 0 {
			c.ReqBuf.Write(p[:rem])
		}
		return
	}
	c.ReqBuf.Write(p)
}

// WriteResp appends upstream response bytes up to 4 MiB.
func (c *Capture) WriteResp(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.RespBuf == nil {
		return
	}
	if c.RespBuf.Len()+len(p) > MaxHalfBytes {
		c.RespTruncated = true
		c.Truncated = true
		rem := MaxHalfBytes - c.RespBuf.Len()
		if rem > 0 {
			c.RespBuf.Write(p[:rem])
		}
		return
	}
	c.RespBuf.Write(p)
}

func (m *CaptureManager) releaseCaptureSlots(c *Capture, hasErr bool) {
	if c.Golden {
		m.goldenSlots--
		if hasErr {
			m.goldenSamples[c.SampleKey]--
		}
	} else {
		m.tracedSlots--
		if !c.Trusted {
			m.untrustedTotalSlots--
			m.untrustedSlots[c.KeyID]--
		} else {
			m.trustedSlots--
		}
	}
	delete(m.activeCaptures, c.TraceID)
}

// Seal finalizes the capture after response writing is complete.
func (c *Capture) Seal(isSSE bool, clientErr, upstreamErr error) error {
	c.mu.Lock()
	if c.sealed {
		c.mu.Unlock()
		return nil
	}
	c.sealed = true
	c.mu.Unlock()

	if clientErr == nil && upstreamErr == nil {
		select {
		case <-c.ProducerDone:
		case <-time.After(5 * time.Second):
		}
	}

	c.mu.Lock()
	var reqBytes, respBytes []byte
	if c.ReqBuf != nil {
		reqBytes = c.ReqBuf.Bytes()
		c.ReqBuf = nil
	}
	if c.RespBuf != nil {
		respBytes = c.RespBuf.Bytes()
		c.RespBuf = nil
	}
	reqTruncated := c.ReqTruncated
	respTruncated := c.RespTruncated
	c.mu.Unlock()

	if clientErr != nil || upstreamErr != nil {
		c.mu.Lock()
		c.Aborted = true
		c.Status = "aborted"
		c.mu.Unlock()

		c.mgr.mu.Lock()
		c.mgr.releaseCaptureSlots(c, true)
		c.mgr.mu.Unlock()

		_ = c.mgr.store.UpdateContractTraceStatus(c.TraceID, "aborted")
		return nil
	}

	reqShape := Reduce(reqBytes, false, c.mgr.key)
	respShape := Reduce(respBytes, isSSE, c.mgr.key)
	if reqTruncated {
		if len(reqShape.Records) == 0 {
			reqShape.Records = append(reqShape.Records, Record{Leaves: []Leaf{{Path: "{cut}", Type: "cut"}}})
		} else {
			last := len(reqShape.Records) - 1
			reqShape.Records[last].Leaves = append(reqShape.Records[last].Leaves, Leaf{Path: "{cut}", Type: "cut"})
		}
	}
	if respTruncated {
		if len(respShape.Records) == 0 {
			respShape.Records = append(respShape.Records, Record{Leaves: []Leaf{{Path: "{cut}", Type: "cut"}}})
		} else {
			last := len(respShape.Records) - 1
			respShape.Records[last].Leaves = append(respShape.Records[last].Leaves, Leaf{Path: "{cut}", Type: "cut"})
		}
	}

	reqJSON, _ := reqShape.MarshalJSON()
	respJSON, _ := respShape.MarshalJSON()

	targetStatus := "open"
	if c.Golden {
		targetStatus = "queued"
	}

	now := time.Now().UnixMilli()
	ok, err := c.mgr.store.CommitIntactShapesAndSeal(c.TraceID, targetStatus, string(reqJSON), string(respJSON), now)
	if err != nil || !ok {
		c.mu.Lock()
		c.Status = "failed"
		c.mu.Unlock()

		c.mgr.mu.Lock()
		c.mgr.releaseCaptureSlots(c, false)
		c.mgr.mu.Unlock()

		_ = c.mgr.store.UpdateContractTraceStatus(c.TraceID, "failed")
		return err
	}

	c.mu.Lock()
	c.Status = targetStatus
	heldHalf := c.HeldHalf
	c.HeldHalf = nil
	c.mu.Unlock()

	c.mgr.mu.Lock()
	c.mgr.releaseCaptureSlots(c, false)
	c.mgr.mu.Unlock()

	if c.Golden {
		if c.mgr.enqueueFunc != nil {
			c.mgr.enqueueFunc(c.TraceID)
		}
		return nil
	}

	// 30 minute expiry timer for non-golden
	time.AfterFunc(30*time.Minute, func() {
		_, _ = c.mgr.store.UpdateContractTraceStatusCAS(c.TraceID, "open", "expired")
	})

	if heldHalf != nil {
		if ok, _ := c.mgr.store.UpdateContractTraceStatusCAS(c.TraceID, "open", "reducing"); ok {
			trusted := c.Trusted && (c.KeyID == "" || c.mgr.store.IsKeyTrusted(c.KeyID))
			if !trusted && c.Trusted {
				_ = c.mgr.store.UpdateContractTraceTrusted(c.TraceID, false)
			}
			c.mgr.processSwitcherHalf(c.TraceID, c.KeyID, trusted, heldHalf)
		}
	}

	return nil
}

// ReleaseOnPanic cleans up slots and marks trace as failed on an unhandled panic.
func (c *Capture) ReleaseOnPanic() {
	c.mu.Lock()
	if c.sealed {
		c.mu.Unlock()
		return
	}
	c.sealed = true
	c.Aborted = true
	c.ReqBuf = nil
	c.RespBuf = nil
	c.HeldHalf = nil
	c.mu.Unlock()

	c.mgr.mu.Lock()
	c.mgr.releaseCaptureSlots(c, true)
	c.mgr.mu.Unlock()

	_ = c.mgr.store.UpdateContractTraceStatus(c.TraceID, "failed")
}

// ReleaseOnAbort aborts the capture and releases its slot.
func (c *Capture) ReleaseOnAbort() {
	_ = c.Seal(false, nil, errors.New("aborted"))
}

// SubmitHalf handles an upload from llm-switcher.
func (m *CaptureManager) SubmitHalf(traceID, callerKeyID string, isCallerTrusted bool, half *HalfPayload) (int, error) {
	trace, err := m.store.GetContractTrace(traceID)
	if err != nil || trace.ID == "" {
		return http.StatusNotFound, errors.New("trace not found")
	}

	if trace.KeyID != callerKeyID || (trace.KeyID == "" && !isCallerTrusted) {
		return http.StatusNotFound, errors.New("not opener")
	}

	// Check if half is already stored.
	shapes, _ := m.store.ListContractShapesForTrace(traceID)
	for _, sh := range shapes {
		if sh.Half == "switcher" {
			return http.StatusConflict, errors.New("half already stored")
		}
	}

	m.mu.Lock()
	activeCap, isActive := m.activeCaptures[traceID]
	m.mu.Unlock()

	if isActive && activeCap != nil {
		activeCap.mu.Lock()
		if activeCap.HeldHalf != nil {
			activeCap.mu.Unlock()
			return http.StatusConflict, errors.New("half already held")
		}
		if activeCap.Status == "capturing" {
			activeCap.HeldHalf = half
			activeCap.mu.Unlock()
			return http.StatusAccepted, nil
		}
		activeCap.mu.Unlock()
	}

	cas, _ := m.store.UpdateContractTraceStatusCAS(traceID, "open", "reducing")
	if !cas {
		return http.StatusGone, errors.New("trace not open")
	}

	trusted := trace.Trusted && (trace.KeyID == "" || m.store.IsKeyTrusted(trace.KeyID))
	if !trusted && trace.Trusted {
		_ = m.store.UpdateContractTraceTrusted(traceID, false)
	}

	m.processSwitcherHalf(traceID, callerKeyID, trusted, half)
	return http.StatusOK, nil
}

func (m *CaptureManager) processSwitcherHalf(traceID, keyID string, trusted bool, half *HalfPayload) {
	_ = m.store.UpdateContractTraceVersions(traceID, half.ToolVersion, half.SwitcherVersion, half.Converter.InFormat, half.Converter.OutFormat)

	if half.ToolRequest != "" {
		sh := Reduce([]byte(half.ToolRequest), false, m.key)
		b, _ := sh.MarshalJSON()
		_ = m.store.InsertContractShape(traceID, "switcher", "request", string(b), time.Now().UnixMilli())
	}
	if half.ToolResponse != "" {
		isSSE := strings.Contains(half.ToolResponse, "event:") || strings.Contains(half.ToolResponse, "data:")
		sh := Reduce([]byte(half.ToolResponse), isSSE, m.key)
		b, _ := sh.MarshalJSON()
		_ = m.store.InsertContractShape(traceID, "switcher", "response", string(b), time.Now().UnixMilli())
	}

	if !trusted {
		m.mu.Lock()
		if m.untrustedQueue[keyID] >= MaxUntrustedQueuePerKey {
			m.mu.Unlock()
			_ = m.store.UpdateContractTraceStatus(traceID, "dropped")
			return
		}
		m.untrustedQueue[keyID]++
		m.mu.Unlock()
	}

	ok, _ := m.store.UpdateContractTraceStatusCAS(traceID, "reducing", "queued")
	if ok && m.enqueueFunc != nil {
		m.enqueueFunc(traceID)
	}
}

// Slots returns slot counters for inspection.
func (m *CaptureManager) Slots(keyID string) (int, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.goldenSlots, m.tracedSlots, m.untrustedSlots[keyID]
}

// QueueCount returns the untrusted queue count for a key.
func (m *CaptureManager) QueueCount(keyID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.untrustedQueue[keyID]
}

// ReleaseQueueSlot decrements untrusted queue counters when consumer finishes a trace.
func (m *CaptureManager) ReleaseQueueSlot(keyID string, trusted bool) {
	if !trusted {
		m.mu.Lock()
		if m.untrustedQueue[keyID] > 0 {
			m.untrustedQueue[keyID]--
		}
		m.mu.Unlock()
	}
}

// DecompressHalf decompresses data if gzipped and validates size bound.
func DecompressHalf(data []byte, isGzip bool) ([]byte, error) {
	if isGzip || (len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b) {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer zr.Close()

		lr := io.LimitReader(zr, MaxHalfBytes+1)
		b, err := io.ReadAll(lr)
		if err != nil {
			return nil, err
		}
		if len(b) > MaxHalfBytes {
			return nil, ErrPayloadTooLarge
		}
		return b, nil
	}

	if len(data) > MaxHalfBytes {
		return nil, ErrPayloadTooLarge
	}
	return data, nil
}

// ValidateSwitcherVersion checks switcherVersion regex, UTC timestamp validity, and ensures date is not > 1 day in the future.
func ValidateSwitcherVersion(v string, now time.Time) error {
	m := switcherVersionRegex.FindStringSubmatch(v)
	if m == nil {
		return errors.New("invalid switcherVersion format")
	}
	t, err := time.Parse("20060102T150405Z", m[2])
	if err != nil {
		return fmt.Errorf("invalid time in switcherVersion: %w", err)
	}
	if t.After(now.Add(24 * time.Hour)) {
		return errors.New("switcherVersion time is more than 1 day in the future")
	}
	return nil
}

// ValidateToolVersion checks tool version regex if provided.
func ValidateToolVersion(v string) error {
	if v == "" {
		return nil
	}
	if !toolVersionRegex.MatchString(v) {
		return errors.New("invalid toolVersion format")
	}
	return nil
}
