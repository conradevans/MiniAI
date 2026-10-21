package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	adminAccessJWTHeader      = "Cf-Access-Jwt-Assertion"
	adminJWKSHTTPTimeout      = 5 * time.Second
	adminJWKSMaxResponseBytes = 1 << 20
)

var (
	errAdminAccessDenied         = errors.New("administrator access denied")
	errAdminJWKSInsecureURL      = errors.New("administrator key retrieval requires HTTPS")
	errAdminJWKSResponseTooLarge = errors.New("administrator key response is too large")
)

type adminPrincipal struct {
	Email string
}

type adminTokenValidator interface {
	Validate(context.Context, string) (adminPrincipal, error)
}

type adminAuthConfig struct {
	TeamDomain   string
	Audience     string
	AdminEmail   string
	PublicOrigin string
}

type cloudflareAdminValidator struct {
	verifier   *oidc.IDTokenVerifier
	adminEmail string
}

type rejectingAdminValidator struct{}

type adminPrincipalContextKey struct{}

func adminAuthConfigFromEnvironment() adminAuthConfig {
	return adminAuthConfig{
		TeamDomain:   os.Getenv("MINIAI_ACCESS_TEAM_DOMAIN"),
		Audience:     os.Getenv("MINIAI_ACCESS_AUDIENCE"),
		AdminEmail:   os.Getenv("MINIAI_ACCESS_ADMIN_EMAIL"),
		PublicOrigin: os.Getenv("MINIAI_ACCESS_PUBLIC_ORIGIN"),
	}
}

func newCloudflareAdminValidator(config adminAuthConfig) (adminTokenValidator, error) {
	return newCloudflareAdminValidatorWithHTTPClient(config, nil)
}

func newCloudflareAdminValidatorWithHTTPClient(
	config adminAuthConfig,
	httpClient *http.Client,
) (adminTokenValidator, error) {
	teamDomain, err := normalizedAdminTeamDomain(config.TeamDomain)
	if err != nil {
		return nil, err
	}

	audience := strings.TrimSpace(config.Audience)
	if audience == "" {
		return nil, errors.New("MINIAI_ACCESS_AUDIENCE is required")
	}

	adminEmail, err := validatedAdminEmail(config.AdminEmail)
	if err != nil {
		return nil, err
	}

	client := boundedAdminJWKSClient(httpClient)
	keyContext := oidc.ClientContext(context.Background(), client)
	keySet := oidc.NewRemoteKeySet(
		keyContext,
		teamDomain+"/cdn-cgi/access/certs",
	)

	return newCloudflareAdminValidatorWithKeySet(
		teamDomain,
		audience,
		adminEmail,
		keySet,
	), nil
}

func newCloudflareAdminValidatorWithKeySet(
	issuer string,
	audience string,
	adminEmail string,
	keySet oidc.KeySet,
) *cloudflareAdminValidator {
	return &cloudflareAdminValidator{
		verifier: oidc.NewVerifier(
			issuer,
			keySet,
			&oidc.Config{ClientID: audience},
		),
		adminEmail: normalizedAdminEmail(adminEmail),
	}
}

func (validator *cloudflareAdminValidator) Validate(
	ctx context.Context,
	rawToken string,
) (adminPrincipal, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return adminPrincipal{}, errAdminAccessDenied
	}

	token, err := validator.verifier.Verify(ctx, rawToken)
	if err != nil {
		return adminPrincipal{}, fmt.Errorf("verify Access assertion: %w", err)
	}

	var claims struct {
		Email string `json:"email"`
	}
	if err := token.Claims(&claims); err != nil {
		return adminPrincipal{}, fmt.Errorf("read Access assertion claims: %w", err)
	}

	email := normalizedAdminEmail(claims.Email)
	if email == "" || email != validator.adminEmail {
		return adminPrincipal{}, errAdminAccessDenied
	}

	return adminPrincipal{Email: email}, nil
}

func (rejectingAdminValidator) Validate(
	context.Context,
	string,
) (adminPrincipal, error) {
	return adminPrincipal{}, errAdminAccessDenied
}

func normalizedAdminTeamDomain(value string) (string, error) {
	return normalizedHTTPSOrigin(
		value,
		"MINIAI_ACCESS_TEAM_DOMAIN must be an HTTPS origin",
	)
}

func normalizedAdminPublicOrigin(value string) (string, error) {
	return normalizedHTTPSOrigin(
		value,
		"MINIAI_ACCESS_PUBLIC_ORIGIN must be an HTTPS origin",
	)
}

func normalizedHTTPSOrigin(value string, invalidMessage string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if value == "" || err != nil ||
		strings.ContainsAny(value, "?#") ||
		parsed.Scheme != "https" ||
		parsed.Host == "" ||
		parsed.Hostname() == "" ||
		parsed.User != nil ||
		parsed.Opaque != "" ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {

		return "", errors.New(invalidMessage)
	}

	return "https://" + parsed.Host, nil
}

func validatedAdminEmail(value string) (string, error) {
	email := normalizedAdminEmail(value)
	address, err := mail.ParseAddress(email)
	if email == "" || err != nil || address.Name != "" || normalizedAdminEmail(address.Address) != email {
		return "", errors.New("MINIAI_ACCESS_ADMIN_EMAIL must be a single email address")
	}
	return email, nil
}

func normalizedAdminEmail(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func withAdminPrincipal(ctx context.Context, principal adminPrincipal) context.Context {
	return context.WithValue(ctx, adminPrincipalContextKey{}, principal)
}

func adminPrincipalFromContext(ctx context.Context) (adminPrincipal, bool) {
	principal, ok := ctx.Value(adminPrincipalContextKey{}).(adminPrincipal)
	return principal, ok
}

func boundedAdminJWKSClient(base *http.Client) *http.Client {
	client := &http.Client{}
	if base != nil {
		*client = *base
	}

	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = adminJWKSTransport{
		next:             transport,
		maxResponseBytes: adminJWKSMaxResponseBytes,
	}
	if client.Timeout <= 0 || client.Timeout > adminJWKSHTTPTimeout {
		client.Timeout = adminJWKSHTTPTimeout
	}

	baseRedirectCheck := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if request.URL.Scheme != "https" {
			return errAdminJWKSInsecureURL
		}
		if baseRedirectCheck != nil {
			return baseRedirectCheck(request, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}

	return client
}

type adminJWKSTransport struct {
	next             http.RoundTripper
	maxResponseBytes int64
}

func (transport adminJWKSTransport) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	if request.URL.Scheme != "https" {
		return nil, errAdminJWKSInsecureURL
	}

	response, err := transport.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = &boundedAdminReadCloser{
		body:      response.Body,
		remaining: transport.maxResponseBytes,
	}
	return response, nil
}

type boundedAdminReadCloser struct {
	body      io.ReadCloser
	remaining int64
}

func (reader *boundedAdminReadCloser) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if reader.remaining > 0 {
		if int64(len(buffer)) > reader.remaining {
			buffer = buffer[:reader.remaining]
		}
		n, err := reader.body.Read(buffer)
		reader.remaining -= int64(n)
		return n, err
	}

	var probe [1]byte
	n, err := reader.body.Read(probe[:])
	if n > 0 {
		return 0, errAdminJWKSResponseTooLarge
	}
	return 0, err
}

func (reader *boundedAdminReadCloser) Close() error {
	return reader.body.Close()
}
