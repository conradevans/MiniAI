package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// These are the compatibility-only generic-agent entry points: the generic
// planner loop plus both tool-schema planner transports. They may remain
// compiled, but no production chat root may reach any of them.
var legacyGenericPlannerSinks = map[string]bool{
	"handleAgentChatStream":         true,
	"callAgentPlanner":              true,
	"callAgentPlannerWithKeepalive": true,
}

func TestProductionSourcesDoNotReachLegacyGenericPlanner(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := []*ast.File{}
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}

	graph := buildIntraPackageCallGraph(files)
	for _, root := range []string{"handleChatStream", "handleSelectedModelChatStream"} {
		if path := callPathToSink(graph, root, legacyGenericPlannerSinks); len(path) > 0 {
			t.Fatalf("production root reaches retired generic planner: %s", strings.Join(path, " -> "))
		}
	}
}

func TestLegacyPlannerCallGraphDetectsWrapperBypass(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", `package main
func handleChatStream() { productionWrapper() }
func productionWrapper() { compatibilityWrapper() }
func compatibilityWrapper() { handleAgentChatStream() }
func handleAgentChatStream() {}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	path := callPathToSink(buildIntraPackageCallGraph([]*ast.File{file}), "handleChatStream", legacyGenericPlannerSinks)
	want := []string{"handleChatStream", "productionWrapper", "compatibilityWrapper", "handleAgentChatStream"}
	if strings.Join(path, " -> ") != strings.Join(want, " -> ") {
		t.Fatalf("wrapper bypass path=%v want=%v", path, want)
	}
}

func buildIntraPackageCallGraph(files []*ast.File) map[string]map[string]bool {
	graph := map[string]map[string]bool{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			calls := graph[function.Name.Name]
			if calls == nil {
				calls = map[string]bool{}
				graph[function.Name.Name] = calls
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch called := call.Fun.(type) {
				case *ast.Ident:
					calls[called.Name] = true
				case *ast.SelectorExpr:
					calls[called.Sel.Name] = true
				}
				return true
			})
		}
	}
	return graph
}

func callPathToSink(graph map[string]map[string]bool, root string, sinks map[string]bool) []string {
	paths := [][]string{{root}}
	seen := map[string]bool{root: true}
	for len(paths) > 0 {
		path := paths[0]
		paths = paths[1:]
		current := path[len(path)-1]
		if sinks[current] {
			return path
		}
		callees := make([]string, 0, len(graph[current]))
		for callee := range graph[current] {
			callees = append(callees, callee)
		}
		sort.Strings(callees)
		for _, callee := range callees {
			if seen[callee] {
				continue
			}
			seen[callee] = true
			next := append(append([]string(nil), path...), callee)
			if sinks[callee] {
				return next
			}
			if _, internal := graph[callee]; internal {
				paths = append(paths, next)
			}
		}
	}
	return nil
}
