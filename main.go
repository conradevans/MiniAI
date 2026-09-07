package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	primaryModel        = "qwen3:8b-q4_K_M"
	fallbackModel       = "qwen3:4b"
	sessionTTL          = 45 * time.Second
	modelIdleTTL        = 20 * time.Minute
	modelKeepAlive      = "25m"
	primaryMinAvailable = 10.0
	fallbackMinAvail    = 7.0
	primaryMaxLoad      = 8.0
	fallbackMaxLoad     = 12.0
)

type app struct {
	started   time.Time
	addr      string
	ollamaURL string
	client    *http.Client

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	ID            string
	CreatedAt     time.Time
	LastHeartbeat time.Time
	LastChat      time.Time
}

type healthResponse struct {
	Service string `json:"service"`
	Status  string `json:"status"`
}

type systemStatus struct {
	AvailableMemoryBytes uint64  `json:"available_memory_bytes"`
	AvailableMemoryGiB   float64 `json:"available_memory_gib"`
	Load1                float64 `json:"load1"`
}

type ollamaStatus struct {
	Reachable    bool     `json:"reachable"`
	Version      string   `json:"version,omitempty"`
	LoadedModels []string `json:"loaded_models"`
}

type modelPolicy struct {
	Allowed bool   `json:"allowed"`
	Model   string `json:"model,omitempty"`
	Mode    string `json:"mode"`
	Reason  string `json:"reason"`
}

type statusResponse struct {
	Service        string       `json:"service"`
	Status         string       `json:"status"`
	UptimeSeconds  int64        `json:"uptime_seconds"`
	ActiveSessions int          `json:"active_sessions"`
	System         systemStatus `json:"system"`
	Ollama         ollamaStatus `json:"ollama"`
	ModelPolicy    modelPolicy  `json:"model_policy"`
}

type chatRequest struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

type generateRequest struct {
	Model     string         `json:"model"`
	Prompt    string         `json:"prompt,omitempty"`
	Stream    bool           `json:"stream"`
	Think     bool           `json:"think"`
	KeepAlive any            `json:"keep_alive,omitempty"`
	Options   map[string]any `json:"options,omitempty"`
}

type generateChunk struct {
	Response      string `json:"response"`
	Done          bool   `json:"done"`
	DoneReason    string `json:"done_reason,omitempty"`
	EvalCount     int    `json:"eval_count,omitempty"`
	EvalDuration  int64  `json:"eval_duration,omitempty"`
	LoadDuration  int64  `json:"load_duration,omitempty"`
	TotalDuration int64  `json:"total_duration,omitempty"`
	Error         string `json:"error,omitempty"`
}

type versionResponse struct {
	Version string `json:"version"`
}

