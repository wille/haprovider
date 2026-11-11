package internal

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/chain"
	"github.com/wille/haprovider/internal/chain/eth"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/rpc"
	"github.com/wille/haprovider/internal/rpctest"
)

// rawTx is a stand-in signed transaction; txHash is its hash (keccak256 of the
// bytes), which a provider that accepts it answers with.
const rawTx = "0x02f8af0180"

var txHash = func() string {
	h, err := eth.Broadcaster("eth_sendRawTransaction").Hash(json.RawMessage(`["` + rawTx + `"]`))
	if err != nil {
		panic(err)
	}
	return h
}()

const sendRawTxReq = `{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + rawTx + `"]}`

// broadcastEndpoint builds an eth endpoint with one online provider per
// upstream, in priority order.
func broadcastEndpoint(upstreams ...*recordingUpstream) *core.Endpoint {
	ep := &core.Endpoint{Name: "test", ChainID: "1", Kind: core.KindEth}
	for i, u := range upstreams {
		p := &core.Provider{Name: fmt.Sprintf("p%d", i), Http: u.URL, Endpoint: ep}
		p.MarkHealthy(0)
		ep.Providers = append(ep.Providers, p)
	}
	return ep
}

// sends returns how many times u was sent method.
func sends(u *recordingUpstream, method string) int {
	n := 0
	for _, m := range u.Methods() {
		if m == method {
			n++
		}
	}
	return n
}

func sendRaw(answer any) map[string]any {
	return map[string]any{"eth_sendRawTransaction": answer, "eth_blockNumber": "0x10"}
}

// eventuallySent waits for every upstream to have been sent the transaction:
// the ones that answer after the client got its answer get it in the
// background.
func eventuallySent(t *testing.T, upstreams ...*recordingUpstream) {
	t.Helper()
	for i, u := range upstreams {
		assert.Eventually(t, func() bool { return sends(u, "eth_sendRawTransaction") == 1 }, 2*time.Second, 10*time.Millisecond,
			"provider %d should be sent the transaction once", i)
	}
}

func TestBroadcast_AllProviders(t *testing.T) {
	a := newRecordingUpstream(t, sendRaw(txHash))
	b := newRecordingUpstream(t, sendRaw(txHash))
	c := newRecordingUpstream(t, sendRaw(txHash))

	assertResult(t, doRPC(broadcastEndpoint(a, b, c), sendRawTxReq), `"`+txHash+`"`)
	eventuallySent(t, a, b, c)
}

// TestBroadcast_AlreadyKnown: once one node has the transaction, the others
// usually say they already know it. That counts as accepted, and the client
// gets the hash.
func TestBroadcast_AlreadyKnown(t *testing.T) {
	known := rpctest.RPCError{Code: -32000, Message: "already known"}

	ep := broadcastEndpoint(newRecordingUpstream(t, sendRaw(known)), newRecordingUpstream(t, sendRaw(known)))
	assertResult(t, doRPC(ep, sendRawTxReq), `"`+txHash+`"`)
}

// TestBroadcast_AnotherAccepts: the first provider turning the transaction down
// doesn't matter when another accepts it.
func TestBroadcast_AnotherAccepts(t *testing.T) {
	a := newRecordingUpstream(t, sendRaw(rpctest.RPCError{Code: -32000, Message: "txpool is full"}))
	b := newRecordingUpstream(t, sendRaw(txHash))

	w := doRPC(broadcastEndpoint(a, b), sendRawTxReq)
	assertResult(t, w, `"`+txHash+`"`)
	assert.Equal(t, "p1", w.Header().Get("X-Provider"))
}

// TestBroadcast_AllReject: a transaction nobody accepts gets the first
// provider's error, and a rejection never takes a provider out of rotation.
func TestBroadcast_AllReject(t *testing.T) {
	a := newRecordingUpstream(t, sendRaw(rpctest.RPCError{Code: -32000, Message: "insufficient funds for gas * price + value"}))
	b := newRecordingUpstream(t, sendRaw(rpctest.RPCError{Code: -32000, Message: "nonce too low"}))
	ep := broadcastEndpoint(a, b)

	w := doRPC(ep, sendRawTxReq)
	res := decodeSingle(t, w)
	if assert.True(t, res.IsError()) {
		_, err := res.GetError()
		assert.Contains(t, err.Error(), "insufficient funds")
	}
	assert.Equal(t, "p0", w.Header().Get("X-Provider"))
	for _, p := range ep.Providers {
		assert.True(t, p.IsOnline())
	}
}

// TestBroadcast_WrongHash: a provider answering with another transaction's
// hash is lying or broken. Its answer is never used, and it's taken out of
// rotation.
func TestBroadcast_WrongHash(t *testing.T) {
	liar := newRecordingUpstream(t, sendRaw("0x"+strings.Repeat("ab", 32)))
	honest := newSlowUpstream(t, sendRaw(txHash), 50*time.Millisecond)
	ep := broadcastEndpoint(liar, honest)

	assertResult(t, doRPC(ep, sendRawTxReq), `"`+txHash+`"`)
	assert.False(t, ep.Providers[0].IsOnline())
	assert.True(t, ep.Providers[1].IsOnline())
}

