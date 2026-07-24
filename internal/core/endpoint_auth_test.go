package core

import "testing"

func TestEndpointAuth(t *testing.T) {
	// No keys: open, no auth required.
	open := &Endpoint{}
	open.InitAuth(nil)
	if open.RequiresAuth() {
		t.Fatal("endpoint with no keys must not require auth")
	}
	if open.Authorized("anything") {
		t.Fatal("open endpoint should not report tokens as authorized")
	}

	// Endpoint keys (rotation: multiple valid keys).
	ep := &Endpoint{Keys: []string{"k1", "k2"}}
	ep.InitAuth(nil)
	if !ep.RequiresAuth() {
		t.Fatal("endpoint with keys must require auth")
	}
	if !ep.Authorized("k1") || !ep.Authorized("k2") {
		t.Fatal("each configured key must authorize")
	}
	if ep.Authorized("nope") || ep.Authorized("") {
		t.Fatal("wrong or empty token must be rejected")
	}

	// Endpoint keys + global keys both authorize.
	both := &Endpoint{Keys: []string{"local"}}
	both.InitAuth([]string{"global1", "global2"})
	for _, k := range []string{"local", "global1", "global2"} {
		if !both.Authorized(k) {
			t.Errorf("key %q should authorize", k)
		}
	}

	// Global-only keys still require and grant auth.
	g := &Endpoint{}
	g.InitAuth([]string{"g"})
	if !g.RequiresAuth() || !g.Authorized("g") {
		t.Fatal("global keys should apply to an endpoint with no own keys")
	}
}
