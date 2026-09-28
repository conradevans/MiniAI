package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	historyRequestMaxRunes      = 8000
	historyResultMaxRunes       = 16000
	historyPreviewMaxRunes      = 240
	historySummaryMaxRunes      = 500
	historyArgumentMaxRunes     = 500
	historyMaximumTargets       = 16
	historyMaximumEvidenceReads = 32
	historyMaximumLinks         = 64
	historyArgumentsMaxBytes    = 4096
)

type HistoryKind string

const (
	HistoryKindConversation  HistoryKind = "conversation"
	HistoryKindInvestigation HistoryKind = "investigation"
	HistoryKindAction        HistoryKind = "action"
	HistoryKindCodeChange    HistoryKind = "code_change"
	HistoryKindPreview       HistoryKind = "preview"
)

type HistoryStatus string

const (
	HistoryStatusRunning     HistoryStatus = "running"
	HistoryStatusSucceeded   HistoryStatus = "succeeded"
	HistoryStatusFailed      HistoryStatus = "failed"
	HistoryStatusCanceled    HistoryStatus = "canceled"
	HistoryStatusInterrupted HistoryStatus = "interrupted"
)

type HistoryExecutionMetadata struct {
	AnswerMode           string  `json:"answer_mode,omitempty"`
	Route                string  `json:"route,omitempty"`
	Model                string  `json:"model,omitempty"`
	ModelMode            string  `json:"model_mode,omitempty"`
	InferenceProfile     string  `json:"inference_profile,omitempty"`
	Confidence           string  `json:"confidence,omitempty"`
	ModelInvoked         bool    `json:"model_invoked"`
	PlannerCalls         int     `json:"planner_calls"`
	ReasonerCalls        int     `json:"reasoner_calls"`
	EvidenceRounds       int     `json:"evidence_rounds"`
	SecondRoundReads     int     `json:"second_round_reads"`
	ToolCalls            int     `json:"tool_calls"`
	EvidencePacketBytes  int     `json:"evidence_packet_bytes"`
	AnswerValidation     string  `json:"answer_validation,omitempty"`
	PromptTokens         int     `json:"prompt_tokens"`
	GeneratedTokens      int     `json:"generated_tokens"`
	LoadSeconds          float64 `json:"load_seconds"`
	PromptSeconds        float64 `json:"prompt_seconds"`
	TotalSeconds         float64 `json:"total_seconds"`
	InvestigationSeconds float64 `json:"investigation_seconds"`
	ErrorCategory        string  `json:"error_category,omitempty"`
}

type HistoryEntry struct {
	ID                 string        `json:"id"`
	Kind               HistoryKind   `json:"kind"`
	Status             HistoryStatus `json:"status"`
	ChatID             string        `json:"chat_id,omitempty"`
	UserMessageID      int64         `json:"user_message_id,omitempty"`
	AssistantMessageID int64         `json:"assistant_message_id,omitempty"`
	RequestText        string        `json:"request_text"`
	RequestRedacted    bool          `json:"request_redacted"`
	RequestTruncated   bool          `json:"request_truncated"`
	ResultText         string        `json:"result_text"`
	ResultRedacted     bool          `json:"result_redacted"`
	ResultTruncated    bool          `json:"result_truncated"`
	StartedAt          time.Time     `json:"started_at"`
	CompletedAt        *time.Time    `json:"completed_at,omitempty"`
	HistoryExecutionMetadata
	Targets        []HistoryTarget       `json:"targets"`
	EvidenceReads  []HistoryEvidenceRead `json:"evidence_reads"`
	Links          []HistoryLink         `json:"links"`
	LinksTruncated bool                  `json:"links_truncated"`
}

type HistoryTarget struct {
	Ordinal     int    `json:"ordinal"`
	Kind        string `json:"kind"`
	CanonicalID string `json:"canonical_id"`
	DisplayName string `json:"display_name,omitempty"`
}

