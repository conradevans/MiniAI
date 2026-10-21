package main

import (
	"crypto"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

func TestAdminBoundaryAuthenticationAndTrustedPrincipal(t *testing.T) {
	boundary, rawToken := cryptographicAdminTestBoundary(t)
	handler := adminMiddlewareTestHandler(boundary)

	t.Run("valid signed administrator assertion allowed", func(t *testing.T) {
		response := adminMiddlewareRequest(
			handler,
			http.MethodGet,
			testAdminOrigin+"/api/admin/probe",
			rawToken,
			nil,
		)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if strings.TrimSpace(response.Body.String()) != testAdminEmail {
			t.Fatalf("principal body = %q", response.Body.String())
		}
	})

	t.Run("missing assertion rejected", func(t *testing.T) {
		response := adminMiddlewareRequest(
			handler, http.MethodGet, testAdminOrigin+"/api/admin/probe", "", nil,
		)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d; want %d", response.Code, http.StatusUnauthorized)
		}
	})

	t.Run("malformed assertion rejected safely", func(t *testing.T) {
		const rawToken = "raw-secret-token-value"
		response := adminMiddlewareRequest(
			handler, http.MethodGet, testAdminOrigin+"/api/admin/probe", rawToken, nil,
		)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d; want %d", response.Code, http.StatusForbidden)
		}
		if strings.Contains(response.Body.String(), rawToken) {
			t.Fatalf("authentication response exposed raw assertion: %s", response.Body.String())
		}
	})

	t.Run("loopback is not authorization", func(t *testing.T) {
		response := adminMiddlewareRequest(
			handler, http.MethodGet, "http://127.0.0.1:9300/api/admin/probe", "", nil,
		)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d; want %d", response.Code, http.StatusUnauthorized)
		}
	})

	t.Run("unsigned identity headers are not authorization", func(t *testing.T) {
		response := adminMiddlewareRequest(
			handler,
			http.MethodGet,
			testAdminOrigin+"/api/admin/probe",
			"",
			map[string][]string{
				"Cf-Access-Authenticated-User-Email": {testAdminEmail},
				"X-Forwarded-Email":                  {testAdminEmail},
			},
		)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d; want %d", response.Code, http.StatusUnauthorized)
		}
	})

	t.Run("verified assertion owns identity", func(t *testing.T) {
		response := adminMiddlewareRequest(
			handler,
			http.MethodGet,
			testAdminOrigin+"/api/admin/probe",
			rawToken,
			map[string][]string{
				"Cf-Access-Authenticated-User-Email": {"attacker@example.com"},
				"X-Forwarded-Email":                  {"attacker@example.com"},
			},
		)
		if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != testAdminEmail {
			t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
		}
	})
}

