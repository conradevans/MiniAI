package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestPhase0CapabilityRegistryCompleteStableReadOnlySet(t *testing.T) {
	registry := phase0CapabilityRegistry()
	wantOrder := []string{
		"list_apps",
		"get_app_context",
		"list_repository_directory",
		"search_repository",
		"read_repository_file",
		"read_runtime_logs",
		"read_deployment_logs",
		"read_deployment_history",
	}
	if len(registry.capabilities) != len(wantOrder) {
		t.Fatalf("capabilities=%d want %d", len(registry.capabilities), len(wantOrder))
	}
	actionCount := 0
	for index, item := range registry.capabilities {
		if got := item.Definition.Function.Name; got != wantOrder[index] {
			t.Fatalf("capability %d=%q want %q", index, got, wantOrder[index])
		}
		if item.Kind != capabilityKindRead || item.Undo != undoModeNone {
			t.Fatalf("capability %q kind=%q undo=%q want read/none", item.Definition.Function.Name, item.Kind, item.Undo)
		}
		if item.Kind == capabilityKindAction {
			actionCount++
		}
	}
	if actionCount != 0 {
		t.Fatalf("registered action capabilities=%d want 0", actionCount)
	}

	definitions := registry.toolDefinitions()
	gotOrder := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		gotOrder = append(gotOrder, definition.Function.Name)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("tool definition order=%v want %v", gotOrder, wantOrder)
	}
}

func TestPhase0CapabilityRegistryPreservesToolDefinitions(t *testing.T) {
	type expectedProperty struct {
		typeName    string
		description string
	}
	type expectedDefinition struct {
		name        string
		description string
		required    []string
		properties  map[string]expectedProperty
	}
	expected := []expectedDefinition{
		{
			name:        "list_apps",
			description: "List deployed applications and their high-level health/database/repository linkage.",
			properties:  map[string]expectedProperty{},
		},
		{
			name:        "get_app_context",
			description: "Get current read-only app context: deployment, database, Dell resource usage, recent activity, and repository identity.",
			required:    []string{"app"},
			properties: map[string]expectedProperty{
				"app": {typeName: "string", description: "Deployment application name, for example myscheduler."},
			},
		},
		{
			name:        "list_repository_directory",
			description: "List a safe directory inside an application's repository. Secret/noise paths are blocked.",
			required:    []string{"app", "path"},
			properties: map[string]expectedProperty{
				"app":  {typeName: "string", description: "Application name."},
				"path": {typeName: "string", description: "Repository-relative directory path, or . for repository root."},
			},
		},
		{
			name:        "search_repository",
			description: "Search safe text files in an application's repository. Use this before reading files when locating implementation code.",
			required:    []string{"app", "query"},
			properties: map[string]expectedProperty{
				"app":   {typeName: "string", description: "Application name."},
				"query": {typeName: "string", description: "Literal text to search for."},
				"path":  {typeName: "string", description: "Optional repository-relative directory path; defaults to ."},
			},
		},
		{
			name:        "read_repository_file",
			description: "Read one safe text file from an application's repository. Secret files, binaries, symlinks, traversal, and oversized files are blocked.",
			required:    []string{"app", "path"},
			properties: map[string]expectedProperty{
				"app":  {typeName: "string", description: "Application name."},
				"path": {typeName: "string", description: "Repository-relative source file path."},
			},
		},
		{
			name:        "read_runtime_logs",
			description: "Read bounded recent runtime logs through MiniDeploy's private redacted log API. Use only when runtime evidence is relevant.",
			required:    []string{"app"},
			properties: map[string]expectedProperty{
				"app":   {typeName: "string", description: "Application name."},
				"lines": {typeName: "integer", description: "Optional number of recent lines, 1-200. Prefer 40-80."},
			},
		},
		{
			name:        "read_deployment_logs",
			description: "Read bounded deployment/build lifecycle logs through MiniDeploy's private redacted log API.",
			required:    []string{"app"},
			properties: map[string]expectedProperty{
				"app":   {typeName: "string", description: "Application name."},
				"lines": {typeName: "integer", description: "Optional number of recent lines, 1-200. Prefer 40-80."},
			},
		},
		{
			name:        "read_deployment_history",
			description: "Read bounded previous deployment metadata from MiniDeploy. History is newest-first, excludes the active version, contains no exact Git commit, and its archive timestamp is not the original deployment time.",
			required:    []string{"app"},
			properties: map[string]expectedProperty{
				"app": {typeName: "string", description: "Canonical deployed application name."},
			},
		},
	}

	definitions := agentToolDefinitions()
	if len(definitions) != len(expected) {
		t.Fatalf("definitions=%d want %d", len(definitions), len(expected))
	}
	for index, want := range expected {
		got := definitions[index]
		if got.Type != "function" || got.Function.Name != want.name || got.Function.Description != want.description {
			t.Fatalf("definition %d identity mismatch: %+v", index, got)
		}
		parameters := got.Function.Parameters
		if parameters["type"] != "object" || parameters["additionalProperties"] != false {
			t.Fatalf("definition %q object schema=%v", want.name, parameters)
		}
		if required, ok := parameters["required"].([]string); !ok || !reflect.DeepEqual(required, want.required) {
			t.Fatalf("definition %q required=%#v want %#v", want.name, parameters["required"], want.required)
		}
		properties, ok := parameters["properties"].(map[string]any)
		if !ok || len(properties) != len(want.properties) {
			t.Fatalf("definition %q properties=%#v", want.name, parameters["properties"])
		}
		for propertyName, wantProperty := range want.properties {
			property, ok := properties[propertyName].(map[string]any)
			if !ok || !reflect.DeepEqual(property, map[string]any{
				"type": wantProperty.typeName, "description": wantProperty.description,
			}) {
				t.Fatalf("definition %q property %q=%#v", want.name, propertyName, properties[propertyName])
			}
		}
	}
}

func TestCapabilityExecutorRejectsUnknownTool(t *testing.T) {
	result, summary, err := newCapabilityExecutor(&app{}).Execute(context.Background(), "write_repository_file", nil)
	if err == nil || !strings.Contains(err.Error(), `unknown tool "write_repository_file"`) {
		t.Fatalf("err=%v", err)
	}
	if result != nil || summary != "" {
		t.Fatalf("result=%v summary=%q want nil/empty", result, summary)
	}
}
