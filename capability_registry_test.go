package main

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestPhase1CCapabilityRegistryCompleteStableReadOnlySet(t *testing.T) {
	registry := phase0CapabilityRegistry()
	wantOrder := []string{
		"get_platform_overview",
		"list_apps",
		"get_app_context",
		"read_host_history",
		"read_temperature_history",
		"read_application_history",
		"read_service_history",
		"read_infrastructure_events",
		"list_databases",
		"read_database_backups",
		"read_activity",
		"read_recovery",
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
			t.Fatalf(
				"capability %q kind=%q undo=%q want read/none",
				item.Definition.Function.Name,
				item.Kind,
				item.Undo,
			)
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

func TestPhase1CToolSchemasStaySimpleAndAccurate(t *testing.T) {
	definitions := phase0CapabilityRegistry().toolDefinitions()
	byName := map[string]toolDefinitionBody{}
	for _, definition := range definitions {
		if definition.Type != "function" {
			t.Fatalf("tool %q type=%q", definition.Function.Name, definition.Type)
		}
		parameters := definition.Function.Parameters
		if parameters["type"] != "object" ||
			parameters["additionalProperties"] != false {

			t.Fatalf("tool %q schema=%#v", definition.Function.Name, parameters)
		}
		if strings.Contains(
			strings.ToLower(definition.Function.Description),
			"contains no exact git commit",
		) {
			t.Fatalf("tool %q retains obsolete history wording", definition.Function.Name)
		}
		byName[definition.Function.Name] = definition.Function
	}

	for _, name := range []string{
		"read_host_history",
		"read_temperature_history",
		"read_application_history",
		"read_service_history",
		"read_infrastructure_events",
	} {
		properties, ok := byName[name].Parameters["properties"].(map[string]any)
		if !ok {
			t.Fatalf("tool %q properties=%#v", name, byName[name].Parameters)
		}
		for _, key := range []string{"range", "from", "to"} {
			property, ok := properties[key].(map[string]any)
			if !ok || property["type"] != "string" {
				t.Fatalf("tool %q property %q=%#v", name, key, properties[key])
			}
		}
	}

	for name, maximum := range map[string]string{
		"read_infrastructure_events": "500",
		"read_database_backups":      "200",
		"read_activity":              "200",
	} {
		properties := byName[name].Parameters["properties"].(map[string]any)
		limit := properties["limit"].(map[string]any)
		if limit["type"] != "integer" ||
			!strings.Contains(limit["description"].(string), maximum) {

			t.Fatalf("tool %q limit schema=%#v", name, limit)
		}
	}
}

func TestPhase1CReactorLabSchemasExposeOnlyFixedSelectors(t *testing.T) {
	wantProperties := map[string][]string{
		"get_platform_overview":      {},
		"list_apps":                  {},
		"get_app_context":            {"app"},
		"read_host_history":          {"from", "range", "to"},
		"read_temperature_history":   {"from", "range", "to"},
		"read_application_history":   {"app", "from", "range", "to"},
		"read_service_history":       {"from", "range", "service", "to"},
		"read_infrastructure_events": {"from", "limit", "range", "to"},
		"list_databases":             {},
		"read_database_backups":      {"database_id", "limit"},
		"read_activity":              {"limit"},
		"read_recovery":              {},
		"read_deployment_history":    {"app"},
	}
	seen := map[string]bool{}
	for _, definition := range phase0CapabilityRegistry().toolDefinitions() {
		want, relevant := wantProperties[definition.Function.Name]
		if !relevant {
			continue
		}
		seen[definition.Function.Name] = true
		properties := definition.Function.Parameters["properties"].(map[string]any)
		got := make([]string, 0, len(properties))
		for name := range properties {
			got = append(got, name)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("tool %q properties=%v want %v", definition.Function.Name, got, want)
		}
	}
	if len(seen) != len(wantProperties) {
		t.Fatalf("checked %d fixed ReactorLab tools, want %d", len(seen), len(wantProperties))
	}
}

func TestCapabilityExecutorRejectsUnknownTool(t *testing.T) {
	result, summary, err := newCapabilityExecutor(&app{}).Execute(
		context.Background(),
		"write_repository_file",
		nil,
	)
	if err == nil ||
		!strings.Contains(err.Error(), `unknown tool "write_repository_file"`) {

		t.Fatalf("err=%v", err)
	}
	if result != nil || summary != "" {
		t.Fatalf("result=%v summary=%q want nil/empty", result, summary)
	}
}
