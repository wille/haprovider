package internal

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wille/haprovider/internal/redact"
)

// loadConfig runs LoadConfig on the given YAML (via $HA_CONFIG) and returns the
// config, or the error LoadConfig panicked with.
func loadConfig(t *testing.T, yml string) (config *Config, err error) {
	t.Helper()
	t.Setenv("HA_CONFIG", yml)

	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(error)
			if !ok {
				e = fmt.Errorf("%v", r)
			}
			err = e
		}
	}()

	return LoadConfig(""), nil
}

func TestLoadConfig_ExpandsProviderURLs(t *testing.T) {
	t.Setenv("QUICKNODE_TOKEN", "qn-secret")
	t.Setenv("INFURA_KEY", "infura-secret")
	t.Setenv("NODE_PASS", "node-secret")

	config, err := loadConfig(t, `
endpoints:
  eth:
    chainId: "1"
    providers:
      - name: quicknode
        http: https://x.quiknode.pro/${QUICKNODE_TOKEN}/
        ws: wss://x.quiknode.pro/${QUICKNODE_TOKEN}/
      - http: https://mainnet.infura.io/v3/$INFURA_KEY
        ws: wss://mainnet.infura.io/ws/v3/$INFURA_KEY
      - http: https://user:${NODE_PASS}@node.example.com/rpc
`)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	providers := config.Endpoints["eth"].Providers
	want := []struct{ name, http, ws string }{
		{"quicknode", "https://x.quiknode.pro/qn-secret/", "wss://x.quiknode.pro/qn-secret/"},
		// Unnamed providers fall back to the URL host, which carries no key.
		{"mainnet.infura.io", "https://mainnet.infura.io/v3/infura-secret", "wss://mainnet.infura.io/ws/v3/infura-secret"},
		{"node.example.com", "https://user:node-secret@node.example.com/rpc", ""},
	}
	for i, w := range want {
		p := providers[i]
		if p.Name != w.name || p.Http != w.http || p.Ws != w.ws {
			t.Errorf("provider %d = {%q, %q, %q}, want {%q, %q, %q}", i, p.Name, p.Http, p.Ws, w.name, w.http, w.ws)
		}
	}
}

// TestLoadConfig_ValidatesExpandedURL checks that chain validation runs on the
// expanded URL, so tron's /jsonrpc suffix check accepts a URL held entirely in
// an env var.
func TestLoadConfig_ValidatesExpandedURL(t *testing.T) {
	t.Setenv("TRON_URL", "https://api.trongrid.io/jsonrpc")

	config, err := loadConfig(t, `
endpoints:
  tron:
    kind: tron
    chainId: mainnet
    providers:
      - http: ${TRON_URL}
`)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := config.Endpoints["tron"].Providers[0].Http; got != "https://api.trongrid.io/jsonrpc" {
		t.Errorf("Http = %q, want the expanded TRON_URL", got)
	}
}

func TestLoadConfig_MissingURLVariable(t *testing.T) {
	t.Setenv("SET_TOKEN", "set-secret")
	t.Setenv("EMPTY_TOKEN", "")
	t.Setenv("UNSET_TOKEN", "")
	_ = os.Unsetenv("UNSET_TOKEN")

	cases := []struct {
		name     string
		provider string
		want     []string
	}{
		{
			name: "unset in http",
			provider: `
      - name: quicknode
        http: https://x.quiknode.pro/${UNSET_TOKEN}/`,
			want: []string{"provider eth/quicknode", "http URL", "UNSET_TOKEN"},
		},
		{
			name: "empty in http",
			provider: `
      - name: quicknode
        http: https://x.quiknode.pro/${EMPTY_TOKEN}/`,
			want: []string{"provider eth/quicknode", "http URL", "EMPTY_TOKEN"},
		},
		{
			// Unnamed providers are identified by index, and only the missing
			// variable is named even when another one in the URL is set.
			name: "unset in ws, unnamed",
			provider: `
      - http: https://x.quiknode.pro/${SET_TOKEN}/
        ws: wss://x.quiknode.pro/${SET_TOKEN}/${UNSET_TOKEN}`,
			want: []string{"provider eth/providers[0]", "ws URL", "UNSET_TOKEN"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadConfig(t, "endpoints:\n  eth:\n    chainId: \"1\"\n    providers:"+c.provider+"\n")
			if err == nil {
				t.Fatal("LoadConfig succeeded, want an error for the missing variable")
			}
			msg := err.Error()
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error %q does not contain %q", msg, w)
				}
			}
			if strings.Contains(msg, "set-secret") || strings.Contains(msg, "quiknode.pro") {
				t.Errorf("error leaks the URL or an expanded value: %q", msg)
			}
		})
	}
}

// TestLoadConfig_ExpandedURLRedacted logs the expanded provider URLs through a
// logger configured like main's and checks the secrets never reach the output.
func TestLoadConfig_ExpandedURLRedacted(t *testing.T) {
	t.Setenv("QUICKNODE_TOKEN", "qn-secret")

	config, err := loadConfig(t, `
endpoints:
  eth:
    chainId: "1"
    providers:
      - http: https://x.quiknode.pro/${QUICKNODE_TOKEN}/
        ws: wss://x.quiknode.pro/${QUICKNODE_TOKEN}/
`)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	p := config.Endpoints["eth"].Providers[0]

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: redact.Attr}))
	logger.Info("connecting to", "provider", p.Name, "http", p.Http, "ws", p.Ws)

	out := buf.String()
	if strings.Contains(out, "qn-secret") {
		t.Errorf("log output leaked the expanded token:\n%s", out)
	}
	if !strings.Contains(out, "https://x.quiknode.pro") || !strings.Contains(out, "wss://x.quiknode.pro") {
		t.Errorf("log output dropped the provider host:\n%s", out)
	}

	for _, s := range []string{redact.URL(p.Http), redact.URL(p.Ws), p.Name} {
		if strings.Contains(s, "qn-secret") {
			t.Errorf("%q leaks the expanded token", s)
		}
	}
}