func TestAdminBoundaryUnsafeSameOriginProtection(t *testing.T) {
	boundary, rawToken := cryptographicAdminTestBoundary(t)
	handler := adminMiddlewareTestHandler(boundary)

	tests := []struct {
		name       string
		headers    map[string][]string
		wantStatus int
	}{
		{
			name: "trusted exact origin",
			headers: map[string][]string{
				"Origin":         {testAdminOrigin},
				"Sec-Fetch-Site": {"same-origin"},
			},
			wantStatus: http.StatusNoContent,
		},
		{
			name: "cross-site origin",
			headers: map[string][]string{
				"Origin": {"https://attacker.example"},
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "lookalike origin",
			headers: map[string][]string{
				"Origin": {testAdminOrigin + ".attacker.example"},
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "malformed origin",
			headers: map[string][]string{
				"Origin": {"://not-an-origin"},
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "multiple origins",
			headers: map[string][]string{
				"Origin": {testAdminOrigin, "https://attacker.example"},
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "cross-site fetch metadata",
			headers: map[string][]string{
				"Origin":         {testAdminOrigin},
				"Sec-Fetch-Site": {"cross-site"},
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "duplicated cross-site fetch metadata",
			headers: map[string][]string{
				"Origin":         {testAdminOrigin},
				"Sec-Fetch-Site": {"same-origin", "cross-site"},
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "authenticated non-browser client without origin",
			headers:    nil,
			wantStatus: http.StatusNoContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := adminMiddlewareRequest(
				handler,
				http.MethodPost,
				testAdminOrigin+"/api/admin/probe",
				rawToken,
				tt.headers,
			)
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, tt.wantStatus, response.Body.String())
			}
		})
	}

	t.Run("safe GET does not require unsafe origin check", func(t *testing.T) {
		response := adminMiddlewareRequest(
			handler,
			http.MethodGet,
			testAdminOrigin+"/api/admin/probe",
			rawToken,
			map[string][]string{
				"Origin":         {"https://attacker.example"},
				"Sec-Fetch-Site": {"cross-site"},
			},
		)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	})
}

func TestAdminBoundaryMissingConfigurationFailsClosed(t *testing.T) {
	if _, err := newAdminSecurityBoundary(adminAuthConfig{}); err == nil {
		t.Fatal("expected missing configuration error")
	}

	boundary := disabledAdminSecurityBoundary()
	handler := adminMiddlewareTestHandler(boundary)
	response := adminMiddlewareRequest(
		handler,
		http.MethodGet,
		testAdminOrigin+"/api/admin/probe",
		"otherwise-valid-looking-token",
		nil,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want %d", response.Code, http.StatusForbidden)
	}
}

func TestAdminBoundaryLeavesExistingPublicRoutesUnchanged(t *testing.T) {
	mux := http.NewServeMux()
	a := &app{}
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := withCommonHeaders(disabledAdminSecurityBoundary().protectAdminRoutes(mux))

	health := adminMiddlewareRequest(handler, http.MethodGet, "http://127.0.0.1:9300/health", "", nil)
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d, body = %s", health.Code, health.Body.String())
	}
	var body healthResponse
	if err := json.NewDecoder(health.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if body.Service != "miniai" || body.Status != "ok" {
		t.Fatalf("health response = %#v", body)
	}

	adminPage := adminMiddlewareRequest(handler, http.MethodGet, "http://127.0.0.1:9300/admin", "", nil)
	if adminPage.Code != http.StatusNoContent {
		t.Fatalf("existing /admin status = %d; want %d", adminPage.Code, http.StatusNoContent)
	}
}

func TestAdminBoundaryProtectsOnlyExactAdminAPINamespace(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/administrator", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/admin/probe", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := disabledAdminSecurityBoundary().protectAdminRoutes(mux)

	unrelated := adminMiddlewareRequest(handler, http.MethodGet, "http://127.0.0.1/api/administrator", "", nil)
	if unrelated.Code != http.StatusNoContent {
		t.Fatalf("unrelated status = %d; want %d", unrelated.Code, http.StatusNoContent)
	}
	protected := adminMiddlewareRequest(handler, http.MethodGet, "http://127.0.0.1/api/admin/probe", "", nil)
	if protected.Code != http.StatusUnauthorized {
		t.Fatalf("protected status = %d; want %d", protected.Code, http.StatusUnauthorized)
	}
}

func cryptographicAdminTestBoundary(t *testing.T) (adminSecurityBoundary, string) {
	t.Helper()
	privateKey := generateAdminTestKey(t)
	validator := newCloudflareAdminValidatorWithKeySet(
		testAdminIssuer,
		testAdminAudience,
		testAdminEmail,
		&oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&privateKey.PublicKey}},
	)
	now := time.Now().UTC()
	rawToken := signAdminTestToken(t, privateKey, "", adminAccessTestClaims{
		issuer: testAdminIssuer, audience: testAdminAudience, email: testAdminEmail,
		expiresAt: now.Add(time.Hour), notBefore: now.Add(-time.Minute),
	})
	return adminSecurityBoundary{validator: validator, trustedOrigin: testAdminOrigin}, rawToken
}

func adminMiddlewareTestHandler(boundary adminSecurityBoundary) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/probe", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := adminPrincipalFromContext(r.Context())
		if !ok {
			http.Error(w, "missing trusted principal", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(principal.Email))
	})
	mux.HandleFunc("POST /api/admin/probe", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminPrincipalFromContext(r.Context()); !ok {
			http.Error(w, "missing trusted principal", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return boundary.protectAdminRoutes(mux)
}

func adminMiddlewareRequest(
	handler http.Handler,
	method string,
	target string,
	rawToken string,
	headers map[string][]string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	if rawToken != "" {
		request.Header.Set(adminAccessJWTHeader, rawToken)
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