type psResponse struct {
	Models []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"models"`
}

func main() {
	a := &app{
		started:   time.Now(),
		addr:      env("MINIAI_ADDR", "127.0.0.1:9300"),
		ollamaURL: strings.TrimRight(env("OLLAMA_URL", "http://127.0.0.1:11434"), "/"),
		client:    &http.Client{Timeout: 10 * time.Minute},
		sessions:  make(map[string]*session),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("GET /api/v1/status", a.handleStatus)
	mux.HandleFunc("POST /api/v1/session", a.handleCreateSession)
	mux.HandleFunc("POST /api/v1/session/{id}/heartbeat", a.handleHeartbeat)
	mux.HandleFunc("DELETE /api/v1/session/{id}", a.handleDeleteSession)
	mux.HandleFunc("POST /api/v1/chat/stream", a.handleChatStream)

	go a.reaper()

	srv := &http.Server{
		Addr:              a.addr,
		Handler:           withCommonHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	log.Printf("MiniAI listening on %s", a.addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (a *app) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Service: "miniai", Status: "ok"})
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	sys, err := readSystemStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	oll := a.getOllamaStatus(r.Context())
	policy := choosePolicy(sys, oll.LoadedModels)

	a.mu.Lock()
	active := len(a.sessions)
	a.mu.Unlock()

	status := "ok"
	if !oll.Reachable {
		status = "degraded"
	}

	writeJSON(w, http.StatusOK, statusResponse{
		Service:        "miniai",
		Status:         status,
		UptimeSeconds:  int64(time.Since(a.started).Seconds()),
		ActiveSessions: active,
		System:         sys,
		Ollama:         oll,
		ModelPolicy:    policy,
	})
}

func (a *app) handleCreateSession(w http.ResponseWriter, _ *http.Request) {
	id, err := randomID(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	now := time.Now()
	a.mu.Lock()
	a.sessions[id] = &session{ID: id, CreatedAt: now, LastHeartbeat: now, LastChat: now}
	a.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{
		"session_id":           id,
		"heartbeat_interval_s": 15,
		"heartbeat_timeout_s":  int(sessionTTL.Seconds()),
		"model_idle_timeout_s": int(modelIdleTTL.Seconds()),
	})
}

func (a *app) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.mu.Lock()
	s, ok := a.sessions[id]
	if ok {
		s.LastHeartbeat = time.Now()
	}
	a.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.mu.Lock()
	_, ok := a.sessions[id]
	if ok {
		delete(a.sessions, id)
	}
	remaining := len(a.sessions)
	a.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)

	if remaining == 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := a.unloadAllManagedModels(ctx); err != nil {
				log.Printf("unload after last session close: %v", err)
			}
		}()
	}
}

func (a *app) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Message = strings.TrimSpace(req.Message)
	if req.SessionID == "" || req.Message == "" {
		writeError(w, http.StatusBadRequest, "session_id and message are required")
		return
	}

	now := time.Now()
	a.mu.Lock()
	s, ok := a.sessions[req.SessionID]
	if ok {
		if now.Sub(s.LastHeartbeat) > sessionTTL {
			delete(a.sessions, req.SessionID)
			ok = false
		} else {
			s.LastHeartbeat = now
			s.LastChat = now
		}
	}
	a.mu.Unlock()
	if !ok {
		writeError(w, http.StatusGone, "session expired or not found")
		return
	}

	sys, err := readSystemStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	oll := a.getOllamaStatus(r.Context())
	if !oll.Reachable {
		writeError(w, http.StatusServiceUnavailable, "ollama is unavailable")
		return
	}
	policy := choosePolicy(sys, oll.LoadedModels)
	if !policy.Allowed {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":  "MiniAI will not load a model while the Dell is under resource pressure",
			"policy": policy,
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	sendSSE(w, "meta", map[string]any{
		"model": policy.Model,
		"mode":  policy.Mode,
	})
	flusher.Flush()

	genReq := generateRequest{
		Model:     policy.Model,
		Prompt:    req.Message,
		Stream:    true,
		Think:     false,
		KeepAlive: modelKeepAlive,
		Options: map[string]any{
			"num_ctx":     8192,
			"num_predict": 768,
		},
	}
	body, _ := json.Marshal(genReq)
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.ollamaURL+"/api/generate", strings.NewReader(string(body)))
	if err != nil {
		sendSSE(w, "error", map[string]string{"error": err.Error()})
		flusher.Flush()
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		sendSSE(w, "error", map[string]string{"error": err.Error()})
		flusher.Flush()
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		sendSSE(w, "error", map[string]string{"error": strings.TrimSpace(string(msg))})
		flusher.Flush()
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)
	var final generateChunk
	for scanner.Scan() {
		var chunk generateChunk
		if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
			continue
		}
		if chunk.Error != "" {
			sendSSE(w, "error", map[string]string{"error": chunk.Error})
			flusher.Flush()
			return
		}
		if chunk.Response != "" {
			sendSSE(w, "token", map[string]string{"content": chunk.Response})
			flusher.Flush()
		}
		if chunk.Done {
			final = chunk
		}
	}
	if err := scanner.Err(); err != nil {
		sendSSE(w, "error", map[string]string{"error": err.Error()})
		flusher.Flush()
		return
	}

	tps := 0.0
	if final.EvalDuration > 0 {
		tps = float64(final.EvalCount) / (float64(final.EvalDuration) / 1e9)
	}
	sendSSE(w, "done", map[string]any{
		"model":             policy.Model,
		"mode":              policy.Mode,
		"tokens":            final.EvalCount,
		"tokens_per_second": round2(tps),
		"load_seconds":      round2(float64(final.LoadDuration) / 1e9),
		"total_seconds":     round2(float64(final.TotalDuration) / 1e9),
	})
	flusher.Flush()
}

func (a *app) reaper() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for now := range ticker.C {
		a.mu.Lock()
		for id, s := range a.sessions {
			if now.Sub(s.LastHeartbeat) > sessionTTL {
				delete(a.sessions, id)
			}
		}
		active := len(a.sessions)
		idle := active > 0
		if idle {
			for _, s := range a.sessions {
				if now.Sub(s.LastChat) < modelIdleTTL {
					idle = false
					break
				}
			}
		}
		a.mu.Unlock()

		if active == 0 || idle {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := a.unloadAllManagedModels(ctx); err != nil {
				log.Printf("background unload: %v", err)
			}
			cancel()
		}
	}
}

func (a *app) getOllamaStatus(ctx context.Context) ollamaStatus {
	out := ollamaStatus{LoadedModels: []string{}}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.ollamaURL+"/api/version", nil)
	resp, err := a.client.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return out
	}
	var vr versionResponse
	if json.NewDecoder(resp.Body).Decode(&vr) != nil {
		return out
	}
	out.Reachable = true
	out.Version = vr.Version

	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, a.ollamaURL+"/api/ps", nil)
	resp, err = a.client.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return out
	}
	var ps psResponse
	if json.NewDecoder(resp.Body).Decode(&ps) != nil {
		return out
	}
	for _, m := range ps.Models {
		name := m.Name
		if name == "" {
			name = m.Model
		}
		if name != "" {
			out.LoadedModels = append(out.LoadedModels, name)
		}
	}
	return out
}

func choosePolicy(sys systemStatus, loaded []string) modelPolicy {
	for _, name := range loaded {
		if sameModel(name, primaryModel) {
			return modelPolicy{Allowed: true, Model: primaryModel, Mode: "primary", Reason: "primary model is already loaded for an active MiniAI session"}
		}
		if sameModel(name, fallbackModel) {
			return modelPolicy{Allowed: true, Model: fallbackModel, Mode: "fallback", Reason: "fallback model is already loaded for an active MiniAI session"}
		}
	}

	if sys.AvailableMemoryGiB >= primaryMinAvailable && sys.Load1 < primaryMaxLoad {
		return modelPolicy{Allowed: true, Model: primaryModel, Mode: "primary", Reason: fmt.Sprintf("available RAM %.2f GiB and load1 %.2f satisfy primary thresholds", sys.AvailableMemoryGiB, sys.Load1)}
	}
	if sys.AvailableMemoryGiB >= fallbackMinAvail && sys.Load1 < fallbackMaxLoad {
		return modelPolicy{Allowed: true, Model: fallbackModel, Mode: "fallback", Reason: fmt.Sprintf("available RAM %.2f GiB or load1 %.2f require fallback thresholds", sys.AvailableMemoryGiB, sys.Load1)}
	}
	return modelPolicy{Allowed: false, Mode: "blocked", Reason: fmt.Sprintf("available RAM %.2f GiB and load1 %.2f do not safely permit a local model", sys.AvailableMemoryGiB, sys.Load1)}
}

func sameModel(got, want string) bool {
	if got == want {
		return true
	}
	return strings.TrimSuffix(got, ":latest") == strings.TrimSuffix(want, ":latest")
}

func (a *app) unloadAllManagedModels(ctx context.Context) error {
	oll := a.getOllamaStatus(ctx)
	if !oll.Reachable {
		return errors.New("ollama unavailable")
	}
	for _, name := range oll.LoadedModels {
		var target string
		switch {
		case sameModel(name, primaryModel):
			target = primaryModel
		case sameModel(name, fallbackModel):
			target = fallbackModel
		default:
			continue
		}
		reqBody, _ := json.Marshal(generateRequest{Model: target, Stream: false, KeepAlive: 0})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.ollamaURL+"/api/generate", strings.NewReader(string(reqBody)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.client.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("unload %s returned %s", target, resp.Status)
		}
	}
	return nil
}

func readSystemStatus() (systemStatus, error) {
	mem, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return systemStatus{}, err
	}
	var availableKB uint64
	for _, line := range strings.Split(string(mem), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				availableKB, _ = strconv.ParseUint(fields[1], 10, 64)
			}
			break
		}
	}
	if availableKB == 0 {
		return systemStatus{}, errors.New("MemAvailable not found")
	}

	loadData, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return systemStatus{}, err
	}
	fields := strings.Fields(string(loadData))
	if len(fields) == 0 {
		return systemStatus{}, errors.New("loadavg unavailable")
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return systemStatus{}, err
	}

	bytes := availableKB * 1024
	gib := float64(bytes) / (1024 * 1024 * 1024)
	return systemStatus{AvailableMemoryBytes: bytes, AvailableMemoryGiB: round2(gib), Load1: load1}, nil
}

func randomID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func sendSSE(w io.Writer, event string, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func round2(v float64) float64 {
	if v == 0 {
		return 0
	}
	s := fmt.Sprintf("%.2f", v)
	out, _ := strconv.ParseFloat(s, 64)
	return out
}
