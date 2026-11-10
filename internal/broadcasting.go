package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/wille/haprovider/internal/broadcast"
	"github.com/wille/haprovider/internal/chain"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/metrics"
	"github.com/wille/haprovider/internal/rpc"
)

// tx is a request of a batch that submits a transaction, broadcast to every
// provider.
type tx struct {
	// index of the request in the batch.
	index  int
	method string
	spec   *broadcast.Spec

	// hash is the transaction's hash, or "" if it can't be computed (a format
	// we don't read), in which case answers aren't checked against it.
	hash string
}

// providerAnswers are one provider's answers to the broadcast transactions.
type providerAnswers struct {
	provider *core.Provider

	// byIndex holds the answers keyed by request index. Nil when the provider
	// gave none.
	byIndex map[int]*rpc.Response
}

// forwardTransactions forwards req like forwardChecked, except that on an
// endpoint with broadcast enabled, the requests that submit a transaction
// (eth_sendRawTransaction, Solana sendTransaction) go to every provider at
// once (see broadcastTxs). The rest of a batch goes the usual way at the same
// time.
func forwardTransactions(ctx context.Context, endpoint *core.Endpoint, req *rpc.BatchRequest, timing *servertiming.Header, log *slog.Logger) (*forwardResult, error) {
	txs := txsFor(endpoint, req)
	if len(txs) == 0 {
		return forwardChecked(ctx, endpoint, req, timing, log)
	}

	start := time.Now()

	isTx := make(map[int]bool, len(txs))
	for _, t := range txs {
		isTx[t.index] = true
	}
	var rest []int
	for i := range req.Requests {
		if !isTx[i] {
			rest = append(rest, i)
		}
	}

	type restResult struct {
		res *forwardResult
		err error
	}
	restCh := make(chan restResult, 1)
	var restReq *rpc.BatchRequest
	if len(rest) > 0 {
		restReq = subBatch(req, rest)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					log.Error("panic while forwarding a batch", "panic", p, "stack", string(debug.Stack()))
					restCh <- restResult{err: fmt.Errorf("panic: %v", p)}
				}
			}()
			res, err := forwardChecked(ctx, endpoint, restReq, timing, log)
			restCh <- restResult{res: res, err: err}
		}()
	}

	answers, provider, txErr := broadcastTxs(ctx, endpoint, req, txs, timing, log)

	var restRes *forwardResult
	if len(rest) > 0 {
		r := <-restCh
		if r.err != nil {
			return nil, r.err
		}
		restRes = r.res
	}
	if txErr != nil {
		return nil, txErr
	}

	responses := make([]*rpc.Response, len(req.Requests))
	for _, t := range txs {
		responses[t.index] = answers[t.index]
	}
	if restRes != nil {
		at := responseIndex(restReq, restRes.res)
		for k, i := range rest {
			responses[i] = restRes.res.Responses[at[k]]
		}
	}

	return &forwardResult{
		res:      &rpc.BatchResponse{Responses: responses, IsBatch: req.IsBatch},
		provider: provider,
		dur:      time.Since(start),
	}, nil
}

// txsFor returns the requests of req that submit a transaction, when the
// endpoint has broadcast enabled.
func txsFor(endpoint *core.Endpoint, req *rpc.BatchRequest) []tx {
	if !endpoint.Broadcast {
		return nil
	}
	c := chain.New(endpoint.Kind)
	if c == nil {
		return nil
	}

	var txs []tx
	for i, sub := range req.Requests {
		spec := c.Broadcaster(sub.Method)
		if spec == nil {
			continue
		}
		hash, err := spec.Hash(sub.Params)
		if err != nil {
			hash = ""
		}
		txs = append(txs, tx{index: i, method: sub.Method, spec: spec, hash: hash})
	}
	return txs
}