type HistoryEvidenceRead struct {
	ID            int64          `json:"id,omitempty"`
	RequestID     string         `json:"request_id,omitempty"`
	EvidenceRound int            `json:"evidence_round"`
	Order         int            `json:"order"`
	Capability    string         `json:"capability"`
	Arguments     map[string]any `json:"arguments,omitempty"`
	SafeSummary   string         `json:"safe_summary,omitempty"`
	Status        string         `json:"status,omitempty"`
	Availability  string         `json:"availability,omitempty"`
	StartedAt     *time.Time     `json:"started_at,omitempty"`
	CompletedAt   *time.Time     `json:"completed_at,omitempty"`
}

type HistoryLink struct {
	FromEntryID string    `json:"from_entry_id"`
	ToEntryID   string    `json:"to_entry_id"`
	Relation    string    `json:"relation"`
	CreatedAt   time.Time `json:"created_at"`
}

type historyFinalizeInput struct {
	Kind              HistoryKind
	Status            HistoryStatus
	ResultText        string
	AssistantText     string
	PersistAssistant  bool
	AssistantEvidence []evidence
	CompletedAt       time.Time
	Metadata          HistoryExecutionMetadata
	Targets           []HistoryTarget
	EvidenceReads     []HistoryEvidenceRead
}

var (
	thinkBlockPattern              = regexp.MustCompile(`(?is)<think>.*?</think>|<think>.*$`)
	historyStandaloneBearerPattern = regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+\-/=]+`)
	historyPrivateKeyBlockPattern  = regexp.MustCompile(`(?is)-----BEGIN [^-\r\n]*PRIVATE KEY-----.*?-----END [^-\r\n]*PRIVATE KEY-----`)
)

func sanitizeHistoryText(value string, maximum int) string {
	value, _, _ = sanitizeHistoryAuditText(value, maximum)
	return value
}

func sanitizeHistoryAuditText(value string, maximum int) (string, bool, bool) {
	redacted := false
	apply := func(pattern *regexp.Regexp, replacement string) {
		next := pattern.ReplaceAllString(value, replacement)
		if next != value {
			redacted = true
			value = next
		}
	}
	apply(thinkBlockPattern, "")
	if scrubbed, sharedRedacted := scrubSensitiveText(value); sharedRedacted {
		value, redacted = scrubbed, true
	} else {
		value = scrubbed
	}
	apply(historyStandaloneBearerPattern, `${1}[REDACTED]`)
	apply(historyPrivateKeyBlockPattern, `[REDACTED PRIVATE KEY]`)
	value = strings.TrimSpace(value)
	truncated := len([]rune(value)) > maximum
	return truncateHistoryRunes(value, maximum), redacted, truncated
}

func truncateHistoryRunes(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	if maximum == 1 {
		return "…"
	}
	return string(runes[:maximum-1]) + "…"
}

func sanitizeHistoryArguments(arguments map[string]any) map[string]any {
	allowed := map[string]bool{
		"app": true, "path": true, "query": true, "lines": true, "range": true,
		"database_id": true, "service": true, "limit": true, "from": true, "to": true,
	}
	keys := make([]string, 0, len(arguments))
	for key := range arguments {
		if allowed[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := map[string]any{}
	for _, key := range keys {
		switch value := arguments[key].(type) {
		case string:
			out[key] = sanitizeHistoryText(value, historyArgumentMaxRunes)
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number, bool:
			out[key] = value
		}
		if len(canonicalJSON(out)) > historyArgumentsMaxBytes {
			delete(out, key)
			break
		}
	}
	return out
}

func normalizeHistoryKind(kind HistoryKind) HistoryKind {
	switch kind {
	case HistoryKindConversation, HistoryKindInvestigation, HistoryKindAction, HistoryKindCodeChange, HistoryKindPreview:
		return kind
	default:
		return HistoryKindConversation
	}
}

func validHistoryStatus(status HistoryStatus) bool {
	switch status {
	case HistoryStatusRunning, HistoryStatusSucceeded, HistoryStatusFailed, HistoryStatusCanceled, HistoryStatusInterrupted:
		return true
	default:
		return false
	}
}

func validHistoryTargetKind(kind string) bool {
	switch kind {
	case "platform", "application", "database", "repository", "service":
		return true
	default:
		return false
	}
}

func sanitizeHistoryTargets(targets []HistoryTarget) []HistoryTarget {
	unique := map[string]HistoryTarget{}
	for _, target := range targets {
		kind := strings.TrimSpace(strings.ToLower(target.Kind))
		canonicalID := sanitizeHistoryText(target.CanonicalID, 160)
		if !validHistoryTargetKind(kind) || canonicalID == "" {
			continue
		}
		key := kind + ":" + canonicalID
		unique[key] = HistoryTarget{Kind: kind, CanonicalID: canonicalID, DisplayName: sanitizeHistoryText(target.DisplayName, 160)}
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > historyMaximumTargets {
		keys = keys[:historyMaximumTargets]
	}
	out := make([]HistoryTarget, 0, len(keys))
	for ordinal, key := range keys {
		target := unique[key]
		target.Ordinal = ordinal
		out = append(out, target)
	}
	return out
}

func sanitizeHistoryEvidenceReads(reads []HistoryEvidenceRead) []HistoryEvidenceRead {
	if len(reads) > historyMaximumEvidenceReads {
		reads = reads[:historyMaximumEvidenceReads]
	}
	out := make([]HistoryEvidenceRead, 0, len(reads))
	for index, read := range reads {
		capability := sanitizeHistoryText(read.Capability, 100)
		if capability == "" {
			continue
		}
		read.ID = 0
		read.RequestID = sanitizeHistoryText(read.RequestID, 160)
		if read.EvidenceRound < 1 || read.EvidenceRound > 2 {
			read.EvidenceRound = 1
		}
		read.Order = index
		read.Capability = capability
		read.Arguments = sanitizeHistoryArguments(read.Arguments)
		read.SafeSummary = sanitizeHistoryText(read.SafeSummary, historySummaryMaxRunes)
		read.Status = sanitizeHistoryText(read.Status, 40)
		read.Availability = sanitizeHistoryText(read.Availability, 40)
		out = append(out, read)
	}
	return out
}

func historyKindForCapture(metadata HistoryExecutionMetadata, reads []HistoryEvidenceRead) HistoryKind {
	switch metadata.AnswerMode {
	case "phase2c", "deterministic", "agent":
		return HistoryKindInvestigation
	}
	if metadata.Route != "" || len(reads) > 0 {
		return HistoryKindInvestigation
	}
	return HistoryKindConversation
}

func deriveHistoryTargets(metadata HistoryExecutionMetadata, reads []HistoryEvidenceRead) []HistoryTarget {
	targets := []HistoryTarget{}
	switch InvestigationRouteID(metadata.Route) {
	case RouteCurrentPlatformHealth, RouteThermalInvestigation, RouteRestartInvestigation:
		targets = append(targets, HistoryTarget{Kind: "platform", CanonicalID: "dell", DisplayName: "Dell"})
	case RouteRepositoryInvestigation:
		targets = append(targets, HistoryTarget{Kind: "repository", CanonicalID: "local", DisplayName: "Local repository"})
	}
	for _, read := range reads {
		if app, _ := read.Arguments["app"].(string); app != "" {
			targets = append(targets, HistoryTarget{Kind: "application", CanonicalID: app, DisplayName: app})
			if strings.Contains(read.Capability, "repository") {
				targets = append(targets, HistoryTarget{Kind: "repository", CanonicalID: app, DisplayName: app})
			}
		}
		if databaseID, _ := read.Arguments["database_id"].(string); databaseID != "" {
			targets = append(targets, HistoryTarget{Kind: "database", CanonicalID: databaseID, DisplayName: databaseID})
		}
		if service, _ := read.Arguments["service"].(string); service != "" {
			targets = append(targets, HistoryTarget{Kind: "service", CanonicalID: service, DisplayName: service})
		}
		if read.Capability == "get_platform_overview" {
			targets = append(targets, HistoryTarget{Kind: "platform", CanonicalID: "dell", DisplayName: "Dell"})
		}
		if read.Capability == "list_databases" {
			targets = append(targets, HistoryTarget{Kind: "database", CanonicalID: "reactorlab", DisplayName: "ReactorLab databases"})
		}
	}
	return sanitizeHistoryTargets(targets)
}
