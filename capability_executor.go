package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type capabilityExecutor interface {
	Execute(context.Context, string, map[string]any) (any, string, error)
}

type registryCapabilityExecutor struct {
	app      *app
	registry capabilityRegistry
}

func newCapabilityExecutor(a *app) capabilityExecutor {
	return registryCapabilityExecutor{app: a, registry: phase0CapabilityRegistry()}
}

func (e registryCapabilityExecutor) Execute(ctx context.Context, name string, args map[string]any) (any, string, error) {
	item, ok := e.registry.lookup(name)
	if !ok || item.handler == nil {
		return nil, "", fmt.Errorf("unknown tool %q", name)
	}
	return item.handler(ctx, e.app, args)
}

// executeAgentTool remains as a compatibility seam for callers and tests while
// orchestration depends on the capabilityExecutor boundary directly.
func (a *app) executeAgentTool(ctx context.Context, name string, args map[string]any) (any, string, error) {
	return newCapabilityExecutor(a).Execute(ctx, name, args)
}

func executeListAppsCapability(ctx context.Context, a *app, _ map[string]any) (any, string, error) {
	deployments, err := a.fetchDeployments(ctx)
	if err != nil {
		return nil, "", err
	}
	out := make([]appSummary, 0, len(deployments))
	for _, d := range deployments {
		appName, _ := d["app"].(string)
		if appName == "" {
			continue
		}
		status, _ := d["status"].(string)
		dbName := ""
		if db, ok := d["database"].(map[string]any); ok {
			dbName, _ = db["displayName"].(string)
		}
		repo := a.readRepoContext(appName)
		out = append(out, appSummary{App: appName, Status: status, Database: dbName, RepositoryPath: repo.Path, RepositoryFound: repo.Exists})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return map[string]any{"apps": out}, fmt.Sprintf("listed %d deployed apps", len(out)), nil
}

func executeGetAppContextCapability(ctx context.Context, a *app, args map[string]any) (any, string, error) {
	appName := stringArg(args, "app")
	if appName == "" {
		return nil, "", fmt.Errorf("app is required")
	}
	out, err := a.resolveAppContext(ctx, appName)
	if err != nil {
		return nil, "", err
	}
	return compactModelContext(out), "resolved current context for " + appName, nil
}

func executeListRepositoryDirectoryCapability(_ context.Context, a *app, args map[string]any) (any, string, error) {
	appName := stringArg(args, "app")
	path := stringArg(args, "path")
	if path == "" {
		path = "."
	}
	out, err := a.listRepository(appName, path)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("listed %d entries under %s", len(out.Entries), out.Path), nil
}

func executeSearchRepositoryCapability(ctx context.Context, a *app, args map[string]any) (any, string, error) {
	appName := stringArg(args, "app")
	path := stringArg(args, "path")
	query := stringArg(args, "query")
	if query == "" {
		return nil, "", fmt.Errorf("query is required")
	}
	if path == "" {
		path = "."
	}
	out, err := a.searchRepository(ctx, appName, path, query)
	if err != nil {
		return nil, "", err
	}
	files := map[string]struct{}{}
	for _, hit := range out.Hits {
		files[hit.Path] = struct{}{}
	}
	return out, fmt.Sprintf("found %d matches across %d files", len(out.Hits), len(files)), nil
}

func executeReadRepositoryFileCapability(_ context.Context, a *app, args map[string]any) (any, string, error) {
	appName := stringArg(args, "app")
	path := stringArg(args, "path")
	if path == "" {
		return nil, "", fmt.Errorf("path is required")
	}
	out, err := a.readRepositoryFile(appName, path)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("read %s (%d bytes)", out.Path, out.SizeBytes), nil
}

func executeReadRuntimeLogsCapability(ctx context.Context, a *app, args map[string]any) (any, string, error) {
	appName := stringArg(args, "app")
	lines := boundedLogLines(args)
	out, err := a.readMiniDeployLogs(ctx, appName, "runtime", lines)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("read %d bounded runtime log lines", out.Lines), nil
}

func executeReadDeploymentLogsCapability(ctx context.Context, a *app, args map[string]any) (any, string, error) {
	appName := stringArg(args, "app")
	lines := boundedLogLines(args)
	out, err := a.readMiniDeployLogs(ctx, appName, "deployment", lines)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("read %d bounded deployment log lines", out.Lines), nil
}

func executeReadDeploymentHistoryCapability(ctx context.Context, a *app, args map[string]any) (any, string, error) {
	appName := stringArg(args, "app")
	if appName == "" {
		return nil, "", fmt.Errorf("app is required")
	}
	out, err := a.readMiniDeployDeploymentHistory(ctx, appName)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("read %d bounded previous deployment versions", len(out.Versions)), nil
}

func boundedLogLines(args map[string]any) int {
	lines := intArg(args, "lines", logDefaultLines)
	if lines < 1 || lines > logMaxLines {
		return logDefaultLines
	}
	return lines
}

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

func intArg(args map[string]any, key string, fallback int) int {
	if args == nil {
		return fallback
	}
	switch value := args[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case json.Number:
		n, err := value.Int64()
		if err == nil {
			return int(n)
		}
	}
	return fallback
}
