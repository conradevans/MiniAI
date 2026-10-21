package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	testAdminIssuer   = "https://test.cloudflareaccess.com"
	testAdminAudience = "miniai-test-audience"
	testAdminEmail    = "admin@example.com"
	testAdminOrigin   = "https://miniai.example.com"
)

type adminAccessTestClaims struct {
	issuer    string
	audience  string
	email     string
	expiresAt time.Time
	notBefore time.Time
}

func TestCloudflareAdminValidatorCryptographicValidation(t *testing.T) {
	privateKey := generateAdminTestKey(t)
	otherPrivateKey := generateAdminTestKey(t)
	validator := newCloudflareAdminValidatorWithKeySet(
		testAdminIssuer,
		testAdminAudience,
		testAdminEmail,
		&oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&privateKey.PublicKey}},
	)

	now := time.Now().UTC()
	valid := adminAccessTestClaims{
		issuer:    testAdminIssuer,
		audience:  testAdminAudience,
		email:     "Admin@Example.com",
		expiresAt: now.Add(time.Hour),
		notBefore: now.Add(-time.Minute),
	}

	tests := []struct {
		name       string
		rawToken   string
		claims     adminAccessTestClaims
		signingKey *rsa.PrivateKey
		wantError  bool
	}{
		{name: "valid assertion", claims: valid, signingKey: privateKey},
		{name: "malformed assertion", rawToken: "not-a-jwt", wantError: true},
		{name: "invalid signature", claims: valid, signingKey: otherPrivateKey, wantError: true},
		{
			name: "wrong issuer",
			claims: adminAccessTestClaims{
				issuer: "https://other.cloudflareaccess.com", audience: valid.audience,
				email: valid.email, expiresAt: valid.expiresAt, notBefore: valid.notBefore,
			},
			signingKey: privateKey, wantError: true,
		},
		{
			name: "token for another Access application",
			claims: adminAccessTestClaims{
				issuer: valid.issuer, audience: "other-application-audience",
				email: valid.email, expiresAt: valid.expiresAt, notBefore: valid.notBefore,
			},
			signingKey: privateKey, wantError: true,
		},
		{
			name: "expired assertion",
			claims: adminAccessTestClaims{
				issuer: valid.issuer, audience: valid.audience, email: valid.email,
				expiresAt: now.Add(-time.Hour), notBefore: valid.notBefore,
			},
			signingKey: privateKey, wantError: true,
		},
		{
			name: "not yet valid assertion",
			claims: adminAccessTestClaims{
				issuer: valid.issuer, audience: valid.audience, email: valid.email,
				expiresAt: valid.expiresAt, notBefore: now.Add(10 * time.Minute),
			},
			signingKey: privateKey, wantError: true,
		},
		{
			name: "token for another administrator",
			claims: adminAccessTestClaims{
				issuer: valid.issuer, audience: valid.audience, email: "other@example.com",
				expiresAt: valid.expiresAt, notBefore: valid.notBefore,
			},
			signingKey: privateKey, wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawToken := tt.rawToken
			if tt.signingKey != nil {
				rawToken = signAdminTestToken(t, tt.signingKey, "", tt.claims)
			}

			principal, err := validator.Validate(context.Background(), rawToken)
			if tt.wantError {
				if err == nil {
					t.Fatal("expected validation error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() error: %v", err)
			}
			if principal.Email != testAdminEmail {
				t.Fatalf("principal email = %q; want %q", principal.Email, testAdminEmail)
			}
		})
	}
}

