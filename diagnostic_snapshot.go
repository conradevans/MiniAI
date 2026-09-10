package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

type appDiagnosticSnapshot struct {
	App                string               `json:"app"`
	CollectedAt        string               `json:"collected_at,omitempty"`
	DeploymentState    string               `json:"deployment_state,omitempty"`
	Strategy           string               `json:"strategy,omitempty"`
	Services           []diagnosticService  `json:"services,omitempty"`
	AllServicesRunning *bool                `json:"all_services_running,omitempty"`
	Health             string               `json:"health,omitempty"`
	HealthSource       string               `json:"health_source,omitempty"`
	Database           *diagnosticDatabase  `json:"database,omitempty"`
	DeployedCommit     string               `json:"deployed_commit,omitempty"`
	RepositoryCommit   string               `json:"repository_commit,omitempty"`
	VersionMismatch    *bool                `json:"version_mismatch,omitempty"`
	DeployedAt         string               `json:"deployed_at,omitempty"`
	ListenerPort       int                  `json:"listener_port,omitempty"`
	RecentActivity     []diagnosticActivity `json:"recent_activity,omitempty"`
	SourceStatus       map[string]string    `json:"source_status"`
}

type diagnosticService struct {
	Name          string  `json:"name"`
	State         string  `json:"state,omitempty"`
	Health        string  `json:"health,omitempty"`
	UptimeSeconds float64 `json:"uptime_seconds,omitempty"`
	RestartCount  int64   `json:"restart_count,omitempty"`
}

type diagnosticDatabase struct {
	Name   string `json:"name,omitempty"`
	State  string `json:"state,omitempty"`
	Status string `json:"status,omitempty"`
}

