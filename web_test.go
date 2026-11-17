package main

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestMiniAIWebRoutes(t *testing.T) {
	a := &app{}
	mux := http.NewServeMux()
	a.registerWebRoutes(mux)

	for _, path := range []string{"/", "/admin", "/admin/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, rec.Code)
		}
		body := rec.Body.String()
		for _, required := range []string{"MiniAI", "New chat", "Message MiniAI"} {
			if !strings.Contains(body, required) {
				t.Fatalf("%s missing %q", path, required)
			}
		}
	}
}

func TestMiniAIWebAssets(t *testing.T) {
	a := &app{}
	mux := http.NewServeMux()
	a.registerWebRoutes(mux)

	for _, path := range []string{"/assets/styles.css", "/assets/action_controls.js", "/assets/app.js", "/favicon.svg"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s empty", path)
		}
	}
}

func TestMiniAIActionControlBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	command := exec.Command(node, "--test", "frontend_action_controls_test.js")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("frontend action control tests failed: %v\n%s", err, output)
	}
}
