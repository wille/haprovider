package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
	"github.com/wille/haprovider/internal/rpctest"
)

// recordingUpstream is a mock provider that records the methods it is asked for.
type recordingUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	methods []string
}

func newRecordingUpstream(t *testing.T, methods map[string]any) *recordingUpstream {
	return newSlowUpstream(t, methods, 0)
}

// newSlowUpstream is a recordingUpstream that takes delay to answer.
func newSlowUpstream(t *testing.T, methods map[string]any, delay time.Duration) *recordingUpstream {
	inner := rpctest.NewServer(methods, nil)
	t.Cleanup(inner.Close)

	u := &recordingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		body, _ := io.ReadAll(r.Body)
		if batch, err := rpc.DecodeBatchRequest(body); err == nil {
			u.mu.Lock()
			for _, req := range batch.Requests {
				u.methods = append(u.methods, req.Method)
			}
			u.mu.Unlock()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *recordingUpstream) Methods() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.methods...)
}

// verifyEndpoint builds an eth endpoint with verify enabled and one online
// provider per methods map, in priority order.
func verifyEndpoint(t *testing.T, upstreams ...map[string]any) (*core.Endpoint, []*recordingUpstream) {
	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth, Verify: true, VerifyRetryDelay: &testRetryDelay}
	var us []*recordingUpstream
	for i, methods := range upstreams {
		u := newRecordingUpstream(t, methods)
		p := &core.Provider{Name: fmt.Sprintf("p%d", i), Http: u.URL, Endpoint: ep}
		p.MarkHealthy(0)
		ep.Providers = append(ep.Providers, p)
		us = append(us, u)
	}
	return ep, us
}

// testRetryDelay keeps the retry on disagreement from slowing the tests down.
var testRetryDelay = 10 * time.Millisecond

func balance(hex string) map[string]any {
	return map[string]any{"eth_getBalance": hex}
}

const getBalanceReq = `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`

// decodeSingle decodes a single JSON-RPC response from w.
func decodeSingle(t *testing.T, w *httptest.ResponseRecorder) *rpc.Response {
	t.Helper()
	res, err := rpc.DecodeResponse(w.Body.Bytes())
	if err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return res
}

func assertResult(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	res := decodeSingle(t, w)
	if assert.False(t, res.IsError(), "unexpected error: %s", res.Error) {
		assert.JSONEq(t, want, string(res.Result))
	}
}

func assertDisagree(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusOK, w.Code, "a disagreement is an in-band JSON-RPC error")
	res := decodeSingle(t, w)
	if assert.True(t, res.IsError(), "expected an error, got %s", res.Result) {
		code, _ := res.GetError()
		assert.Equal(t, rpc.CodeProvidersDisagree, code)
	}
}

func TestCrossCheck_Off(t *testing.T) {
	ep, us := verifyEndpoint(t, balance("0x1"), balance("0x2"))
	ep.Verify = false

	assertResult(t, doRPC(ep, getBalanceReq), `"0x1"`)
	assert.Empty(t, us[1].Methods(), "the second provider isn't asked")
}

func TestCrossCheck_Agree(t *testing.T) {
	ep, us := verifyEndpoint(t, balance("0x10"), balance("0x010"))

	assertResult(t, doRPC(ep, getBalanceReq), `"0x10"`)
	assert.Equal(t, []string{"eth_getBalance"}, us[1].Methods())
}

// TestCrossCheck_DisagreeTwoProviders: with two providers there is no majority,
// so neither answer is returned.
func TestCrossCheck_DisagreeTwoProviders(t *testing.T) {
	ep, _ := verifyEndpoint(t, balance("0xde0b6b3a7640000"), balance("0x0"))

	assertDisagree(t, doRPC(ep, getBalanceReq))
	for _, p := range ep.Providers {
		assert.True(t, p.IsOnline(), "a disagreement doesn't take %s out of rotation", p.Name)
	}
}

func TestCrossCheck_FirstOutvoted(t *testing.T) {
	ep, _ := verifyEndpoint(t, balance("0xde0b6b3a7640000"), balance("0x0"), balance("0x0"))

	assertResult(t, doRPC(ep, getBalanceReq), `"0x0"`)
}

func TestCrossCheck_SecondOutvoted(t *testing.T) {
	ep, _ := verifyEndpoint(t, balance("0x0"), balance("0xde0b6b3a7640000"), balance("0x0"))

	assertResult(t, doRPC(ep, getBalanceReq), `"0x0"`)
}

func TestCrossCheck_NoMajority(t *testing.T) {
	ep, _ := verifyEndpoint(t, balance("0x1"), balance("0x2"), balance("0x3"))

	assertDisagree(t, doRPC(ep, getBalanceReq))
}

