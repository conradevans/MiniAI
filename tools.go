package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	repoReadMaxBytes         = 256 * 1024
	repoSearchMaxFiles       = 500
	repoSearchMaxResults     = 50
	repoSearchMaxHitsPerFile = 5
	repoListMaxEntries       = 200
	repoSearchLineMax        = 8 * 1024
	logDefaultLines          = 80
	logMaxLines              = 200
	logMaxOutputBytes        = 64 * 1024
	miniDeployMaxResponse    = 8 * 1024 * 1024
)

type repoEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Type      string `json:"type"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

type repoListResponse struct {
	App       string      `json:"app"`
	Path      string      `json:"path"`
	Entries   []repoEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
}

type repoFileResponse struct {
	App       string `json:"app"`
	Path      string `json:"path"`
	SizeBytes int    `json:"size_bytes"`
	Content   string `json:"content"`
	Redacted  bool   `json:"redacted"`
}

type repoSearchHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type repoSearchResponse struct {
	App          string          `json:"app"`
	Query        string          `json:"query"`
	Path         string          `json:"path"`
	FilesScanned int             `json:"files_scanned"`
	Hits         []repoSearchHit `json:"hits"`
	Truncated    bool            `json:"truncated"`
}

type logToolResponse struct {
	App       string `json:"app"`
	Kind      string `json:"kind"`
	Container string `json:"container,omitempty"`
	Lines     int    `json:"lines"`
	Bytes     int    `json:"bytes"`
	Logs      string `json:"logs"`
	Truncated bool   `json:"truncated"`
	Redacted  bool   `json:"redacted"`
	Source    string `json:"source"`
}

type miniDeployRuntimeLogs struct {
	App       string `json:"app"`
	Container string `json:"container"`
	Logs      string `json:"logs"`
}

type miniDeployDeploymentLogs struct {
	App  string `json:"app"`
	Logs string `json:"logs"`
}

var (
	bearerPattern           = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)[A-Za-z0-9._~+\-/=]+`)
	urlCredPattern          = regexp.MustCompile(`(https?://[^\s:/@]+:)[^\s@/]+(@)`)
	secretAssignmentPattern = regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|client[_-]?secret|access[_-]?key|database[_-]?url|db[_-]?url|private[_-]?key)[A-Za-z0-9_.-]*\s*[:=]\s*)([^\s,;]+)`)
)

func (a *app) handleRepoList(w http.ResponseWriter, r *http.Request) {
	appName := strings.TrimSpace(r.PathValue("app"))
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	out, err := a.listRepository(appName, rel)
	if err != nil {
		writeToolError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleRepoFile(w http.ResponseWriter, r *http.Request) {
	appName := strings.TrimSpace(r.PathValue("app"))
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if rel == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	out, err := a.readRepositoryFile(appName, rel)
	if err != nil {
		writeToolError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleRepoSearch(w http.ResponseWriter, r *http.Request) {
	appName := strings.TrimSpace(r.PathValue("app"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if len(query) < 2 || len(query) > 128 {
		writeError(w, http.StatusBadRequest, "q must be between 2 and 128 characters")
		return
	}
	out, err := a.searchRepository(r.Context(), appName, rel, query)
	if err != nil {
		writeToolError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleRuntimeLogs(w http.ResponseWriter, r *http.Request) {
	lines, err := parseRequestedLines(r.URL.Query().Get("lines"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := a.readMiniDeployLogs(r.Context(), strings.TrimSpace(r.PathValue("app")), "runtime", lines)
	if err != nil {
		writeToolError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleDeploymentLogs(w http.ResponseWriter, r *http.Request) {
	lines, err := parseRequestedLines(r.URL.Query().Get("lines"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := a.readMiniDeployLogs(r.Context(), strings.TrimSpace(r.PathValue("app")), "deployment", lines)
	if err != nil {
		writeToolError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var (
	errRepoForbidden = errors.New("repository path is blocked")
	errRepoTooLarge  = errors.New("repository file is too large")
	errRepoBinary    = errors.New("repository file is not text")
)

func writeToolError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, "resource not found")
	case errors.Is(err, errRepoForbidden), errors.Is(err, errRepoBinary), errors.Is(err, errRepoTooLarge):
		writeError(w, http.StatusForbidden, err.Error())
	default:
		writeError(w, http.StatusBadGateway, err.Error())
	}
}

func (a *app) listRepository(appName, rel string) (repoListResponse, error) {
	root, target, cleanRel, err := a.resolveRepositoryPath(appName, rel)
	_ = root
	if err != nil {
		return repoListResponse{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return repoListResponse{}, err
	}
	if !info.IsDir() {
		return repoListResponse{}, fmt.Errorf("path is not a directory")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return repoListResponse{}, err
	}
	out := repoListResponse{App: appName, Path: slashPath(cleanRel), Entries: []repoEntry{}}
	for _, entry := range entries {
		if isBlockedRepoComponent(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if len(out.Entries) >= repoListMaxEntries {
			out.Truncated = true
			break
		}
		kind := "file"
		size := int64(0)
		if entry.IsDir() {
			kind = "directory"
		} else if info, statErr := entry.Info(); statErr == nil {
			size = info.Size()
		}
		child := entry.Name()
		if cleanRel != "." {
			child = filepath.Join(cleanRel, child)
		}
		out.Entries = append(out.Entries, repoEntry{Name: entry.Name(), Path: slashPath(child), Type: kind, SizeBytes: size})
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		if out.Entries[i].Type != out.Entries[j].Type {
			return out.Entries[i].Type == "directory"
		}
		return out.Entries[i].Name < out.Entries[j].Name
	})
	return out, nil
}

func (a *app) readRepositoryFile(appName, rel string) (repoFileResponse, error) {
	_, target, cleanRel, err := a.resolveRepositoryPath(appName, rel)
	if err != nil {
		return repoFileResponse{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return repoFileResponse{}, err
	}
	if !info.Mode().IsRegular() {
		return repoFileResponse{}, errRepoForbidden
	}
	if info.Size() > repoReadMaxBytes {
		return repoFileResponse{}, errRepoTooLarge
	}
	if isObviousBinaryName(info.Name()) {
		return repoFileResponse{}, errRepoBinary
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return repoFileResponse{}, err
	}
	if looksBinary(data) {
		return repoFileResponse{}, errRepoBinary
	}
	content, redacted := scrubSensitiveText(string(data))
	return repoFileResponse{App: appName, Path: slashPath(cleanRel), SizeBytes: len(data), Content: content, Redacted: redacted}, nil
}

func (a *app) searchRepository(ctx context.Context, appName, rel, query string) (repoSearchResponse, error) {
	root, start, cleanRel, err := a.resolveRepositoryPath(appName, rel)
	if err != nil {
		return repoSearchResponse{}, err
	}
	info, err := os.Stat(start)
	if err != nil {
		return repoSearchResponse{}, err
	}
	if !info.IsDir() {
		return repoSearchResponse{}, fmt.Errorf("search path is not a directory")
	}

	out := repoSearchResponse{
		App:   appName,
		Query: query,
		Path:  slashPath(cleanRel),
		Hits:  []repoSearchHit{},
	}
	needle := strings.ToLower(query)
	stop := errors.New("search complete")

	err = filepath.WalkDir(start, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if path != start && (isBlockedRepoComponent(d.Name()) || d.Type()&os.ModeSymlink != 0) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if out.FilesScanned >= repoSearchMaxFiles {
			out.Truncated = true
			return stop
		}

		entryInfo, statErr := d.Info()
		if statErr != nil || !entryInfo.Mode().IsRegular() || entryInfo.Size() > repoReadMaxBytes || isObviousBinaryName(d.Name()) {
			return nil
		}

		f, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		out.FilesScanned++

		relPath, relErr := filepath.Rel(root, path)
		if relErr != nil {
			_ = f.Close()
			return nil
		}
		relPath = slashPath(relPath)

		fileHits := make([]repoSearchHit, 0, repoSearchMaxHitsPerFile)
		scanner := bufio.NewScanner(io.LimitReader(f, repoReadMaxBytes+1))
		scanner.Buffer(make([]byte, 16*1024), repoSearchLineMax)
		lineNo := 0

		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			if strings.IndexByte(line, 0) >= 0 {
				break
			}
			if !strings.Contains(strings.ToLower(line), needle) {
				continue
			}
			if len(fileHits) >= repoSearchMaxHitsPerFile {
				out.Truncated = true
				break
			}

			text, _ := scrubSensitiveText(strings.TrimSpace(line))
			text = truncateRunes(text, 320)
			fileHits = append(fileHits, repoSearchHit{
				Path: relPath,
				Line: lineNo,
				Text: text,
			})
		}
		_ = f.Close()

		remaining := repoSearchMaxResults - len(out.Hits)
		if remaining <= 0 {
			out.Truncated = true
			return stop
		}
		if len(fileHits) > remaining {
			out.Hits = append(out.Hits, fileHits[:remaining]...)
			out.Truncated = true
			return stop
		}
		out.Hits = append(out.Hits, fileHits...)

		if len(out.Hits) >= repoSearchMaxResults {
			out.Truncated = true
			return stop
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return repoSearchResponse{}, err
	}
	return out, nil
}

func (a *app) resolveRepositoryPath(appName, rel string) (root, target, cleanRel string, err error) {
	if !safeAppName(appName) {
		return "", "", "", errRepoForbidden
	}
	base, err := filepath.Abs(a.repoRoot)
	if err != nil {
		return "", "", "", err
	}
	root = filepath.Join(base, appName)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", "", "", err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", "", errRepoForbidden
	}

	rel = strings.TrimSpace(rel)
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) || strings.ContainsRune(rel, '\\') {
		return "", "", "", errRepoForbidden
	}
	cleanRel = filepath.Clean(filepath.FromSlash(rel))
	if cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) {
		return "", "", "", errRepoForbidden
	}
	if cleanRel != "." {
		for _, part := range strings.Split(cleanRel, string(filepath.Separator)) {
			if part == "" || part == "." || part == ".." || isBlockedRepoComponent(part) {
				return "", "", "", errRepoForbidden
			}
		}
	}
	target = filepath.Join(root, cleanRel)
	relCheck, err := filepath.Rel(root, target)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", "", "", errRepoForbidden
	}

	cursor := root
	if cleanRel != "." {
		for _, part := range strings.Split(cleanRel, string(filepath.Separator)) {
			cursor = filepath.Join(cursor, part)
			info, lerr := os.Lstat(cursor)
			if lerr != nil {
				return "", "", "", lerr
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return "", "", "", errRepoForbidden
			}
		}
	}
	return root, target, cleanRel, nil
}

func isBlockedRepoComponent(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" || lower == "." || lower == ".." {
		return true
	}
	for _, blocked := range []string{".git", "node_modules", "dist", "build", "coverage", ".next", ".cache", "vendor"} {
		if lower == blocked {
			return true
		}
	}
	if isSecretName(lower) || strings.HasSuffix(lower, ".pem") || strings.HasSuffix(lower, ".key") || strings.HasSuffix(lower, ".p12") || strings.HasSuffix(lower, ".pfx") {
		return true
	}
	return false
}

func isObviousBinaryName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf", ".zip", ".gz", ".tgz", ".tar", ".7z", ".rar", ".woff", ".woff2", ".ttf", ".otf", ".mp3", ".mp4", ".mov", ".avi", ".sqlite", ".db", ".bin", ".exe", ".dll", ".so", ".dylib":
		return true
	default:
		return false
	}
}

func looksBinary(data []byte) bool {
	limit := len(data)
	if limit > 8192 {
		limit = 8192
	}
	for _, b := range data[:limit] {
		if b == 0 {
			return true
		}
	}
	return false
}

func scrubSensitiveText(input string) (string, bool) {
	out := input
	redacted := false
	apply := func(re *regexp.Regexp, replacement string) {
		next := re.ReplaceAllString(out, replacement)
		if next != out {
			redacted = true
			out = next
		}
	}
	apply(bearerPattern, `${1}[REDACTED]`)
	apply(urlCredPattern, `${1}[REDACTED]${2}`)
	apply(secretAssignmentPattern, `${1}[REDACTED]`)
	return out, redacted
}

func (a *app) readMiniDeployLogs(ctx context.Context, appName, kind string, lines int) (logToolResponse, error) {
	if !safeAppName(appName) {
		return logToolResponse{}, errRepoForbidden
	}
	if !a.deploymentExists(ctx, appName) {
		return logToolResponse{}, os.ErrNotExist
	}
	endpoint := ""
	switch kind {
	case "runtime":
		endpoint = a.minideployURL + "/deployments/" + url.PathEscape(appName) + "/logs"
	case "deployment":
		endpoint = a.minideployURL + "/deployments/" + url.PathEscape(appName) + "/deploy-logs"
	default:
		return logToolResponse{}, fmt.Errorf("invalid log kind")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return logToolResponse{}, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return logToolResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return logToolResponse{}, os.ErrNotExist
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return logToolResponse{}, fmt.Errorf("MiniDeploy %s logs returned %s: %s", kind, resp.Status, strings.TrimSpace(string(msg)))
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, miniDeployMaxResponse))
	logs := ""
	container := ""
	if kind == "runtime" {
		var payload miniDeployRuntimeLogs
		if err := dec.Decode(&payload); err != nil {
			return logToolResponse{}, err
		}
		logs = payload.Logs
		container = payload.Container
	} else {
		var payload miniDeployDeploymentLogs
		if err := dec.Decode(&payload); err != nil {
			return logToolResponse{}, err
		}
		logs = payload.Logs
	}
	trimmed, truncated := trimLogOutput(logs, lines, logMaxOutputBytes)
	trimmed, redacted := scrubSensitiveText(trimmed)
	return logToolResponse{
		App: appName, Kind: kind, Container: container, Lines: countLines(trimmed), Bytes: len([]byte(trimmed)),
		Logs: trimmed, Truncated: truncated, Redacted: redacted, Source: "MiniDeploy private API (managed-secret redaction + MiniAI secondary scrub)",
	}, nil
}

func (a *app) deploymentExists(ctx context.Context, appName string) bool {
	deployments, err := a.fetchDeployments(ctx)
	if err != nil {
		return false
	}
	for _, d := range deployments {
		name, _ := d["app"].(string)
		if strings.EqualFold(name, appName) {
			return true
		}
	}
	return false
}

func parseRequestedLines(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return logDefaultLines, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > logMaxLines {
		return 0, fmt.Errorf("lines must be between 1 and %d", logMaxLines)
	}
	return n, nil
}

func trimLogOutput(input string, lines, maxBytes int) (string, bool) {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	parts := strings.Split(strings.TrimRight(input, "\n"), "\n")
	truncated := false
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
		truncated = true
	}
	out := strings.Join(parts, "\n")
	if len([]byte(out)) > maxBytes {
		data := []byte(out)
		data = data[len(data)-maxBytes:]
		if idx := strings.IndexByte(string(data), '\n'); idx >= 0 && idx+1 < len(data) {
			data = data[idx+1:]
		}
		out = string(data)
		truncated = true
	}
	return out, truncated
}

func countLines(input string) int {
	if strings.TrimSpace(input) == "" {
		return 0
	}
	return strings.Count(input, "\n") + 1
}

func slashPath(path string) string {
	if path == "." || path == "" {
		return "."
	}
	return filepath.ToSlash(path)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
