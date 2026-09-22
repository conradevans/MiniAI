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
	deployments, err := a.reactorLabReads().Deployments(ctx)
	if err != nil {
		return nil, "", err
	}
	sort.Slice(deployments.Deployments, func(i, j int) bool {
		return deployments.Deployments[i].App <
			deployments.Deployments[j].App
	})
	total := len(deployments.Deployments)
	truncated := total > reactorLabMaxToolApps
	if truncated {
		deployments.Deployments =
			deployments.Deployments[:reactorLabMaxToolApps]
	}
	out := make([]reactorLabAppSummary, 0, len(deployments.Deployments))
	for _, deployment := range deployments.Deployments {
		out = append(out, reactorLabAppSummary{
			App:             deployment.App,
			Strategy:        deployment.Strategy,
			Status:          deployment.Status,
			SourceAvailable: deployment.Source != nil,
			Source:          deployment.Source,
			ActivatedAt:     deployment.ActivatedAt,
			ImageID:         deployment.ImageID,
			Services:        deployment.Services,
			Databases:       deployment.Databases,
			LocalRepository: localRepositoryEvidenceFrom(
				a.readRepoContext(deployment.App),
			),
		})
	}
	return reactorLabAppListResult{
		Apps: out, TotalApps: total, Truncated: truncated,
	}, fmt.Sprintf("listed %d deployed apps", len(out)), nil
}

func executeGetAppContextCapability(ctx context.Context, a *app, args map[string]any) (any, string, error) {
	appName := stringArg(args, "app")
	if appName == "" {
		return nil, "", fmt.Errorf("app is required")
	}
	out, err := a.readReactorLabAppContext(ctx, appName)
	if err != nil {
		return nil, "", err
	}
	return out, "resolved current context for " + out.App, nil
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
	canonical, err := canonicalReactorLabAppName(
		ctx,
		a.reactorLabReads(),
		appName,
	)
	if err != nil {
		return nil, "", err
	}
	out, err := a.reactorLabReads().DeploymentHistory(ctx, canonical)
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf(
		"read %d bounded previous deployment versions",
		len(out.Versions),
	), nil
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