// TestCrossCheck_DisagreeThirdDown: a disagreement with the third provider
// unable to answer is still a disagreement.
func TestCrossCheck_DisagreeThirdDown(t *testing.T) {
	ep, us := verifyEndpoint(t, balance("0x1"), balance("0x2"), balance("0x1"))
	us[2].Close()

	assertDisagree(t, doRPC(ep, getBalanceReq))
}

// TestCrossCheck_SingleProvider: best effort, a single provider's answer is
// returned unverified.
func TestCrossCheck_SingleProvider(t *testing.T) {
	ep, _ := verifyEndpoint(t, balance("0x1"))

	assertResult(t, doRPC(ep, getBalanceReq), `"0x1"`)
}

// TestCrossCheck_SecondDown: best effort, when the other provider can't be
// reached the answer is returned unverified.
func TestCrossCheck_SecondDown(t *testing.T) {
	ep, us := verifyEndpoint(t, balance("0x1"), balance("0x2"))
	us[1].Close()

	assertResult(t, doRPC(ep, getBalanceReq), `"0x1"`)
}

// TestCrossCheck_SecondErrors: best effort, when the other provider answers
// with an error the answer is returned unverified.
func TestCrossCheck_SecondErrors(t *testing.T) {
	ep, _ := verifyEndpoint(t, balance("0x1"), map[string]any{"eth_getBalance": rpctest.RPCError{Code: -32000, Message: "boom"}})

	assertResult(t, doRPC(ep, getBalanceReq), `"0x1"`)
}

// TestCrossCheck_SecondMalformed: best effort, a malformed answer from the other
// provider can't be compared, so the first is returned unverified.
func TestCrossCheck_SecondMalformed(t *testing.T) {
	ep, _ := verifyEndpoint(t, balance("0x1"), balance("not hex"))

	assertResult(t, doRPC(ep, getBalanceReq), `"0x1"`)
}

// TestCrossCheck_BlockNumberLowest: of two block heights the lower is returned,
// so a provider reporting a too-high height can't inflate confirmations.
func TestCrossCheck_BlockNumberLowest(t *testing.T) {
	high := map[string]any{"eth_blockNumber": "0x105"}
	low := map[string]any{"eth_blockNumber": "0x100"}

	ep, _ := verifyEndpoint(t, high, low)
	assertResult(t, doRPC(ep, blockNumberReq), `"0x100"`)

	ep, _ = verifyEndpoint(t, low, high)
	assertResult(t, doRPC(ep, blockNumberReq), `"0x100"`)
}

func TestCrossCheck_ErrorNotChecked(t *testing.T) {
	ep, us := verifyEndpoint(t,
		map[string]any{"eth_call": rpctest.RPCError{Code: 3, Message: "execution reverted"}},
		map[string]any{"eth_call": "0x01"},
	)

	// The second provider is asked at the same time, before the first answer is
	// known to be an error; the error is still returned as the first gave it.
	w := doRPC(ep, `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{},"latest"]}`)
	res := decodeSingle(t, w)
	if assert.True(t, res.IsError(), "an error credits nothing, so it isn't compared") {
		code, _ := res.GetError()
		assert.Equal(t, 3, code)
	}
	assert.Equal(t, []string{"eth_call"}, us[1].Methods())
}

// TestCrossCheck_Concurrent: the second provider is asked at the same time as
// the first, so a check takes as long as the slower of the two, not both.
func TestCrossCheck_Concurrent(t *testing.T) {
	const delay = 300 * time.Millisecond

	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth, Verify: true, VerifyRetryDelay: &testRetryDelay}
	for i := range 2 {
		u := newSlowUpstream(t, balance("0x1"), delay)
		p := &core.Provider{Name: fmt.Sprintf("p%d", i), Http: u.URL, Endpoint: ep}
		p.MarkHealthy(0)
		ep.Providers = append(ep.Providers, p)
	}

	start := time.Now()
	assertResult(t, doRPC(ep, getBalanceReq), `"0x1"`)
	assert.Less(t, time.Since(start), 2*delay-50*time.Millisecond, "the two providers were asked one after the other")
}

// TestCrossCheck_FirstFailsOver: when the first provider fails and the request
// is served by the one asked for the second opinion, another is asked instead.
func TestCrossCheck_FirstFailsOver(t *testing.T) {
	ep, us := verifyEndpoint(t, balance("0x1"), balance("0x1"), balance("0x1"))
	us[0].Close()

	assertResult(t, doRPC(ep, getBalanceReq), `"0x1"`)
	assert.Equal(t, []string{"eth_getBalance"}, us[2].Methods(), "the third provider gives the second opinion")
}

