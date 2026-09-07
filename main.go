package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	serviceName     = "miniai"
	defaultAddr     = "127.0.0.1:9300"
	defaultOllama   = "http://127.0.0.1:11434"
	primaryModel    = "qwen3:8b-q4_K_M"
	fallbackModel   = "qwen3:4b"
	primaryRAMGiB   = 10.0
	fallbackRAMGiB  = 7.0
	primaryLoadMax  = 8.0
	fallbackLoadMax = 12.0
)

type Server struct {
	ollamaURL string
	client    *http.Client
	startedAt time.Time
}

type HealthResponse struct {
	Service string `json:"service"`
	Status  string `json:"status"`
}

type StatusResponse struct {
	Service     string       `json:"service"`
	Status      string       `json:"status"`
	UptimeSec   int64        `json:"uptime_seconds"`
	System      SystemStatus `json:"system"`
	Ollama      OllamaStatus `json:"ollama"`
	ModelPolicy ModelPolicy  `json:"model_policy"`
}

type SystemStatus struct {
	AvailableMemoryBytes uint64  `json:"available_memory_bytes"`
	AvailableMemoryGiB   float64 `json:"available_memory_gib"`
	Load1                float64 `json:"load1"`
}

type OllamaStatus struct {
	Reachable    bool     `json:"reachable"`
	Version      string   `json:"version,omitempty"`
	LoadedModels []string `json:"loaded_models"`
	Error        string   `json:"error,omitempty"`
}

type ModelPolicy struct {
	Allowed bool   `json:"allowed"`
	Model   string `json:"model,omitempty"`
	Mode    string `json:"mode"`
	Reason  string `json:"reason"`
}

type ollamaVersionResponse struct {
	Version string `json:"version"`
}

type ollamaPSResponse struct {
	Models []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"models"`
}

func main() {
	addr := envOr("MINIAI_ADDR", defaultAddr)
	ollamaURL := strings.TrimRight(envOr("OLLAMA_URL", defaultOllama), "/")

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		log.Fatalf("invalid MINIAI_ADDR %q: %v", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		log.Fatalf("refusing non-loopback MINIAI_ADDR %q", addr)
	}

	s := &Server{
		ollamaURL: ollamaURL,
		client: &http.Client{
			Timeout: 2 * time.Second,
		},
		startedAt: time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)

	server := &http.Server{
		Addr:              addr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("%s listening on http://%s", serviceName, addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{
		Service: serviceName,
		Status:  "ok",
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	system, err := readSystemStatus()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"service": serviceName,
			"status":  "error",
			"error":   "unable to read system status",
		})
		return
	}

	ollama := s.readOllamaStatus(r.Context())
	policy := chooseModel(system)

	status := "ok"
	if !ollama.Reachable {
		status = "degraded"
	}

	writeJSON(w, http.StatusOK, StatusResponse{
		Service:     serviceName,
		Status:      status,
		UptimeSec:   int64(time.Since(s.startedAt).Seconds()),
		System:      system,
		Ollama:      ollama,
		ModelPolicy: policy,
	})
}

func readSystemStatus() (SystemStatus, error) {
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return SystemStatus{}, err
	}

	var availableKB uint64
	for _, line := range strings.Split(string(meminfo), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return SystemStatus{}, fmt.Errorf("invalid MemAvailable line")
			}
			availableKB, err = strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return SystemStatus{}, err
			}
			break
		}
	}
	if availableKB == 0 {
		return SystemStatus{}, fmt.Errorf("MemAvailable not found")
	}

	loadRaw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return SystemStatus{}, err
	}
	loadFields := strings.Fields(string(loadRaw))
	if len(loadFields) == 0 {
		return SystemStatus{}, fmt.Errorf("invalid /proc/loadavg")
	}
	load1, err := strconv.ParseFloat(loadFields[0], 64)
	if err != nil {
		return SystemStatus{}, err
	}

	bytes := availableKB * 1024
	gib := float64(bytes) / (1024 * 1024 * 1024)

	return SystemStatus{
		AvailableMemoryBytes: bytes,
		AvailableMemoryGiB:   round2(gib),
		Load1:                round2(load1),
	}, nil
}

func chooseModel(system SystemStatus) ModelPolicy {
	if system.AvailableMemoryGiB >= primaryRAMGiB && system.Load1 < primaryLoadMax {
		return ModelPolicy{
			Allowed: true,
			Model:   primaryModel,
			Mode:    "primary",
			Reason:  fmt.Sprintf("available RAM %.2f GiB and load1 %.2f satisfy primary thresholds", system.AvailableMemoryGiB, system.Load1),
		}
	}

	if system.AvailableMemoryGiB >= fallbackRAMGiB && system.Load1 < fallbackLoadMax {
		return ModelPolicy{
			Allowed: true,
			Model:   fallbackModel,
			Mode:    "fallback",
			Reason:  fmt.Sprintf("primary thresholds not met; fallback safe at %.2f GiB available RAM and load1 %.2f", system.AvailableMemoryGiB, system.Load1),
		}
	}

	return ModelPolicy{
		Allowed: false,
		Mode:    "blocked",
		Reason:  fmt.Sprintf("server pressure too high for local inference: %.2f GiB available RAM, load1 %.2f", system.AvailableMemoryGiB, system.Load1),
	}
}

func (s *Server) readOllamaStatus(parent context.Context) OllamaStatus {
	status := OllamaStatus{
		LoadedModels: []string{},
	}

	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()

	var version ollamaVersionResponse
	if err := s.getJSON(ctx, "/api/version", &version); err != nil {
		status.Error = "ollama unavailable"
		return status
	}
	status.Reachable = true
	status.Version = version.Version

	var ps ollamaPSResponse
	if err := s.getJSON(ctx, "/api/ps", &ps); err != nil {
		status.Error = "unable to read loaded models"
		return status
	}
	for _, model := range ps.Models {
		name := model.Name
		if name == "" {
			name = model.Model
		}
		if name != "" {
			status.LoadedModels = append(status.LoadedModels, name)
		}
	}

	return status
}

func (s *Server) getJSON(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.ollamaURL+path, nil)
	if err != nil {
		return err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ollama returned %s", resp.Status)
	}

	return json.NewDecoder(resp.Body).Decode(dst)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write response: %v", err)
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}
