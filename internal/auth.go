package internal

import (
	"net/http"
	"strings"

	"github.com/wille/haprovider/internal/core"
)

// Authenticate reports whether a request may proceed against endpoint. It returns
// true when the endpoint requires no authentication, or when the request carries
// an Authorization: Bearer <token> header with a token the endpoint accepts.
func Authenticate(endpoint *core.Endpoint, r *http.Request) bool {
	if !endpoint.RequiresAuth() {
		return true
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	return ok && endpoint.Authorized(token)
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header
// value. The scheme is matched case-insensitively. It returns ok=false when the
// header is missing, not a Bearer scheme, or has an empty token.
func bearerToken(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}
