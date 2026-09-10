package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const repositoryIndexMaxFiles = repoSearchMaxFiles

type repositoryIndexCache struct {
	version string
	index   *repositoryIndex
}

type repositoryIndex struct {
	App, Version string
	Entries      []repositoryIndexEntry
	Files        []string
}

type repositoryIndexEntry struct {
	Kind, Name, Path, Method, RoutePath, MountTarget string
	Line                                             int
}

type deterministicRepositoryResult struct {
	App, Answer, Kind string
	Evidence          []deterministicRepositoryEvidence
}

type deterministicRepositoryEvidence struct {
	Path, Summary string
	Line          int
}

type repositorySourceMetadata struct {
	Path          string
	Size, ModTime int64
}

var (
	jsRoutePattern    = regexp.MustCompile(`(?i)\b(?:app|router)\.(get|post|put|patch|delete|options|head)\s*\(\s*["'\x60]([^"'\x60]+)["'\x60]`)
	jsMountPattern    = regexp.MustCompile(`(?i)\b(?:app|router)\.use\s*\(\s*["'\x60]([^"'\x60]+)["'\x60]\s*,\s*([A-Za-z_$][A-Za-z0-9_$]*)`)
	jsFunctionPattern = regexp.MustCompile(`(?i)^(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)
	jsClassPattern    = regexp.MustCompile(`(?i)^(?:export\s+)?(?:default\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)\b`)
	jsArrowPattern    = regexp.MustCompile(`(?i)^(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][A-Za-z0-9_$]*)\s*=>`)
	goFunctionPattern = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	goTypePattern     = regexp.MustCompile(`^type\s+([A-Za-z_][A-Za-z0-9_]*)\s+(?:struct|interface)\b`)
	goRoutePattern    = regexp.MustCompile(`["\x60](GET|POST|PUT|PATCH|DELETE|OPTIONS|HEAD)\s+(/[^"\x60]*)["\x60]`)
)

func (a *app) deterministicRepositoryLookup(ctx context.Context, message string) (deterministicRepositoryResult, bool, bool) {
	if !isSimpleRepositoryLocationQuestion(message) {
		return deterministicRepositoryResult{}, false, false
	}
	apps := a.resolveMentionedAppNames(ctx, message)
	if len(apps) != 1 {
		return deterministicRepositoryResult{}, false, false
	}
	return a.deterministicRepositoryLookupForApp(ctx, message, apps[0])
}

func (a *app) deterministicRepositoryLookupForApp(ctx context.Context, message, appName string) (deterministicRepositoryResult, bool, bool) {
	if !isSimpleRepositoryLocationQuestion(message) || appName == "" {
		return deterministicRepositoryResult{}, false, false
	}
	index, hit, err := a.repositoryIndex(ctx, appName)
	if err != nil {
		return deterministicRepositoryResult{}, hit, false
	}
	result, ok := lookupRepositoryIndex(index, message)
	return result, hit, ok
}