// TestBroadcast_NoneAccepts: with no provider accepting or rejecting the
// transaction (here, all answer with the wrong hash), there's no answer to give.
func TestBroadcast_NoneAccepts(t *testing.T) {
	wrong := sendRaw("0x" + strings.Repeat("ab", 32))
	ep := broadcastEndpoint(newRecordingUpstream(t, wrong), newRecordingUpstream(t, wrong))

	w := doRPC(ep, sendRawTxReq)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assertRPCError(t, w, rpc.CodeNoProvidersAvailable)
}

// TestBroadcast_FirstAcceptanceAnswers: the client gets its answer when the
// first provider accepts the transaction, not when the slowest does; the slow
// one still gets it.
func TestBroadcast_FirstAcceptanceAnswers(t *testing.T) {
	const slow = 500 * time.Millisecond
	a := newSlowUpstream(t, sendRaw(txHash), slow)
	b := newRecordingUpstream(t, sendRaw(txHash))

	start := time.Now()
	w := doRPC(broadcastEndpoint(a, b), sendRawTxReq)
	assert.Less(t, time.Since(start), slow-100*time.Millisecond)
	assertResult(t, w, `"`+txHash+`"`)
	assert.Equal(t, "p1", w.Header().Get("X-Provider"))

	eventuallySent(t, a, b)
}

// TestBroadcast_Batch: in a batch, only the transaction goes to every provider;
// the rest goes the usual way, and the answers come back in order.
func TestBroadcast_Batch(t *testing.T) {
	a := newRecordingUpstream(t, sendRaw(txHash))
	b := newRecordingUpstream(t, sendRaw(txHash))

	w := doRPC(broadcastEndpoint(a, b), `[
		{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]},
		{"jsonrpc":"2.0","id":2,"method":"eth_sendRawTransaction","params":["`+rawTx+`"]}
	]`)

	res, err := rpc.DecodeBatchResponse(w.Body.Bytes())
	if assert.NoError(t, err) && assert.Len(t, res.Responses, 2) {
		assert.Equal(t, "1", res.Responses[0].GetID())
		assert.JSONEq(t, `"0x10"`, string(res.Responses[0].Result))
		assert.Equal(t, "2", res.Responses[1].GetID())
		assert.JSONEq(t, `"`+txHash+`"`, string(res.Responses[1].Result))
	}

	eventuallySent(t, a, b)
	assert.Equal(t, 1, sends(a, "eth_blockNumber"))
	assert.Zero(t, sends(b, "eth_blockNumber"), "only the transaction is broadcast")
}

// solanaBroadcastEndpoint builds a Solana endpoint whose providers answer
// sendTransaction with answer.
func solanaBroadcastEndpoint(t *testing.T, answer any) (*core.Endpoint, []*recordingUpstream) {
	ep := &core.Endpoint{Name: "test", ChainID: "mainnet", Kind: core.KindSolana}
	var us []*recordingUpstream
	for i := range 2 {
		u := newRecordingUpstream(t, map[string]any{"sendTransaction": answer})
		p := &core.Provider{Name: fmt.Sprintf("p%d", i), Http: u.URL, Endpoint: ep}
		p.MarkHealthy(0)
		ep.Providers = append(ep.Providers, p)
		us = append(us, u)
	}
	return ep, us
}

// TestBroadcast_Solana: a legacy Solana transaction (one signature first) is
// broadcast and its answer checked against its first signature.
func TestBroadcast_Solana(t *testing.T) {
	sig := make([]byte, 64)
	for i := range sig {
		sig[i] = byte(i + 1)
	}
	tx := base64.StdEncoding.EncodeToString(append(append([]byte{1}, sig...), 0x80, 1, 2, 3))
	params := `["` + tx + `",{"encoding":"base64"}]`

	want, err := chain.New(core.KindSolana).Broadcaster("sendTransaction").Hash(json.RawMessage(params))
	if !assert.NoError(t, err) {
		return
	}

	ep, us := solanaBroadcastEndpoint(t, want)
	w := doRPC(ep, `{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":`+params+`}`)
	assertResult(t, w, `"`+want+`"`)
	for _, u := range us {
		assert.Eventually(t, func() bool { return len(u.Methods()) == 1 }, 2*time.Second, 10*time.Millisecond)
	}
}

// TestBroadcast_UnreadFormat: a transaction in a format we don't read (here a
// Solana v1 transaction) is still broadcast, and the answer taken as given.
func TestBroadcast_UnreadFormat(t *testing.T) {
	tx := base64.StdEncoding.EncodeToString(append([]byte{0x81, 1, 0, 0}, make([]byte, 64)...))
	ep, us := solanaBroadcastEndpoint(t, "someSignature")

	w := doRPC(ep, `{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["`+tx+`",{"encoding":"base64"}]}`)
	assertResult(t, w, `"someSignature"`)
	for _, u := range us {
		assert.Eventually(t, func() bool { return len(u.Methods()) == 1 }, 2*time.Second, 10*time.Millisecond)
	}
}
