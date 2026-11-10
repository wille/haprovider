package internal

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/rpctest"
)

// syncBuffer is a bytes.Buffer safe to write from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// lines returns the logged lines containing substr.
func (s *syncBuffer) lines(substr string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, l := range strings.Split(s.b.String(), "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return out
}

// captureWarnings sends what's logged at warn level and above to a buffer for
// the duration of the test.
func captureWarnings(t *testing.T) *syncBuffer {
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func setUnverifiedWarnInterval(t *testing.T, d time.Duration) {
	prev := unverifiedWarnInterval
	unverifiedWarnInterval = d
	t.Cleanup(func() { unverifiedWarnInterval = prev })
}

// TestUnverifiedWarning_Throttled: with the second provider down, every checked
// read goes out unverified. That's a warning at most once per interval per
// endpoint, counting the ones since the last.
func TestUnverifiedWarning_Throttled(t *testing.T) {
	logs := captureWarnings(t)
	ep, us := verifyEndpoint(t, balance("0x1"), balance("0x1"))
	us[1].Close()

	for range 5 {
		assertResult(t, doRPC(ep, getBalanceReq), `"0x1"`)
	}
	lines := logs.lines("results not verified")
	if assert.Len(t, lines, 1, "one warning for the five") {
		assert.Contains(t, lines[0], "no other provider answered")
		assert.Contains(t, lines[0], "method=eth_getBalance")
		assert.Contains(t, lines[0], "answered_by=p0")
		assert.Contains(t, lines[0], "other_providers_online=0")
		assert.Contains(t, lines[0], "unverified_since_last_warning=1")
	}

	// Once the interval has passed, the next one is logged, with how many there
	// were since the last warning (the four held back and itself).
	setUnverifiedWarnInterval(t, 0)
	assertResult(t, doRPC(ep, getBalanceReq), `"0x1"`)
	lines = logs.lines("results not verified")
	if assert.Len(t, lines, 2) {
		assert.Contains(t, lines[1], "unverified_since_last_warning=5")
	}
}

// TestUnverifiedWarning_ProviderError: a second provider answering with an
// error is named, with its error.
func TestUnverifiedWarning_ProviderError(t *testing.T) {
	logs := captureWarnings(t)
	ep, _ := verifyEndpoint(t, balance("0x1"), map[string]any{"eth_getBalance": rpctest.RPCError{Code: -32000, Message: "boom"}})

	assertResult(t, doRPC(ep, getBalanceReq), `"0x1"`)
	lines := logs.lines("results not verified")
	if assert.Len(t, lines, 1) {
		assert.Contains(t, lines[0], "other provider answered with an error")
		assert.Contains(t, lines[0], "provider=p1")
		assert.Contains(t, lines[0], "boom")
		assert.Contains(t, lines[0], "other_providers_online=1")
	}
}

// TestUnverifiedWarning_PerEndpoint: each endpoint has its own interval, so one
// endpoint's warnings don't hold back another's.
func TestUnverifiedWarning_PerEndpoint(t *testing.T) {
	logs := captureWarnings(t)
	a, aus := verifyEndpoint(t, balance("0x1"), balance("0x1"))
	b, bus := verifyEndpoint(t, balance("0x1"), balance("0x1"))
	aus[1].Close()
	bus[1].Close()

	assertResult(t, doRPC(a, getBalanceReq), `"0x1"`)
	assertResult(t, doRPC(b, getBalanceReq), `"0x1"`)
	assert.Len(t, logs.lines("results not verified"), 2)
}

// TestUnverifiedWarning_NotWhenVerified: a check that agrees logs nothing.
func TestUnverifiedWarning_NotWhenVerified(t *testing.T) {
	logs := captureWarnings(t)
	ep, _ := verifyEndpoint(t, balance("0x1"), balance("0x1"))

	assertResult(t, doRPC(ep, getBalanceReq), `"0x1"`)
	assert.Empty(t, logs.lines("results not verified"))
}
