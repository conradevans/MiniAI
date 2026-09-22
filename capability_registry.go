package main

import "context"

type capabilityKind string

const (
	capabilityKindRead   capabilityKind = "read"
	capabilityKindAction capabilityKind = "action"
)

type undoMode string

const (
	undoModeNone          undoMode = "none"
	undoModeReversible    undoMode = "reversible"
	undoModeRecoverable   undoMode = "recoverable"
	undoModeCompensatable undoMode = "compensatable"
	undoModeIrreversible  undoMode = "irreversible"
)

type capabilityHandler func(context.Context, *app, map[string]any) (any, string, error)

type capability struct {
	Kind       capabilityKind
	Undo       undoMode
	Definition toolDefinition
	handler    capabilityHandler
}

type capabilityRegistry struct {
	capabilities []capability
}

func phase0CapabilityRegistry() capabilityRegistry {
	stringProp := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	intProp := func(description string) map[string]any {
		return map[string]any{"type": "integer", "description": description}
	}
	obj := func(required []string, properties map[string]any) map[string]any {
		return map[string]any{
			"type":                 "object",
			"required":             required,
			"properties":           properties,
			"additionalProperties": false,
		}
	}
	readCapability := func(definition toolDefinition, handler capabilityHandler) capability {
		return capability{
			Kind:       capabilityKindRead,
			Undo:       undoModeNone,
			Definition: definition,
			handler:    handler,
		}
	}
	windowProperties := func() map[string]any {
		return map[string]any{
			"range": stringProp("Optional named window: 15m, 1h, 6h, 24h, or 7d. Do not combine with from/to."),
			"from":  stringProp("Optional RFC3339 start timestamp. Must be supplied together with to."),
			"to":    stringProp("Optional RFC3339 end timestamp. Must be supplied together with from."),
		}
	}

	return capabilityRegistry{capabilities: []capability{
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "get_platform_overview",
			Description: "Preferred first read for broad Dell health questions. Returns current system, recovery, deployment, database, and service state with partial availability preserved.",
			Parameters:  obj(nil, map[string]any{}),
		}}, executeGetPlatformOverviewCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "list_apps",
			Description: "List current deployments with exact deployed source identity when known, immutable image identity, database relationships, and separate local repository evidence.",
			Parameters:  obj(nil, map[string]any{}),
		}}, executeListAppsCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "get_app_context",
			Description: "Get current typed deployment, exact deployed source when known, attached databases, Dell overview, recovery state, and separate local repository evidence.",
			Parameters: obj([]string{"app"}, map[string]any{
				"app": stringProp("Deployment application name, for example myscheduler."),
			}),
		}}, executeGetAppContextCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "read_host_history",
			Description: "Read bounded historical Dell CPU, memory, load, disk, disk I/O, and network measurements. Correlation does not prove causation.",
			Parameters:  obj(nil, windowProperties()),
		}}, executeReadHostHistoryCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "read_temperature_history",
			Description: "Read bounded historical temperature minimum, average, maximum, peak time, and sample counts.",
			Parameters:  obj(nil, windowProperties()),
		}}, executeReadTemperatureHistoryCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "read_application_history",
			Description: "Resolve a deployed app to an observability application and read bounded historical CPU, memory, network, status, and restart-count evidence.",
			Parameters: obj([]string{"app"}, mergeToolProperties(
				windowProperties(),
				map[string]any{"app": stringProp("Deployed application name to resolve safely by observability ID or name.")},
			)),
		}}, executeReadApplicationHistoryCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "read_service_history",
			Description: "Read bounded historical platform-service availability. An optional service filters by an exact case-insensitive returned ID or name.",
			Parameters: obj(nil, mergeToolProperties(
				windowProperties(),
				map[string]any{"service": stringProp("Optional returned service ID or name. It is never used as a path.")},
			)),
		}}, executeReadServiceHistoryCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "read_infrastructure_events",
			Description: "Read bounded newest-first infrastructure events for a named or explicit historical window.",
			Parameters: obj(nil, mergeToolProperties(
				windowProperties(),
				map[string]any{"limit": intProp("Optional event limit from 1 through 500; defaults to 100.")},
			)),
		}}, executeReadInfrastructureEventsCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "list_databases",
			Description: "List safe current database metrics, cumulative transaction/cache/row counters, backup summaries, and deployment relationships.",
			Parameters:  obj(nil, map[string]any{}),
		}}, executeListDatabasesCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "read_database_backups",
			Description: "Read a bounded newest-first safe backup inventory for one database.",
			Parameters: obj([]string{"database_id"}, map[string]any{
				"database_id": stringProp("Canonical database resource ID."),
				"limit":       intProp("Optional backup limit from 1 through 200; defaults to 100."),
			}),
		}}, executeReadDatabaseBackupsCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "read_activity",
			Description: "Read bounded newest-first recent platform and database Activity. This is not permanent historical retention.",
			Parameters: obj(nil, map[string]any{
				"limit": intProp("Optional Activity limit from 1 through 200; defaults to 100."),
			}),
		}}, executeReadActivityCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "read_recovery",
			Description: "Read current recovery protection, hardware watchdog and RTC state, plus bounded recovery incidents.",
			Parameters:  obj(nil, map[string]any{}),
		}}, executeReadRecoveryCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "list_repository_directory",
			Description: "List a safe directory inside an application's repository. Secret/noise paths are blocked.",
			Parameters: obj([]string{"app", "path"}, map[string]any{
				"app":  stringProp("Application name."),
				"path": stringProp("Repository-relative directory path, or . for repository root."),
			}),
		}}, executeListRepositoryDirectoryCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "search_repository",
			Description: "Search safe text files in an application's repository. Use this before reading files when locating implementation code.",
			Parameters: obj([]string{"app", "query"}, map[string]any{
				"app":   stringProp("Application name."),
				"query": stringProp("Literal text to search for."),
				"path":  stringProp("Optional repository-relative directory path; defaults to ."),
			}),
		}}, executeSearchRepositoryCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "read_repository_file",
			Description: "Read one safe text file from an application's repository. Secret files, binaries, symlinks, traversal, and oversized files are blocked.",
			Parameters: obj([]string{"app", "path"}, map[string]any{
				"app":  stringProp("Application name."),
				"path": stringProp("Repository-relative source file path."),
			}),
		}}, executeReadRepositoryFileCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "read_runtime_logs",
			Description: "Read bounded recent runtime logs through MiniDeploy's private redacted log API. Use only when runtime evidence is relevant.",
			Parameters: obj([]string{"app"}, map[string]any{
				"app":   stringProp("Application name."),
				"lines": intProp("Optional number of recent lines, 1-200. Prefer 40-80."),
			}),
		}}, executeReadRuntimeLogsCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "read_deployment_logs",
			Description: "Read bounded deployment/build lifecycle logs through MiniDeploy's private redacted log API.",
			Parameters: obj([]string{"app"}, map[string]any{
				"app":   stringProp("Application name."),
				"lines": intProp("Optional number of recent lines, 1-200. Prefer 40-80."),
			}),
		}}, executeReadDeploymentLogsCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "read_deployment_history",
			Description: "Read bounded deployment rollback history from ReactorLab. Exact source commits and activation times are preserved when known; archivedAt is archive-entry time, not original activation time.",
			Parameters: obj([]string{"app"}, map[string]any{
				"app": stringProp("Canonical deployed application name."),
			}),
		}}, executeReadDeploymentHistoryCapability),
	}}
}

func mergeToolProperties(
	left map[string]any,
	right map[string]any,
) map[string]any {
	merged := make(map[string]any, len(left)+len(right))
	for key, value := range left {
		merged[key] = value
	}
	for key, value := range right {
		merged[key] = value
	}
	return merged
}

func (r capabilityRegistry) toolDefinitions() []toolDefinition {
	definitions := make([]toolDefinition, 0, len(r.capabilities))
	for _, item := range r.capabilities {
		definitions = append(definitions, item.Definition)
	}
	return definitions
}

func (r capabilityRegistry) lookup(name string) (capability, bool) {
	for _, item := range r.capabilities {
		if item.Definition.Function.Name == name {
			return item, true
		}
	}
	return capability{}, false
}

func agentToolDefinitions() []toolDefinition {
	return phase0CapabilityRegistry().toolDefinitions()
}