func (a *app) resolveMentionedAppNames(ctx context.Context, message string) []string {
	deployments, err := a.fetchDeployments(ctx)
	if err != nil {
		return nil
	}
	normalized := normalizeMatch(message)
	var names []string
	for _, deployment := range deployments {
		name, _ := deployment["app"].(string)
		if name != "" && strings.Contains(normalized, normalizeMatch(name)) {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	return names
}

func (a *app) repositoryIndex(ctx context.Context, appName string) (*repositoryIndex, bool, error) {
	version, files, err := a.repositoryVersion(appName)
	if err != nil {
		return nil, false, err
	}
	a.repoIndexMu.Lock()
	defer a.repoIndexMu.Unlock()
	if cached, ok := a.repoIndexes[appName]; ok && cached.version == version {
		return cached.index, true, nil
	}
	index, err := a.buildRepositoryIndex(ctx, appName, version, files)
	if err != nil {
		return nil, false, err
	}
	if a.repoIndexes == nil {
		a.repoIndexes = make(map[string]repositoryIndexCache)
	}
	a.repoIndexes[appName] = repositoryIndexCache{version: version, index: index}
	return index, false, nil
}

func (a *app) repositoryVersion(appName string) (string, []repositorySourceMetadata, error) {
	repo := a.readRepoContext(appName)
	if !repo.Exists {
		return "", nil, os.ErrNotExist
	}
	files, err := a.repositorySourceMetadata(appName)
	if err != nil {
		return "", nil, err
	}
	hash := sha256.New()
	for _, file := range files {
		fmt.Fprintf(hash, "%s%c%d%c%d%c", file.Path, 0, file.Size, 0, file.ModTime, 0)
	}
	return fmt.Sprintf("git:%s;metadata:%x", repo.Commit, hash.Sum(nil)), files, nil
}

func (a *app) repositorySourceMetadata(appName string) ([]repositorySourceMetadata, error) {
	directories := []string{"."}
	var files []repositorySourceMetadata
	for len(directories) > 0 && len(files) < repositoryIndexMaxFiles {
		directory := directories[0]
		directories = directories[1:]
		listing, err := a.listRepository(appName, directory)
		if err != nil {
			return nil, err
		}
		for _, entry := range listing.Entries {
			if entry.Type == "directory" {
				directories = append(directories, entry.Path)
				continue
			}
			if !isRepositoryIndexSource(entry.Path) {
				continue
			}
			_, target, _, err := a.resolveRepositoryPath(appName, entry.Path)
			if err != nil {
				continue
			}
			info, err := os.Stat(target)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			files = append(files, repositorySourceMetadata{Path: entry.Path, Size: info.Size(), ModTime: info.ModTime().UnixNano()})
			if len(files) >= repositoryIndexMaxFiles {
				break
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func isRepositoryIndexSource(path string) bool {
	if !looksLikeSourcePath(path) {
		return false
	}
	lower := strings.ToLower(path)
	for _, marker := range []string{"/test/", "/tests/", "_test.go", ".test.js", ".test.jsx", ".test.ts", ".test.tsx", "/migrations/"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

func (a *app) buildRepositoryIndex(ctx context.Context, appName, version string, files []repositorySourceMetadata) (*repositoryIndex, error) {
	if files == nil {
		var err error
		files, err = a.repositorySourceMetadata(appName)
		if err != nil {
			return nil, err
		}
	}
	index := &repositoryIndex{App: appName, Version: version, Entries: []repositoryIndexEntry{}, Files: []string{}}
	for _, metadata := range files {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		file, err := a.readRepositoryFile(appName, metadata.Path)
		if err != nil {
			continue
		}
		index.Files = append(index.Files, file.Path)
		index.Entries = append(index.Entries, extractRepositoryIndexEntries(file)...)
	}
	return index, nil
}

func extractRepositoryIndexEntries(file repoFileResponse) []repositoryIndexEntry {
	var entries []repositoryIndexEntry
	lowerPath := strings.ToLower(file.Path)
	for i, raw := range strings.Split(file.Content, "\n") {
		line, lineNo := strings.TrimSpace(raw), i+1
		add := func(kind, name, method, routePath, target string) {
			entries = append(entries, repositoryIndexEntry{
				Kind: kind, Name: name, Path: file.Path, Line: lineNo, Method: method,
				RoutePath: routePath, MountTarget: target,
			})
		}
		if strings.HasSuffix(lowerPath, ".js") || strings.HasSuffix(lowerPath, ".jsx") ||
			strings.HasSuffix(lowerPath, ".ts") || strings.HasSuffix(lowerPath, ".tsx") {
			if match := jsMountPattern.FindStringSubmatch(line); len(match) == 3 {
				add("mount", match[2], "", match[1], match[2])
			}
			if match := jsRoutePattern.FindStringSubmatch(line); len(match) == 3 {
				add("route", "", strings.ToUpper(match[1]), match[2], "")
			}
			for _, item := range []struct {
				kind string
				re   *regexp.Regexp
			}{{"function", jsFunctionPattern}, {"class", jsClassPattern}, {"function", jsArrowPattern}} {
				if match := item.re.FindStringSubmatch(line); len(match) == 2 {
					add(item.kind, match[1], "", "", "")
					break
				}
			}
		} else if strings.HasSuffix(lowerPath, ".go") {
			if match := goRoutePattern.FindStringSubmatch(line); len(match) == 3 {
				add("route", "", match[1], match[2], "")
			}
			if match := goFunctionPattern.FindStringSubmatch(line); len(match) == 2 {
				add("function", match[1], "", "", "")
			}
			if match := goTypePattern.FindStringSubmatch(line); len(match) == 2 {
				add("type", match[1], "", "", "")
			}
		}
	}
	return entries
}

func lookupRepositoryIndex(index *repositoryIndex, message string) (deterministicRepositoryResult, bool) {
	if index == nil {
		return deterministicRepositoryResult{}, false
	}
	lower := strings.ToLower(message)
	if messageContainsTerm(lower, "function") || messageContainsTerm(lower, "class") ||
		messageContainsTerm(lower, "symbol") || messageContainsTerm(lower, "defined") ||
		messageContainsTerm(lower, "definition") || messageContainsTerm(lower, "contains") {
		if result, ok := lookupRepositorySymbol(index, message); ok {
			return result, true
		}
	}
	if messageContainsTerm(lower, "route") || messageContainsTerm(lower, "endpoint") || messageContainsTerm(lower, "handles") {
		return lookupRepositoryRoute(index, message)
	}
	return deterministicRepositoryResult{}, false
}

func repositoryLookupTerms(message, appName string) []string {
	stop := map[string]bool{
		"which": true, "what": true, "where": true, "the": true, "is": true, "are": true,
		"in": true, "for": true, "of": true, "to": true, "a": true, "an": true,
		"backend": true, "frontend": true, "route": true, "routes": true, "endpoint": true,
		"endpoints": true, "handler": true, "handlers": true, "handles": true, "handle": true,
		"file": true, "files": true, "function": true, "class": true, "symbol": true,
		"defined": true, "definition": true, "contains": true, "located": true,
	}
	appToken := normalizeEvidenceTerm(appName)
	seen, out := map[string]bool{}, []string{}
	for _, part := range strings.FieldsFunc(strings.ToLower(message), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_')
	}) {
		normalized := normalizeLookupTerm(part)
		if normalized == "" || stop[part] || normalized == appToken || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	return out
}

func normalizeLookupTerm(value string) string {
	value = normalizeEvidenceTerm(value)
	if len(value) > 4 && strings.HasSuffix(value, "s") {
		value = strings.TrimSuffix(value, "s")
	}
	return value
}

func lookupRepositoryRoute(index *repositoryIndex, message string) (deterministicRepositoryResult, bool) {
	terms := repositoryLookupTerms(message, index.App)
	type candidate struct {
		path          string
		termCoverage  int
		exactCompound bool
	}
	byPath := map[string]map[string]bool{}
	for _, entry := range index.Entries {
		if entry.Kind != "route" {
			continue
		}
		if byPath[entry.Path] == nil {
			byPath[entry.Path] = map[string]bool{}
		}
		haystack := normalizeEvidenceTerm(entry.Path + " " + entry.RoutePath + " " + entry.MountTarget)
		for _, term := range terms {
			if strings.Contains(haystack, term) {
				byPath[entry.Path][term] = true
			}
		}
	}
	compound := strings.Join(terms, "")
	var ranked []candidate
	for path, covered := range byPath {
		if len(covered) == 0 {
			continue
		}
		ranked = append(ranked, candidate{
			path: path, termCoverage: len(covered),
			exactCompound: repositoryRouteKey(path) == compound,
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].termCoverage != ranked[j].termCoverage {
			return ranked[i].termCoverage > ranked[j].termCoverage
		}
		if ranked[i].exactCompound != ranked[j].exactCompound {
			return ranked[i].exactCompound
		}
		return ranked[i].path < ranked[j].path
	})
	if len(terms) == 0 || len(ranked) == 0 {
		return deterministicRepositoryResult{}, false
	}
	if len(ranked) > 1 &&
		ranked[0].termCoverage == ranked[1].termCoverage &&
		ranked[0].exactCompound == ranked[1].exactCompound {
		return deterministicRepositoryResult{}, false
	}
	routeFile := ranked[0].path
	var routes []repositoryIndexEntry
	for _, entry := range index.Entries {
		if entry.Kind == "route" && entry.Path == routeFile {
			routes = append(routes, entry)
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Line < routes[j].Line })
	if len(routes) == 0 {
		return deterministicRepositoryResult{}, false
	}
	evidence := []deterministicRepositoryEvidence{{routeFile, fmt.Sprintf("indexed route declarations at line %d", routes[0].Line), routes[0].Line}}
	base := normalizeEvidenceTerm(strings.TrimSuffix(filepath.Base(routeFile), filepath.Ext(routeFile)))
	var mount *repositoryIndexEntry
	for i := range index.Entries {
		entry := &index.Entries[i]
		if entry.Kind == "mount" && normalizeEvidenceTerm(entry.MountTarget) == base {
			if mount != nil {
				return deterministicRepositoryResult{}, false
			}
			mount = entry
		}
	}
	var methods []string
	for _, route := range routes {
		methods = append(methods, route.Method+" "+route.RoutePath)
	}
	answer := fmt.Sprintf("This route is handled in `%s`.\n\nThe router defines %s.", routeFile, joinCodeItems(methods))
	if mount != nil {
		answer += fmt.Sprintf(" `%s` mounts it at `%s`.", mount.Path, mount.RoutePath)
		evidence = append(evidence, deterministicRepositoryEvidence{mount.Path, fmt.Sprintf("indexed route mount at line %d", mount.Line), mount.Line})
	}
	return deterministicRepositoryResult{App: index.App, Answer: answer, Kind: "route", Evidence: evidence}, true
}

func repositoryRouteKey(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	key := normalizeEvidenceTerm(base)
	for _, suffix := range []string{"routes", "route", "controllers", "controller", "handlers", "handler"} {
		if strings.HasSuffix(key, suffix) {
			return strings.TrimSuffix(key, suffix)
		}
	}
	return key
}

func lookupRepositorySymbol(index *repositoryIndex, message string) (deterministicRepositoryResult, bool) {
	terms := repositoryLookupTerms(message, index.App)
	var matches []repositoryIndexEntry
	for _, entry := range index.Entries {
		if entry.Name == "" || (entry.Kind != "function" && entry.Kind != "class" && entry.Kind != "type") {
			continue
		}
		for _, term := range terms {
			if normalizeLookupTerm(entry.Name) == term {
				matches = append(matches, entry)
				break
			}
		}
	}
	if len(matches) != 1 {
		return deterministicRepositoryResult{}, false
	}
	match := matches[0]
	return deterministicRepositoryResult{
		App: index.App, Kind: "symbol",
		Answer: fmt.Sprintf("`%s` is defined in `%s` at line %d.", match.Name, match.Path, match.Line),
		Evidence: []deterministicRepositoryEvidence{{
			Path: match.Path, Line: match.Line, Summary: fmt.Sprintf("indexed %s definition at line %d", match.Kind, match.Line),
		}},
	}, true
}

func joinCodeItems(items []string) string {
	if len(items) > 8 {
		items = items[:8]
	}
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = "`" + item + "`"
	}
	if len(quoted) == 1 {
		return quoted[0]
	}
	if len(quoted) == 2 {
		return quoted[0] + " and " + quoted[1]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + ", and " + quoted[len(quoted)-1]
}

func (a *app) handleDeterministicRepositoryLookup(w http.ResponseWriter, r *http.Request, message string) bool {
	result, indexHit, ok := a.deterministicRepositoryLookup(r.Context(), message)
	return emitDeterministicRepositoryResult(w, result, indexHit, ok)
}

func (a *app) handleConversationRepositoryLookup(w http.ResponseWriter, r *http.Request, message string, history []storedMessage) bool {
	if !isSimpleRepositoryLocationQuestion(message) {
		return false
	}

	subject := a.resolveConversationSubject(r.Context(), message, history)
	if subject.App == "" || subject.Ambiguous {
		return a.handleDeterministicRepositoryLookup(w, r, message)
	}

	result, indexHit, ok := a.deterministicRepositoryLookupForApp(r.Context(), message, subject.App)
	return emitDeterministicRepositoryResult(w, result, indexHit, ok)
}

func emitDeterministicRepositoryResult(w http.ResponseWriter, result deterministicRepositoryResult, indexHit, ok bool) bool {
	if !ok {
		return false
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	sendSSE(w, "meta", map[string]any{
		"answer_mode": "deterministic", "model_invoked": false, "index_hit": indexHit,
		"evidence_files": len(result.Evidence),
	})
	flusher.Flush()
	for _, evidence := range result.Evidence {
		args := map[string]any{"app": result.App, "path": evidence.Path}
		sendSSE(w, "tool", agentToolEvent{Phase: "start", Name: "read_repository_file", Arguments: args, Summary: "repository index evidence"})
		sendSSE(w, "tool", agentToolEvent{Phase: "result", Name: "read_repository_file", Summary: evidence.Summary})
		flusher.Flush()
	}
	emitBufferedAnswer(w, flusher, result.Answer)
	sendSSE(w, "done", map[string]any{
		"answer_mode": "deterministic", "model_invoked": false, "index_hit": indexHit,
		"evidence_files": len(result.Evidence), "tool_calls": 0, "planner_calls": 0, "tokens": 0,
	})
	flusher.Flush()
	return true
}