// TestCrossCheck_FirstFailsOverTwoProviders: best effort, with the first
// provider down and only one left, the answer is returned unverified.
func TestCrossCheck_FirstFailsOverTwoProviders(t *testing.T) {
	ep, us := verifyEndpoint(t, balance("0x1"), balance("0x2"))
	us[0].Close()

	assertResult(t, doRPC(ep, getBalanceReq), `"0x2"`)
}

// TestCrossCheck_PendingNotChecked: reads at the pending block aren't
// cross-checked, since each node builds its pending block from its own mempool.
func TestCrossCheck_PendingNotChecked(t *testing.T) {
	ep, us := verifyEndpoint(t, map[string]any{"eth_call": "0x01"}, map[string]any{"eth_call": "0x02"})

	assertResult(t, doRPC(ep, `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{},"pending"]}`), `"0x01"`)
	assert.Empty(t, us[1].Methods())
}

func TestCrossCheck_UnlistedMethodNotChecked(t *testing.T) {
	ep, us := verifyEndpoint(t, map[string]any{"eth_chainId": "0x1"}, map[string]any{"eth_chainId": "0x2"})

	assertResult(t, doRPC(ep, `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`), `"0x1"`)
	assert.Empty(t, us[1].Methods())
}

// TestCrossCheck_Batch: only the listed methods of a batch are sent to the
// second provider, so a transaction in the batch goes to each provider once (by
// the broadcast, not again by the cross-check), and a disagreement only fails
// its own entry.
func TestCrossCheck_Batch(t *testing.T) {
	ep, us := verifyEndpoint(t,
		map[string]any{"eth_getBalance": "0x1", "eth_sendRawTransaction": txHash, "eth_call": "0xAB"},
		map[string]any{"eth_getBalance": "0x2", "eth_sendRawTransaction": txHash, "eth_call": "0xab"},
	)

	w := doRPC(ep, `[
		{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]},
		{"jsonrpc":"2.0","id":2,"method":"eth_sendRawTransaction","params":["`+rawTx+`"]},
		{"jsonrpc":"2.0","id":3,"method":"eth_call","params":[{},"latest"]}
	]`)

	res, err := rpc.DecodeBatchResponse(w.Body.Bytes())
	if !assert.NoError(t, err) || !assert.Len(t, res.Responses, 3) {
		return
	}

	balanceRes := res.GetResponseByID("1")
	if assert.True(t, balanceRes.IsError(), "the balances disagree") {
		code, _ := balanceRes.GetError()
		assert.Equal(t, rpc.CodeProvidersDisagree, code)
	}
	assert.JSONEq(t, `"`+txHash+`"`, string(res.GetResponseByID("2").Result))
	assert.JSONEq(t, `"0xAB"`, string(res.GetResponseByID("3").Result), "eth_call agrees, letter case aside")

	// eth_getBalance twice: the disagreement is asked again before giving up.
	eventuallySent(t, us...)
	assert.ElementsMatch(t, []string{"eth_getBalance", "eth_sendRawTransaction", "eth_call", "eth_getBalance"}, us[1].Methods())
}

// TestCrossCheck_Solana: Solana results agree on their value even though the
// context slot differs.
func TestCrossCheck_Solana(t *testing.T) {
	a := newRecordingUpstream(t, map[string]any{"getBalance": map[string]any{"context": map[string]any{"slot": 1}, "value": 5}})
	b := newRecordingUpstream(t, map[string]any{"getBalance": map[string]any{"context": map[string]any{"slot": 2}, "value": 5}})

	ep := &core.Endpoint{Name: "test", ChainID: "mainnet", Kind: core.KindSolana, Verify: true, VerifyRetryDelay: &testRetryDelay}
	for i, u := range []*recordingUpstream{a, b} {
		p := &core.Provider{Name: fmt.Sprintf("p%d", i), Http: u.URL, Endpoint: ep}
		p.MarkHealthy(0)
		ep.Providers = append(ep.Providers, p)
	}

	w := doRPC(ep, `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["addr"]}`)
	assertResult(t, w, `{"context":{"slot":1},"value":5}`)
	assert.Equal(t, []string{"getBalance"}, b.Methods())
}