func TestAdminAuthConfiguration(t *testing.T) {
	t.Setenv("MINIAI_ACCESS_TEAM_DOMAIN", testAdminIssuer)
	t.Setenv("MINIAI_ACCESS_AUDIENCE", testAdminAudience)
	t.Setenv("MINIAI_ACCESS_ADMIN_EMAIL", "Admin@Example.com")
	t.Setenv("MINIAI_ACCESS_PUBLIC_ORIGIN", testAdminOrigin)

	config := adminAuthConfigFromEnvironment()
	if config.TeamDomain != testAdminIssuer ||
		config.Audience != testAdminAudience ||
		config.AdminEmail != "Admin@Example.com" ||
		config.PublicOrigin != testAdminOrigin {

		t.Fatalf("unexpected admin auth configuration: %#v", config)
	}

	valid := adminAuthConfig{
		TeamDomain: testAdminIssuer, Audience: testAdminAudience,
		AdminEmail: testAdminEmail, PublicOrigin: testAdminOrigin,
	}
	if _, err := newAdminSecurityBoundary(valid); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*adminAuthConfig)
	}{
		{name: "missing team domain", mutate: func(c *adminAuthConfig) { c.TeamDomain = "" }},
		{name: "HTTP team domain", mutate: func(c *adminAuthConfig) { c.TeamDomain = "http://test.cloudflareaccess.com" }},
		{name: "team domain with path", mutate: func(c *adminAuthConfig) { c.TeamDomain = testAdminIssuer + "/keys" }},
		{name: "team domain with empty query", mutate: func(c *adminAuthConfig) { c.TeamDomain = testAdminIssuer + "?" }},
		{name: "missing audience", mutate: func(c *adminAuthConfig) { c.Audience = "" }},
		{name: "missing administrator", mutate: func(c *adminAuthConfig) { c.AdminEmail = "" }},
		{name: "malformed administrator", mutate: func(c *adminAuthConfig) { c.AdminEmail = "not-an-email" }},
		{name: "multiple administrators", mutate: func(c *adminAuthConfig) { c.AdminEmail = "a@example.com, b@example.com" }},
		{name: "missing public origin", mutate: func(c *adminAuthConfig) { c.PublicOrigin = "" }},
		{name: "HTTP public origin", mutate: func(c *adminAuthConfig) { c.PublicOrigin = "http://miniai.example.com" }},
		{name: "public origin with path", mutate: func(c *adminAuthConfig) { c.PublicOrigin = testAdminOrigin + "/admin" }},
		{name: "public origin with empty fragment", mutate: func(c *adminAuthConfig) { c.PublicOrigin = testAdminOrigin + "#" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := valid
			tt.mutate(&config)
			if _, err := newAdminSecurityBoundary(config); err == nil {
				t.Fatal("expected malformed configuration to fail closed")
			}
		})
	}
}

func TestCloudflareAdminRemoteKeysCacheAndRefresh(t *testing.T) {
	keyA := generateAdminTestKey(t)
	keyB := generateAdminTestKey(t)
	var requests atomic.Int32
	var keyMu sync.RWMutex
	currentKeys := adminTestJWKS(t, &keyA.PublicKey, "key-a")

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/cdn-cgi/access/certs" {
			http.NotFound(w, r)
			return
		}
		keyMu.RLock()
		body := append([]byte(nil), currentKeys...)
		keyMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	config := adminAuthConfig{
		TeamDomain: server.URL, Audience: testAdminAudience,
		AdminEmail: testAdminEmail, PublicOrigin: testAdminOrigin,
	}
	validator, err := newCloudflareAdminValidatorWithHTTPClient(config, server.Client())
	if err != nil {
		t.Fatalf("construct remote validator: %v", err)
	}

	now := time.Now().UTC()
	claims := adminAccessTestClaims{
		issuer: server.URL, audience: testAdminAudience, email: testAdminEmail,
		expiresAt: now.Add(time.Hour), notBefore: now.Add(-time.Minute),
	}
	tokenA := signAdminTestToken(t, keyA, "key-a", claims)
	for range 2 {
		if _, err := validator.Validate(context.Background(), tokenA); err != nil {
			t.Fatalf("validate cached token: %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("JWKS requests after cache reuse = %d; want 1", got)
	}

	keyMu.Lock()
	currentKeys = adminTestJWKS(t, &keyB.PublicKey, "key-b")
	keyMu.Unlock()
	tokenB := signAdminTestToken(t, keyB, "key-b", claims)
	if _, err := validator.Validate(context.Background(), tokenB); err != nil {
		t.Fatalf("validate token after key rotation: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("JWKS requests after unknown key ID = %d; want 2", got)
	}
}

func TestCloudflareAdminRemoteKeyFailuresFailClosed(t *testing.T) {
	privateKey := generateAdminTestKey(t)

	t.Run("key fetch failure", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer server.Close()
		validator := remoteAdminTestValidator(t, server, server.Client())
		token := remoteAdminTestToken(t, privateKey, server.URL)
		if _, err := validator.Validate(context.Background(), token); err == nil {
			t.Fatal("expected failed key fetch to reject assertion")
		}
	})

	t.Run("key fetch timeout", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			time.Sleep(150 * time.Millisecond)
		}))
		defer server.Close()
		client := server.Client()
		client.Timeout = 20 * time.Millisecond
		validator := remoteAdminTestValidator(t, server, client)
		token := remoteAdminTestToken(t, privateKey, server.URL)
		started := time.Now()
		if _, err := validator.Validate(context.Background(), token); err == nil {
			t.Fatal("expected timed-out key fetch to reject assertion")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("key fetch timeout took %s", elapsed)
		}
	})

	t.Run("oversized key response", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", adminJWKSMaxResponseBytes+1)))
		}))
		defer server.Close()
		validator := remoteAdminTestValidator(t, server, server.Client())
		token := remoteAdminTestToken(t, privateKey, server.URL)
		_, err := validator.Validate(context.Background(), token)
		if err == nil || !strings.Contains(err.Error(), errAdminJWKSResponseTooLarge.Error()) {
			t.Fatalf("oversized response error = %v", err)
		}
	})

	t.Run("HTTP key URL rejected", func(t *testing.T) {
		config := adminAuthConfig{
			TeamDomain: "http://test.cloudflareaccess.com", Audience: testAdminAudience,
			AdminEmail: testAdminEmail, PublicOrigin: testAdminOrigin,
		}
		if _, err := newCloudflareAdminValidator(config); err == nil {
			t.Fatal("expected insecure key URL to be rejected")
		}
	})
}

