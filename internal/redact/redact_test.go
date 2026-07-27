package redact

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

func TestURL(t *testing.T) {
	cases := map[string]string{
		// Helius: key in query.
		"https://mainnet.helius-rpc.com/?api-key=SECRET":   "https://mainnet.helius-rpc.com",
		"https://mainnet.helius-rpc.com/?api-key=SECRET#f": "https://mainnet.helius-rpc.com",
		// Infura / Alchemy: key in the path.
		"https://mainnet.infura.io/v3/SECRET":         "https://mainnet.infura.io",
		"https://eth-mainnet.g.alchemy.com/v2/SECRET": "https://eth-mainnet.g.alchemy.com",
		// userinfo credentials.
		"https://user:pass@node.example.com/rpc": "https://node.example.com",
		// WebSocket schemes + port preserved.
		"wss://ws.example.com:8546/?token=SECRET": "wss://ws.example.com:8546",
		"ws://127.0.0.1:9944/SECRET":              "ws://127.0.0.1:9944",
		// No secret to strip.
		"https://plain.example.com": "https://plain.example.com",
		// Unparseable / non-URL input.
		"not a url": "[redacted]",
		"":          "[redacted]",
	}
	for in, want := range cases {
		if got := URL(in); got != want {
			t.Errorf("URL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestString(t *testing.T) {
	// A URL embedded in a net/url-style error message is scrubbed to scheme+host.
	got := String(`Post "https://mainnet.helius-rpc.com/?api-key=SECRET": dial tcp: timeout`)
	if strings.Contains(got, "SECRET") {
		t.Errorf("String left the secret in: %q", got)
	}
	if !strings.Contains(got, "https://mainnet.helius-rpc.com") {
		t.Errorf("String dropped the host: %q", got)
	}

	// Multiple URLs are all scrubbed.
	got = String("from https://a.example/v3/K1 to wss://b.example/?key=K2 done")
	for _, secret := range []string{"K1", "K2"} {
		if strings.Contains(got, secret) {
			t.Errorf("String left %s in: %q", secret, got)
		}
	}

	// No URL -> unchanged (fast path).
	const plain = "provider is behind by 5 slots"
	if got := String(plain); got != plain {
		t.Errorf("String(%q) = %q, want unchanged", plain, got)
	}
}

// TestAttr_RedactsThroughSlog builds a logger with Attr as the ReplaceAttr hook
// and verifies both a string URL attribute and a URL-bearing error attribute are
// scrubbed in the actual output.
func TestAttr_RedactsThroughSlog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: Attr}))

	logger.Info("connecting to", "http", "https://mainnet.helius-rpc.com/?api-key=SECRET")
	logger.Error("provider lost", "error", &url.Error{
		Op:  "Post",
		URL: "https://mainnet.helius-rpc.com/?api-key=SECRET",
		Err: errTimeout{},
	})

	out := buf.String()
	if strings.Contains(out, "SECRET") {
		t.Errorf("log output leaked the API key:\n%s", out)
	}
	if !strings.Contains(out, "https://mainnet.helius-rpc.com") {
		t.Errorf("log output dropped the host:\n%s", out)
	}

	// A plain error (no URL) is preserved as an error string, not mangled.
	buf.Reset()
	logger.Error("boom", "error", errTimeout{})
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("invalid json log: %v", err)
	}
	if rec["error"] != "i/o timeout" {
		t.Errorf("plain error attr = %v, want %q", rec["error"], "i/o timeout")
	}
}

type errTimeout struct{}

func (errTimeout) Error() string { return "i/o timeout" }