// sequenceUpstream is a mock provider that answers method with results in
// turn, repeating the last one. An rpctest.RPCError entry answers with that
// error. Other methods get "method not found".
func sequenceUpstream(t *testing.T, method string, results ...any) *recordingUpstream {
	u := &recordingUpstream{}
	calls := 0
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		batch, err := rpc.DecodeBatchRequest(body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		var responses []*rpc.Response
		for _, req := range batch.Requests {
			res := &rpc.Response{Version: "2.0", ID: req.ID}

			u.mu.Lock()
			u.methods = append(u.methods, req.Method)
			var v any
			if req.Method == method {
				v = results[min(calls, len(results)-1)]
				calls++
			}
			u.mu.Unlock()

			switch e := v.(type) {
			case nil:
				res.Error = rpc.NewError(-32601, "method not found")
			case rpctest.RPCError:
				res.Error = rpc.NewError(e.Code, e.Message)
			default:
				res.Result, _ = json.Marshal(v)
			}
			responses = append(responses, res)
		}

		out, _ := rpc.SerializeBatchResponse(&rpc.BatchResponse{Responses: responses, IsBatch: batch.IsBatch})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(u.Close)
	return u
}

// endpointOf builds an eth endpoint with verify enabled and one online provider
// per upstream, in priority order.
func endpointOf(upstreams ...*recordingUpstream) *core.Endpoint {
	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth, Verify: true, VerifyRetryDelay: &testRetryDelay}
	for i, u := range upstreams {
		p := &core.Provider{Name: fmt.Sprintf("p%d", i), Http: u.URL, Endpoint: ep}
		p.MarkHealthy(0)
		ep.Providers = append(ep.Providers, p)
	}
	return ep
}

// TestCrossCheck_RetryResolves: providers a block apart disagree on a balance
// that just changed; asked again, they agree, and the client gets the new
// balance instead of an error.
func TestCrossCheck_RetryResolves(t *testing.T) {
	ahead := sequenceUpstream(t, "eth_getBalance", "0x5")
	behind := sequenceUpstream(t, "eth_getBalance", "0x0", "0x5")

	assertResult(t, doRPC(endpointOf(ahead, behind), getBalanceReq), `"0x5"`)
	assert.Len(t, ahead.Methods(), 2, "asked, then asked again")
	assert.Len(t, behind.Methods(), 2, "asked, then asked again")
}

// TestCrossCheck_RetryAvoidsThird: when asking again settles it, the third
// provider isn't asked, so nobody is reported as outvoted.
func TestCrossCheck_RetryAvoidsThird(t *testing.T) {
	ahead := sequenceUpstream(t, "eth_getBalance", "0x5")
	behind := sequenceUpstream(t, "eth_getBalance", "0x0", "0x5")
	third := sequenceUpstream(t, "eth_getBalance", "0x5")

	assertResult(t, doRPC(endpointOf(ahead, behind, third), getBalanceReq), `"0x5"`)
	assert.Empty(t, third.Methods())
}

func TestCrossCheck_RetryStillDisagrees(t *testing.T) {
	a := sequenceUpstream(t, "eth_getBalance", "0x5")
	b := sequenceUpstream(t, "eth_getBalance", "0x0")

	assertDisagree(t, doRPC(endpointOf(a, b), getBalanceReq))
	assert.Len(t, b.Methods(), 2)
}

// TestCrossCheck_RetryStillDisagreesThird: a disagreement that outlasts the
// retry is settled by the third provider.
func TestCrossCheck_RetryStillDisagreesThird(t *testing.T) {
	a := sequenceUpstream(t, "eth_getBalance", "0x5")
	b := sequenceUpstream(t, "eth_getBalance", "0x0")
	c := sequenceUpstream(t, "eth_getBalance", "0x5")

	assertResult(t, doRPC(endpointOf(a, b, c), getBalanceReq), `"0x5"`)
	assert.Len(t, c.Methods(), 1)
}

// TestCrossCheck_RetryFails: when one provider fails on the retry, the
// disagreement already on record stands; it's not returned unverified.
func TestCrossCheck_RetryFails(t *testing.T) {
	a := sequenceUpstream(t, "eth_getBalance", "0x5")
	b := sequenceUpstream(t, "eth_getBalance", "0x0", rpctest.RPCError{Code: -32000, Message: "boom"})

	assertDisagree(t, doRPC(endpointOf(a, b), getBalanceReq))
}

func TestCrossCheck_RetryDisabled(t *testing.T) {
	a := sequenceUpstream(t, "eth_getBalance", "0x5")
	b := sequenceUpstream(t, "eth_getBalance", "0x0", "0x5")
	ep := endpointOf(a, b)
	off := time.Duration(0)
	ep.VerifyRetryDelay = &off

	assertDisagree(t, doRPC(ep, getBalanceReq))
	assert.Len(t, b.Methods(), 1)
}

// TestGoAsk_RecoversPanic: a panic while asking for a second opinion is
// recovered, and counts as no answer instead of crashing the process.
func TestGoAsk_RecoversPanic(t *testing.T) {
	got := <-goAsk(slog.New(slog.DiscardHandler), func() answers { panic("boom") })
	assert.Nil(t, got.provider)
	assert.Nil(t, got.byIndex)
}