// broadcastTxs sends the transactions of req to every healthy provider at
// once, and returns the answer for each to send the client, along with the
// provider that gave the first one.
//
// A transaction is accepted by a provider that answers with its hash, or says
// it already has it (once one node has it, that's the usual answer from the
// others). The client gets its answer as soon as each transaction has been
// accepted by any provider; the other providers still get the transactions,
// and their answers are recorded in the background. A provider answering with
// another transaction's hash is lying or broken: its answer is never used, and
// it's taken out of rotation.
//
// A transaction no provider accepts gets the error of the first provider, in
// priority order, that rejected it (e.g. insufficient funds: a rejection is
// about the transaction, so it never marks a provider unhealthy). When no
// provider answered at all, it returns errNoProvidersAvailable.
func broadcastTxs(ctx context.Context, endpoint *core.Endpoint, req *rpc.BatchRequest, txs []tx, timing *servertiming.Header, log *slog.Logger) (map[int]*rpc.Response, *core.Provider, error) {
	indices := make([]int, len(txs))
	for k, t := range txs {
		indices[k] = t.index
	}
	sub := subBatch(req, indices)

	var providers []*core.Provider
	for _, p := range endpoint.Providers {
		if p.IsOnline() && supportsAllMethods(p, sub) {
			providers = append(providers, p)
		}
	}
	if len(providers) == 0 {
		return nil, nil, errNoProvidersAvailable
	}

	// Every provider gets the transactions, even after one has accepted them
	// or the client has left: that's the point. Each send is still bounded by
	// the provider's timeout.
	sendCtx := context.WithoutCancel(ctx)
	results := make(chan providerAnswers, len(providers))
	for _, p := range providers {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("panic while broadcasting a transaction", "provider", p.Name, "panic", r, "stack", string(debug.Stack()))
					results <- providerAnswers{provider: p}
				}
			}()
			a := askProvider(sendCtx, endpoint, req, indices, timing, log, p)
			results <- providerAnswers{provider: p, byIndex: a.byIndex}
		}()
	}

	accepted := make(map[int]*rpc.Response, len(txs))
	rejected := make(map[int]map[*core.Provider]*rpc.Response, len(txs))
	var first *core.Provider

	pending := len(providers)
	for pending > 0 && len(accepted) < len(txs) {
		r := <-results
		pending--

		for _, t := range txs {
			res := r.byIndex[t.index]
			switch recordBroadcast(endpoint, r.provider, t, res, log) {
			case metrics.BroadcastAccepted, metrics.BroadcastAlreadyKnown:
				if _, ok := accepted[t.index]; !ok {
					accepted[t.index] = acceptedResponse(req.Requests[t.index], t, res)
					if first == nil {
						first = r.provider
					}
					log.Debug("transaction accepted", "provider", r.provider.Name, "method", t.method, "hash", t.hash)
				}
			case metrics.BroadcastRejected:
				if rejected[t.index] == nil {
					rejected[t.index] = make(map[*core.Provider]*rpc.Response)
				}
				rejected[t.index][r.provider] = res
			}
		}
	}

	// The providers yet to answer still get the transactions; record their
	// answers when they come.
	if pending > 0 {
		go func(pending int) {
			for ; pending > 0; pending-- {
				r := <-results
				for _, t := range txs {
					recordBroadcast(endpoint, r.provider, t, r.byIndex[t.index], log)
				}
			}
		}(pending)
	}

	out := make(map[int]*rpc.Response, len(txs))
	for _, t := range txs {
		if res, ok := accepted[t.index]; ok {
			out[t.index] = res
			continue
		}

		var res *rpc.Response
		for _, p := range providers {
			if r, ok := rejected[t.index][p]; ok {
				res = r
				if first == nil {
					first = p
				}
				break
			}
		}
		if res == nil {
			return nil, nil, errNoProvidersAvailable
		}
		out[t.index] = res
	}
	return out, first, nil
}

// recordBroadcast classifies a provider's answer to a broadcast transaction,
// records it, and returns its outcome. A provider answering with another
// transaction's hash is logged and taken out of rotation.
func recordBroadcast(endpoint *core.Endpoint, p *core.Provider, t tx, res *rpc.Response, log *slog.Logger) string {
	outcome := classifyBroadcast(t, res)
	metrics.RecordBroadcastResult(endpoint.Name, p.Name, t.method, outcome)

	if outcome == metrics.BroadcastWrongHash {
		log.Error("provider answered a transaction with another transaction's hash", "provider", p.Name, "method", t.method, "hash", t.hash, "answer", rpc.FormatRawBody(string(res.Result)))
		p.MarkUnhealthy(fmt.Errorf("answered a transaction with another transaction's hash"))
	}
	return outcome
}

// classifyBroadcast returns the outcome of a provider's answer (nil if it
// gave none) to a broadcast transaction.
func classifyBroadcast(t tx, res *rpc.Response) string {
	switch {
	case res == nil:
		return metrics.BroadcastFailed
	case res.IsError():
		// "Already known" only counts when we know the hash to give the client.
		if _, err := res.GetError(); t.hash != "" && t.spec.AlreadyKnown(err.Error()) {
			return metrics.BroadcastAlreadyKnown
		}
		return metrics.BroadcastRejected
	case t.hash == "":
		// A format we don't read: take the answer unchecked.
		return metrics.BroadcastAccepted
	}

	var got string
	if err := json.Unmarshal(res.Result, &got); err != nil || !t.spec.SameHash(t.hash, got) {
		return metrics.BroadcastWrongHash
	}
	return metrics.BroadcastAccepted
}

// acceptedResponse is the client's answer to a transaction a provider
// accepted: the transaction's hash under the client's id, or, for a format we
// don't read, the provider's answer as it gave it.
func acceptedResponse(req *rpc.Request, t tx, res *rpc.Response) *rpc.Response {
	if t.hash == "" {
		r := *res
		r.ID = req.ID
		return &r
	}
	result, _ := json.Marshal(t.hash)
	return &rpc.Response{Version: "2.0", ID: req.ID, Result: result}
}
