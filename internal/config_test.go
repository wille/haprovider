package internal

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

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
			_, err := loadConfig(t, "endpoints:\n  eth:\n    providers:"+c.provider+"\n")
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
