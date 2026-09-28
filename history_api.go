package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type historyCursor struct {
	StartedAt time.Time `json:"started_at"`
	ID        string    `json:"id"`
}

func (a *app) handleListHistory(w http.ResponseWriter, r *http.Request) {
	filter, err := parseHistoryListFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := a.store.listHistory(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list execution history")
		return
	}
	nextCursor := ""
	if result.HasMore && len(result.Entries) > 0 {
		last := result.Entries[len(result.Entries)-1]
		nextCursor = encodeHistoryCursor(historyCursor{StartedAt: last.StartedAt, ID: last.ID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": result.Entries, "next_cursor": nextCursor})
}

func (a *app) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !validHistoryOpaqueID(id) {
		writeError(w, http.StatusBadRequest, "invalid history id")
		return
	}
	entry, err := a.store.getHistoryEntry(id)
	if errors.Is(err, errHistoryNotFound) {
		writeError(w, http.StatusNotFound, "history entry not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read execution history")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func parseHistoryListFilter(r *http.Request) (HistoryListFilter, error) {
	query := r.URL.Query()
	allowed := map[string]bool{
		"limit": true, "cursor": true, "kind": true, "status": true, "chat_id": true,
		"route": true, "target_kind": true, "target_id": true,
	}
	for key := range query {
		if !allowed[key] {
			return HistoryListFilter{}, errors.New("invalid history filter")
		}
	}
	filter := HistoryListFilter{Limit: 50}
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return HistoryListFilter{}, errors.New("limit must be between 1 and 100")
		}
		filter.Limit = limit
	}
	if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
		cursor, err := decodeHistoryCursor(raw)
		if err != nil {
			return HistoryListFilter{}, errors.New("invalid cursor")
		}
		filter.BeforeTime, filter.BeforeID = &cursor.StartedAt, cursor.ID
	}
	if raw := strings.TrimSpace(query.Get("kind")); raw != "" {
		kind := HistoryKind(raw)
		if normalizeHistoryKind(kind) != kind {
			return HistoryListFilter{}, errors.New("invalid history kind")
		}
		filter.Kind = kind
	}
	if raw := strings.TrimSpace(query.Get("status")); raw != "" {
		status := HistoryStatus(raw)
		if !validHistoryStatus(status) {
			return HistoryListFilter{}, errors.New("invalid history status")
		}
		filter.Status = status
	}
	if raw := strings.TrimSpace(query.Get("chat_id")); raw != "" {
		if !validHistoryOpaqueID(raw) {
			return HistoryListFilter{}, errors.New("invalid chat id")
		}
		filter.ChatID = raw
	}
	if raw := strings.TrimSpace(query.Get("route")); raw != "" {
		if !validHistoryRoute(raw) {
			return HistoryListFilter{}, errors.New("invalid history route")
		}
		filter.Route = raw
	}
	if raw := strings.TrimSpace(query.Get("target_kind")); raw != "" {
		if !validHistoryTargetKind(raw) {
			return HistoryListFilter{}, errors.New("invalid target kind")
		}
		filter.TargetKind = raw
	}
	if raw := strings.TrimSpace(query.Get("target_id")); raw != "" {
		if len([]rune(raw)) > 160 || strings.ContainsAny(raw, "\r\n\x00") {
			return HistoryListFilter{}, errors.New("invalid target id")
		}
		filter.TargetID = raw
	}
	return filter, nil
}

func validHistoryOpaqueID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func validHistoryRoute(value string) bool {
	switch InvestigationRouteID(value) {
	case RouteExactCurrentFact, RouteCurrentPlatformHealth, RouteThermalInvestigation,
		RouteRestartInvestigation, RouteApplicationCurrent, RouteApplicationPerformance,
		RouteDeploymentCorrelation, RouteDatabaseInvestigation, RouteRepositoryInvestigation,
		RouteUnresolved:
		return true
	default:
		return false
	}
}

func encodeHistoryCursor(cursor historyCursor) string {
	encoded, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeHistoryCursor(value string) (historyCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > 512 {
		return historyCursor{}, errors.New("invalid cursor")
	}
	var cursor historyCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.StartedAt.IsZero() || !validHistoryOpaqueID(cursor.ID) {
		return historyCursor{}, errors.New("invalid cursor")
	}
	cursor.StartedAt = cursor.StartedAt.UTC()
	return cursor, nil
}
