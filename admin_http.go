package main

import (
	"net/http"
	"net/url"
	"strings"
)

type adminSecurityBoundary struct {
	validator     adminTokenValidator
	trustedOrigin string
}

func newAdminSecurityBoundary(config adminAuthConfig) (adminSecurityBoundary, error) {
	validator, err := newCloudflareAdminValidator(config)
	if err != nil {
		return adminSecurityBoundary{}, err
	}

	trustedOrigin, err := normalizedAdminPublicOrigin(config.PublicOrigin)
	if err != nil {
		return adminSecurityBoundary{}, err
	}

	return adminSecurityBoundary{
		validator:     validator,
		trustedOrigin: trustedOrigin,
	}, nil
}

func disabledAdminSecurityBoundary() adminSecurityBoundary {
	return adminSecurityBoundary{validator: rejectingAdminValidator{}}
}

func (boundary adminSecurityBoundary) requireAdmin(next http.Handler) http.Handler {
	validator := boundary.validator
	if validator == nil {
		validator = rejectingAdminValidator{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawToken := strings.TrimSpace(r.Header.Get(adminAccessJWTHeader))
		if rawToken == "" {
			http.Error(w, "administrator authentication required", http.StatusUnauthorized)
			return
		}

		principal, err := validator.Validate(r.Context(), rawToken)
		if err != nil {
			http.Error(w, "administrator access denied", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r.WithContext(withAdminPrincipal(r.Context(), principal)))
	})
}

func (boundary adminSecurityBoundary) requireAdminUnsafe(next http.Handler) http.Handler {
	originProtected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isUnsafeAdminMethod(r.Method) && !boundary.allowsUnsafeRequest(r) {
			http.Error(w, "cross-origin administrator request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})

	return boundary.requireAdmin(originProtected)
}

func (boundary adminSecurityBoundary) protectAdminRoutes(next http.Handler) http.Handler {
	protected := boundary.requireAdminUnsafe(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin" || strings.HasPrefix(r.URL.Path, "/api/admin/") {
			protected.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (boundary adminSecurityBoundary) allowsUnsafeRequest(r *http.Request) bool {
	if boundary.trustedOrigin == "" {
		return false
	}
	if hasCrossSiteFetchMetadata(r) {
		return false
	}

	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		// Authenticated non-browser clients do not normally send Origin. They are
		// allowed only after requireAdmin has validated the signed assertion.
		return true
	}
	if len(origins) != 1 {
		return false
	}

	origin := strings.TrimSpace(origins[0])
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}

	return origin == boundary.trustedOrigin
}

func hasCrossSiteFetchMetadata(r *http.Request) bool {
	for _, value := range r.Header.Values("Sec-Fetch-Site") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "cross-site") {
				return true
			}
		}
	}
	return false
}

func isUnsafeAdminMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}