type diagnosticActivity struct {
	OccurredAt string `json:"occurred_at,omitempty"`
	Source     string `json:"source,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Severity   string `json:"severity,omitempty"`
	Subject    string `json:"subject,omitempty"`
	Message    string `json:"message,omitempty"`
}

type diagnosticEvidence struct {
	ToolName string
	Summary  string
	Args     map[string]any
}

func isDiagnosticReasoningQuestion(message string) bool {
	if isDiagnosticVerificationQuestion(message) {
		return true
	}
	m := strings.ToLower(message)
	for _, term := range []string{
		"why", "diagnose", "debug", "investigate", "root cause", "most likely",
		"caused", "cause", "causing", "explain", "relate", "relationship", "what changed",
	} {
		if messageContainsTerm(m, term) {
			return true
		}
	}
	return false
}

func isDiagnosticVerificationQuestion(message string) bool {
	m := strings.ToLower(strings.TrimSpace(message))
	if hasCurrentFailurePremise(m) {
		return false
	}
	if containsAny(m, "okay", "all good", "anything wrong", "any issue", "any problem", "hidden issue") || messageContainsTerm(m, "ok") {
		return containsAny(m, "is ", "does ", "check", "verify", "confirm", "investigate", "whether")
	}
	return containsAny(m, "check whether", "verify whether", "confirm whether", "investigate whether") &&
		containsAny(m, "healthy", "working", "issue", "problem", "error")
}

func isExplicitDiagnosticLookup(message string) bool {
	if isDiagnosticReasoningQuestion(message) || isRepositoryCodeQuestion(message) {
		return false
	}
	m := strings.ToLower(message)
	if asksForDeployedCommit(m) {
		return true
	}
	for _, term := range []string{
		"running", "healthy", "health", "status", "deployed commit", "deployed version",
		"repository commit", "same version", "last deployed", "listening", "port",
	} {
		if messageContainsTerm(m, term) {
			return true
		}
	}
	return isExplicitLogLookup(message)
}

func (a *app) resolveDiagnosticSnapshot(ctx context.Context, message string) (appDiagnosticSnapshot, bool) {
	deployments, err := a.fetchDeployments(ctx)
	if err != nil {
		return appDiagnosticSnapshot{}, false
	}
	system, systemErr := a.fetchReactorSystem(ctx)
	normalized := normalizeMatch(message)
	type candidate struct {
		name       string
		deployment map[string]any
		service    map[string]any
	}
	candidates := map[string]candidate{}
	for _, deployment := range deployments {
		name := stringField(deployment, "app")
		key := normalizeMatch(name)
		if key != "" && strings.Contains(normalized, key) {
			candidates[strings.ToLower(name)] = candidate{name: name, deployment: deployment}
		}
	}
	if systemErr == nil {
		for _, service := range objectArray(system["services"]) {
			name := stringField(service, "name")
			key := normalizeMatch(name)
			if key == "" || !strings.Contains(normalized, key) {
				continue
			}
			lower := strings.ToLower(name)
			if existing, ok := candidates[lower]; ok {
				existing.service = service
				candidates[lower] = existing
			} else {
				candidates[lower] = candidate{name: name, service: service}
			}
		}
	}
	if len(candidates) != 1 {
		return appDiagnosticSnapshot{}, false
	}
	var selected candidate
	for _, item := range candidates {
		selected = item
	}
	snapshot := appDiagnosticSnapshot{
		App: selected.name, Services: []diagnosticService{}, RecentActivity: []diagnosticActivity{},
		SourceStatus: map[string]string{"reactorlab_deployment": "not-found", "reactorlab_system": "unavailable", "reactorlab_activity": "unavailable", "repository": "not-found"},
	}
	if collected := stringField(system, "collectedAt"); collected != "" {
		snapshot.CollectedAt = collected
	} else {
		snapshot.CollectedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if systemErr == nil {
		snapshot.SourceStatus["reactorlab_system"] = "ok"
	}
	if selected.deployment != nil {
		fillDeploymentSnapshot(&snapshot, selected.deployment)
		snapshot.SourceStatus["reactorlab_deployment"] = "ok"
	}
	if selected.service != nil && selected.deployment == nil {
		active, known := boolField(selected.service, "active")
		state := stringField(selected.service, "status")
		snapshot.Services = append(snapshot.Services, diagnosticService{Name: selected.name, State: state, Health: state})
		if known {
			snapshot.AllServicesRunning = boolPointer(active)
		}
		snapshot.Health = state
		snapshot.HealthSource = "ReactorLab service state"
	}
	repo := a.readRepoContext(selected.name)
	if repo.Exists {
		snapshot.RepositoryCommit = repo.Commit
		snapshot.SourceStatus["repository"] = "ok"
	}
	if snapshot.DeployedCommit != "" && snapshot.RepositoryCommit != "" {
		snapshot.VersionMismatch = boolPointer(!commitValuesMatch(snapshot.DeployedCommit, snapshot.RepositoryCommit))
	}
	if events, err := a.fetchActivity(ctx, 30); err == nil {
		snapshot.SourceStatus["reactorlab_activity"] = "ok"
		activity := selectActivity(selected.name, events)
		for _, raw := range activity.AppMatched {
			event, ok := raw.(map[string]any)
			if !ok || len(snapshot.RecentActivity) >= 6 {
				continue
			}
			snapshot.RecentActivity = append(snapshot.RecentActivity, diagnosticActivity{
				OccurredAt: stringField(event, "occurredAt"), Source: stringField(event, "source"),
				Kind: stringField(event, "kind"), Severity: stringField(event, "severity"),
				Subject: scrubbedDiagnosticText(stringField(event, "subject"), 160), Message: scrubbedDiagnosticText(stringField(event, "message"), 240),
			})
		}
	}
	return snapshot, true
}

func fillDeploymentSnapshot(snapshot *appDiagnosticSnapshot, deployment map[string]any) {
	snapshot.DeploymentState = stringField(deployment, "status")
	snapshot.Strategy = stringField(deployment, "strategy")
	snapshot.DeployedCommit = firstStringField(deployment, "deployedCommit", "commit", "revision", "version")
	snapshot.DeployedAt = firstStringField(deployment, "deployedAt", "deployed_at", "updatedAt", "createdAt")
	snapshot.ListenerPort = int(number(deployment["port"]))
	allRunning := true
	containers, _ := deployment["containers"].([]any)
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		state := stringField(container, "state")
		if !strings.EqualFold(state, "running") {
			allRunning = false
		}
		snapshot.Services = append(snapshot.Services, diagnosticService{
			Name: stringField(container, "service"), State: state, Health: stringField(container, "health"),
			UptimeSeconds: number(container["uptimeSeconds"]), RestartCount: int64(number(container["restartCount"])),
		})
	}
	if len(snapshot.Services) > 0 {
		snapshot.AllServicesRunning = boolPointer(allRunning)
	}
	snapshot.Health = snapshot.DeploymentState
	snapshot.HealthSource = "ReactorLab deployment state"
	if database, ok := deployment["database"].(map[string]any); ok {
		snapshot.Database = &diagnosticDatabase{
			Name: stringField(database, "displayName"), State: stringField(database, "state"), Status: stringField(database, "status"),
		}
	}
}

func deterministicSnapshotAnswer(snapshot appDiagnosticSnapshot, message string) (string, bool) {
	m := strings.ToLower(message)
	switch {
	case messageContainsTerm(m, "running"):
		if snapshot.AllServicesRunning == nil {
			return "", false
		}
		if *snapshot.AllServicesRunning {
			return fmt.Sprintf("%s is running. All %d reported service components are in the running state.", snapshot.App, len(snapshot.Services)), true
		}
		return fmt.Sprintf("%s is not fully running. ReactorLab reports: %s.", snapshot.App, formatServiceStates(snapshot.Services)), true
	case messageContainsTerm(m, "healthy") || messageContainsTerm(m, "health"):
		if snapshot.Health == "" {
			return "", false
		}
		if strings.EqualFold(snapshot.Health, "healthy") {
			return fmt.Sprintf("%s is healthy according to %s.", snapshot.App, snapshot.HealthSource), true
		}
		if strings.EqualFold(snapshot.Health, "active") {
			return fmt.Sprintf("ReactorLab reports the %s service is active; this snapshot does not include a separate application health probe.", snapshot.App), true
		}
		return fmt.Sprintf("%s is not reported healthy. Its current state is %q according to %s.", snapshot.App, snapshot.Health, snapshot.HealthSource), true
	case messageContainsTerm(m, "status"):
		state := snapshot.DeploymentState
		stateKind := "deployment"
		if state == "" {
			state = snapshot.Health
			stateKind = "service"
		}
		if state == "" {
			return "", false
		}
		answer := fmt.Sprintf("%s's %s state is %s.", snapshot.App, stateKind, state)
		if len(snapshot.Services) > 0 {
			answer += " Reported services: " + formatServiceStates(snapshot.Services) + "."
		}
		return answer, true
	case strings.Contains(m, "same version"):
		if snapshot.VersionMismatch == nil {
			return "", false
		}
		if !*snapshot.VersionMismatch {
			return fmt.Sprintf("%s's deployed commit and repository commit match at %s.", snapshot.App, snapshot.DeployedCommit), true
		}
		return fmt.Sprintf("%s's deployed commit %s differs from repository commit %s.", snapshot.App, snapshot.DeployedCommit, snapshot.RepositoryCommit), true
	case asksForDeployedCommit(m):
		if snapshot.DeployedCommit == "" {
			return "", false
		}
		return fmt.Sprintf("%s is deployed at commit %s.", snapshot.App, snapshot.DeployedCommit), true
	case strings.Contains(m, "repository commit"):
		if snapshot.RepositoryCommit == "" {
			return "", false
		}
		return fmt.Sprintf("%s's current repository commit is %s.", snapshot.App, snapshot.RepositoryCommit), true
	case strings.Contains(m, "last deployed"):
		if snapshot.DeployedAt == "" {
			return "", false
		}
		return fmt.Sprintf("%s was last deployed at %s.", snapshot.App, snapshot.DeployedAt), true
	case messageContainsTerm(m, "port") || messageContainsTerm(m, "listening"):
		if snapshot.ListenerPort == 0 {
			return "", false
		}
		return fmt.Sprintf("%s is reported listening on port %d.", snapshot.App, snapshot.ListenerPort), true
	}
	return "", false
}

func isRepositoryCodeQuestion(message string) bool {
	m := strings.ToLower(message)
	for _, term := range []string{
		"code", "source", "file", "route", "function", "class", "symbol",
		"definition", "defined", "implementation", "endpoint",
	} {
		if messageContainsTerm(m, term) {
			return true
		}
	}
	return false
}

func asksForDeployedCommit(message string) bool {
	return (messageContainsTerm(message, "commit") || messageContainsTerm(message, "version")) &&
		(messageContainsTerm(message, "deployed") || messageContainsTerm(message, "deployment"))
}

func commitValuesMatch(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left == right || strings.HasPrefix(left, right) || strings.HasPrefix(right, left)
}

func scrubbedDiagnosticText(value string, maxRunes int) string {
	value, _ = scrubSensitiveText(value)
	return truncateRunes(value, maxRunes)
}

func formatServiceStates(services []diagnosticService) string {
	parts := make([]string, 0, len(services))
	for _, service := range services {
		parts = append(parts, fmt.Sprintf("%s=%s", service.Name, service.State))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func (a *app) handleDeterministicDiagnosticLookup(w http.ResponseWriter, r *http.Request, message string) bool {
	if !isExplicitDiagnosticLookup(message) {
		return false
	}
	snapshot, ok := a.resolveDiagnosticSnapshot(r.Context(), message)
	if !ok {
		return false
	}
	return a.handleDeterministicDiagnosticSnapshot(w, r, snapshot, message)
}

func (a *app) handleConversationDiagnosticLookup(w http.ResponseWriter, r *http.Request, message string, history []storedMessage) bool {
	if !isExplicitDiagnosticLookup(message) {
		return false
	}

	subject := a.resolveConversationSubject(r.Context(), message, history)
	if subject.App == "" || subject.Ambiguous {
		return a.handleDeterministicDiagnosticLookup(w, r, message)
	}

	// Passing only the canonical app name selects identity. The snapshot
	// resolver still fetches current ReactorLab state for this request.
	snapshot, ok := a.resolveDiagnosticSnapshot(r.Context(), subject.App)
	if !ok {
		return false
	}
	return a.handleDeterministicDiagnosticSnapshot(w, r, snapshot, message)
}

func (a *app) handleDeterministicDiagnosticSnapshot(w http.ResponseWriter, r *http.Request, snapshot appDiagnosticSnapshot, message string) bool {
	if isExplicitLogLookup(message) {
		return a.handleDeterministicLogLookup(w, r, snapshot, message)
	}
	answer, ok := deterministicSnapshotAnswer(snapshot, message)
	if !ok {
		return false
	}
	return emitDeterministicDiagnosticAnswer(w, answer, []diagnosticEvidence{{
		ToolName: "get_app_context", Args: map[string]any{"app": snapshot.App},
		Summary: "resolved structured application state",
	}}, map[string]any{"evidence_sources": 1})
}

func emitDeterministicDiagnosticAnswer(w http.ResponseWriter, answer string, evidence []diagnosticEvidence, metrics map[string]any) bool {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	meta := map[string]any{"answer_mode": "deterministic", "model_invoked": false, "evidence_sources": len(evidence)}
	for key, value := range metrics {
		meta[key] = value
	}
	sendSSE(w, "meta", meta)
	flusher.Flush()
	for _, item := range evidence {
		sendSSE(w, "tool", agentToolEvent{Phase: "start", Name: item.ToolName, Arguments: item.Args, Summary: item.Summary})
		sendSSE(w, "tool", agentToolEvent{Phase: "result", Name: item.ToolName, Summary: item.Summary})
		flusher.Flush()
	}
	emitBufferedAnswer(w, flusher, answer)
	done := map[string]any{
		"answer_mode": "deterministic", "model_invoked": false, "evidence_sources": len(evidence),
		"tool_calls": 0, "planner_calls": 0, "tokens": 0,
	}
	for key, value := range metrics {
		done[key] = value
	}
	sendSSE(w, "done", done)
	flusher.Flush()
	return true
}

func stringField(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return strings.TrimSpace(value)
}

func firstStringField(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringField(object, key); value != "" {
			return value
		}
	}
	return ""
}

func boolField(object map[string]any, key string) (bool, bool) {
	value, ok := object[key].(bool)
	return value, ok
}

func boolPointer(value bool) *bool {
	return &value
}

func diagnosticPacketJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}