func TestLoadConfig_StallTimeout(t *testing.T) {
	config, err := loadConfig(t, `
endpoints:
  eth:
    chainId: "1"
    stall_timeout: 90s
    providers:
      - http: https://example.com
  base:
    chainId: "8453"
    stall_timeout: 0s
    providers:
      - http: https://example.com
  polygon:
    chainId: "137"
    providers:
      - http: https://example.com
`)
	if err != nil {
		t.Fatal(err)
	}

	if got := config.Endpoints["eth"].StallTimeout; got == nil || *got != 90*time.Second {
		t.Errorf("eth stall_timeout = %v, want 90s", got)
	}
	if got := config.Endpoints["base"].StallTimeout; got == nil || *got != 0 {
		t.Errorf("base stall_timeout = %v, want 0 (disabled)", got)
	}
	if got := config.Endpoints["polygon"].StallTimeout; got != nil {
		t.Errorf("polygon stall_timeout = %v, want unset", *got)
	}
}

// TestLoadConfig_UnknownKey: a misspelled setting is an error, rather than
// silently ignored (which for "chainID" used to mean trusting whichever
// provider answered first).
func TestLoadConfig_UnknownKey(t *testing.T) {
	for _, yml := range []string{
		"endpoints:\n  eth:\n    chainID: \"1\"\n    providers:\n      - http: https://example.com\n",
		"endpoints:\n  eth:\n    chainId: \"1\"\n    block_lag_tolerence: 5\n    providers:\n      - http: https://example.com\n",
		"endpoints:\n  eth:\n    chainId: \"1\"\n    providers:\n      - http: https://example.com\n        timout: 5s\n",
		"logLevel: debug\nendpoints:\n  eth:\n    chainId: \"1\"\n    providers:\n      - http: https://example.com\n",
		"endpoints:\n  eth:\n    chainId: \"1\"\n    chainId: \"5\"\n    providers:\n      - http: https://example.com\n",
	} {
		if _, err := loadConfig(t, yml); err == nil {
			t.Errorf("LoadConfig succeeded, want an error for:\n%s", yml)
		}
	}
}

func TestLoadConfig_ChainIDRequired(t *testing.T) {
	for _, kind := range []string{"eth", "solana", "tron", "btc"} {
		_, err := loadConfig(t, "endpoints:\n  e:\n    kind: "+kind+"\n    providers:\n      - http: https://example.com/jsonrpc\n")
		if err == nil || !strings.Contains(err.Error(), "endpoint e: chainId is required") {
			t.Errorf("%s: LoadConfig error = %v, want chainId is required", kind, err)
		}
	}
}

// TestLoadConfig_TronJSONRPCSuffix: the check that tron provider URLs point at
// the JSON-RPC API runs with a chainId set too (it used to be skipped).
func TestLoadConfig_TronJSONRPCSuffix(t *testing.T) {
	_, err := loadConfig(t, "endpoints:\n  tron:\n    kind: tron\n    chainId: mainnet\n    providers:\n      - http: https://api.trongrid.io\n")
	if err == nil || !strings.Contains(err.Error(), "/jsonrpc") {
		t.Errorf("LoadConfig error = %v, want the /jsonrpc suffix error", err)
	}
}

// TestLoadConfig_Example: the example config shipped in the repository loads.
func TestLoadConfig_Example(t *testing.T) {
	b, err := os.ReadFile("../config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(t, string(b)); err != nil {
		t.Fatalf("config.example.yml: %v", err)
	}
}

func TestLoadConfig_Verify(t *testing.T) {
	config, err := loadConfig(t, `
endpoints:
  eth:
    chainId: "1"
    verify: true
    verify_retry_delay: 500ms
    providers:
      - http: https://example.com
  base:
    chainId: "8453"
    verify: true
    verify_retry_delay: 0s
    providers:
      - http: https://example.com
  polygon:
    chainId: "137"
    providers:
      - http: https://example.com
`)
	if err != nil {
		t.Fatal(err)
	}

	eth, base, polygon := config.Endpoints["eth"], config.Endpoints["base"], config.Endpoints["polygon"]
	if !eth.Verify || !base.Verify || polygon.Verify {
		t.Errorf("verify = %v/%v/%v, want true/true/false", eth.Verify, base.Verify, polygon.Verify)
	}
	if got := eth.GetVerifyRetryDelay(); got != 500*time.Millisecond {
		t.Errorf("eth retry delay = %s, want 500ms", got)
	}
	if got := base.GetVerifyRetryDelay(); got != 0 {
		t.Errorf("base retry delay = %s, want 0 (no retry)", got)
	}
	if got := polygon.GetVerifyRetryDelay(); got != time.Second {
		t.Errorf("polygon retry delay = %s, want the 1s default", got)
	}
}

func TestLoadConfig_Broadcast(t *testing.T) {
	config, err := loadConfig(t, `
endpoints:
  eth:
    chainId: "1"
    broadcast: true
    providers:
      - http: https://example.com
  base:
    chainId: "8453"
    providers:
      - http: https://example.com
`)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Endpoints["eth"].Broadcast || config.Endpoints["base"].Broadcast {
		t.Errorf("broadcast = %v/%v, want true/false", config.Endpoints["eth"].Broadcast, config.Endpoints["base"].Broadcast)
	}
}