func remoteAdminTestValidator(
	t *testing.T,
	server *httptest.Server,
	client *http.Client,
) adminTokenValidator {
	t.Helper()
	validator, err := newCloudflareAdminValidatorWithHTTPClient(
		adminAuthConfig{
			TeamDomain: server.URL, Audience: testAdminAudience,
			AdminEmail: testAdminEmail, PublicOrigin: testAdminOrigin,
		},
		client,
	)
	if err != nil {
		t.Fatalf("construct remote validator: %v", err)
	}
	return validator
}

func remoteAdminTestToken(t *testing.T, key *rsa.PrivateKey, issuer string) string {
	t.Helper()
	now := time.Now().UTC()
	return signAdminTestToken(t, key, "remote-key", adminAccessTestClaims{
		issuer: issuer, audience: testAdminAudience, email: testAdminEmail,
		expiresAt: now.Add(time.Hour), notBefore: now.Add(-time.Minute),
	})
}

func generateAdminTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return privateKey
}

func signAdminTestToken(
	t *testing.T,
	privateKey *rsa.PrivateKey,
	keyID string,
	claims adminAccessTestClaims,
) string {
	t.Helper()
	options := (&jose.SignerOptions{}).WithType("JWT")
	if keyID != "" {
		options = options.WithHeader("kid", keyID)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: privateKey},
		options,
	)
	if err != nil {
		t.Fatalf("create JWT signer: %v", err)
	}

	standardClaims := jwt.Claims{
		Issuer:    claims.issuer,
		Audience:  jwt.Audience{claims.audience},
		Expiry:    jwt.NewNumericDate(claims.expiresAt),
		NotBefore: jwt.NewNumericDate(claims.notBefore),
		IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
	}
	privateClaims := struct {
		Email string `json:"email"`
	}{Email: claims.email}

	rawToken, err := jwt.Signed(signer).
		Claims(standardClaims).
		Claims(privateClaims).
		Serialize()
	if err != nil {
		t.Fatalf("sign Access assertion: %v", err)
	}
	return rawToken
}

func adminTestJWKS(t *testing.T, publicKey *rsa.PublicKey, keyID string) []byte {
	t.Helper()
	body, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: publicKey, KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig",
	}}})
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}
	return body
}

func TestRejectingAdminValidatorAlwaysFailsClosed(t *testing.T) {
	_, err := (rejectingAdminValidator{}).Validate(context.Background(), "anything")
	if !errors.Is(err, errAdminAccessDenied) {
		t.Fatalf("rejecting validator error = %v", err)
	}
}
