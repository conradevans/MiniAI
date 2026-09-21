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

	return capabilityRegistry{capabilities: []capability{
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "list_apps",
			Description: "List deployed applications and their high-level health/database/repository linkage.",
			Parameters:  obj(nil, map[string]any{}),
		}}, executeListAppsCapability),
		readCapability(toolDefinition{Type: "function", Function: toolDefinitionBody{
			Name:        "get_app_context",
			Description: "Get current read-only app context: deployment, database, Dell resource usage, recent activity, and repository identity.",
			Parameters: obj([]string{"app"}, map[string]any{
				"app": stringProp("Deployment application name, for example myscheduler."),
			}),
		}}, executeGetAppContextCapability),
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
			Description: "Read bounded previous deployment metadata from MiniDeploy. History is newest-first, excludes the active version, contains no exact Git commit, and its archive timestamp is not the original deployment time.",
			Parameters: obj([]string{"app"}, map[string]any{
				"app": stringProp("Canonical deployed application name."),
			}),
		}}, executeReadDeploymentHistoryCapability),
	}}
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
