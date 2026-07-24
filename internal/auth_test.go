package internal

import (
	"net/http"
	"testing"

	"github.com/wille/haprovider/internal/core"
)

func TestBearerToken(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true}, // scheme is case-insensitive
		{"BEARER abc", "abc", true},
		{"Bearer   abc  ", "abc", true}, // surrounding whitespace trimmed
		{"", "", false},
		{"abc", "", false},
		{"Basic abc", "", false},
		{"Bearer ", "", false}, // empty token
		{"Bearer", "", false},
	}
	for _, c := range cases {
		got, ok := bearerToken(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestAuthenticate(t *testing.T) {
	req := func(auth string) *http.Request {
		r := httptestNewRequest()
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		return r
	}

	// Open endpoint passes with no token.
	open := &core.Endpoint{}
	open.InitAuth(nil)
	if !Authenticate(open, req("")) {
		t.Fatal("open endpoint should pass without a token")
	}

	// Protected endpoint requires a valid token.
	protected := &core.Endpoint{Keys: []string{"secret"}}
	protected.InitAuth(nil)
	if Authenticate(protected, req("")) {
		t.Fatal("protected endpoint should reject a request with no token")
	}
	if Authenticate(protected, req("Bearer wrong")) {
		t.Fatal("wrong token should be rejected")
	}
	if !Authenticate(protected, req("Bearer secret")) {
		t.Fatal("valid token should authenticate")
	}
}

func httptestNewRequest() *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/eth", nil)
	return r
}
