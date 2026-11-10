package internal

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime/debug"
	"slices"
	"time"

	servertiming "github.com/mitchellh/go-server-timing"
	"github.com/wille/haprovider/internal/chain"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/metrics"
	"github.com/wille/haprovider/internal/rpc"
	"github.com/wille/haprovider/internal/verify"
)

// check is a request of a batch whose answer is cross-checked, and how.
type check struct {
	// index of the request in the batch.
	index int

	// verify compares two providers' results for it.
	verify verify.Func
}

// answers are another provider's answers to the cross-checked requests of a
// batch.
type answers struct {
	// byIndex holds the answers keyed by request index. Nil when no provider
	// answered.
	byIndex map[int]*rpc.Response

	// provider is the provider that answered, or nil.
	provider *core.Provider
}

// forwardChecked forwards req like forwardWithFailover and, on an endpoint
// with verify enabled, cross-checks the answers against another provider (see
// crossCheck).
//
// The other provider is asked at the same time as the first, so a check adds
// no latency unless it's the slower of the two. Only when the first provider
// fails and the request fails over to the one already asked for the second
// opinion is another asked afterwards. Settling a disagreement also asks a
// third provider afterwards.
func forwardChecked(ctx context.Context, endpoint *core.Endpoint, req *rpc.BatchRequest, timing *servertiming.Header, log *slog.Logger) (*forwardResult, error) {
	checks := checksFor(endpoint, req)
	if len(checks) == 0 {
		return forwardWithFailover(ctx, endpoint, req, timing, log)
	}

	// Ask for the second opinion from any provider but the one the request is
	// about to be sent to.
	secondCtx, cancelSecond := context.WithCancel(ctx)
	defer cancelSecond()

	first := firstProvider(endpoint, req)
	secondCh := goAsk(log, func() answers {
		return askOthers(secondCtx, endpoint, req, checks, timing, log, first)
	})

	result, err := forwardWithFailover(ctx, endpoint, req, timing, log)
	if err != nil {
		// Nothing to check: stop asking, and wait so the goroutine is done with
		// req before the caller moves on.
		cancelSecond()
		<-secondCh
		return nil, err
	}
	second := <-secondCh

	// The first provider failed and the request was served by the one asked
	// for the second opinion, which therefore isn't one: ask another.
	if second.provider != nil && second.provider == result.provider {
		second = askOthers(ctx, endpoint, req, checks, timing, log, result.provider)
	}

	return crossCheck(ctx, endpoint, req, result, checks, second, timing, log), nil
}

// goAsk runs ask on its own goroutine and delivers its answers on the returned
// channel. A panic in ask is recovered and logged, and delivers no answers
// (so the result goes out unverified): neither net/http nor the coalescing
// path recovers panics on goroutines other than their own, so an unrecovered
// one here would crash the process.
func goAsk(log *slog.Logger, ask func() answers) <-chan answers {
	ch := make(chan answers, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic while cross-checking a result", "panic", p, "stack", string(debug.Stack()))
				ch <- answers{}
			}
		}()
		ch <- ask()
	}()
	return ch
}

// checksFor returns the requests of req to cross-check: those the endpoint's
// chain cross-checks (see chain.Chain.Verifier), when it has verify enabled.
func checksFor(endpoint *core.Endpoint, req *rpc.BatchRequest) []check {
	if !endpoint.Verify {
		return nil
	}
	c := chain.New(endpoint.Kind)
	if c == nil {
		return nil
	}

	var checks []check
	for i, sub := range req.Requests {
		if v := c.Verifier(sub.Method, sub.Params); v != nil {
			checks = append(checks, check{index: i, verify: v})
		}
	}
	return checks
}

// firstProvider returns the provider forwardWithFailover tries first for req,
// or nil if there is none.
func firstProvider(endpoint *core.Endpoint, req *rpc.BatchRequest) *core.Provider {
	for _, p := range endpoint.Providers {
		if p.IsOnline() && supportsAllMethods(p, req) {
			return p
		}
	}
	return nil
}

// crossCheck compares result, the first provider's response to req, with
// second, another provider's answers to the checks, and returns the result to
// send the client. Only successful answers are compared; everything else,
// errors included, is sent as the first provider gave it.
//
// It is best effort, so losing a provider never disrupts traffic: when no
// other provider answered, or its answer is malformed, the result is sent
// unverified. When two providers disagree, a third settles it if there is
// one. Without a majority the client gets a CodeProvidersDisagree error for
// that request instead of either answer, so one provider can't get a wrong
// answer through.
func crossCheck(ctx context.Context, endpoint *core.Endpoint, req *rpc.BatchRequest, result *forwardResult, checks []check, second answers, timing *servertiming.Header, log *slog.Logger) *forwardResult {
	at := responseIndex(req, result.res)

	// The response is shared with coalesced callers once returned, so build a
	// new one rather than changing the first provider's.
	responses := slices.Clone(result.res.Responses)
	setResult := func(i int, value json.RawMessage) {
		r := *responses[at[i]]
		r.Result = value
		responses[at[i]] = &r
	}

	var disputed []check
	for _, ch := range checks {
		method := req.Requests[ch.index].Method
		first, other := result.res.Responses[at[ch.index]], second.byIndex[ch.index]

		// An error credits nothing, so there is nothing to check.
		if first.IsError() {
			continue
		}

		if other == nil || other.IsError() {
			metrics.RecordVerifiedResult(endpoint.Name, method, metrics.VerifyUnverified)
			log.Debug("result not verified: no other provider answered", "method", method)
			continue
		}

		value, outcome := ch.verify(first.Result, other.Result)
		switch outcome {
		case verify.Agree:
			setResult(ch.index, value)
			metrics.RecordVerifiedResult(endpoint.Name, method, metrics.VerifyAgree)
		case verify.Inconclusive:
			metrics.RecordVerifiedResult(endpoint.Name, method, metrics.VerifyUnverified)
			log.Warn("result not verified: malformed result from other provider", "method", method, "provider", second.provider.Name, "result", rpc.FormatRawBody(string(other.Result)))
		case verify.Disagree:
			disputed = append(disputed, ch)
		}
	}

	// The answers each disputed request was last compared on.
	firsts := make(map[int]*rpc.Response, len(disputed))
	others := make(map[int]*rpc.Response, len(disputed))
	for _, ch := range disputed {
		firsts[ch.index] = result.res.Responses[at[ch.index]]
		others[ch.index] = second.byIndex[ch.index]
	}

	// Ask the same two once more. Providers a block apart disagree on a value
	// that changed in that block (e.g. a payment just arrived), and have
	// usually caught up with each other by now.
	if delay := endpoint.GetVerifyRetryDelay(); len(disputed) > 0 && delay > 0 && sleep(ctx, delay) {
		againFirst := goAsk(log, func() answers {
			return askProvider(ctx, endpoint, req, disputed, timing, log, result.provider)
		})
		againSecond := goAsk(log, func() answers {
			return askProvider(ctx, endpoint, req, disputed, timing, log, second.provider)
		})
		a, b := <-againFirst, <-againSecond

		var still []check
		for _, ch := range disputed {
			method := req.Requests[ch.index].Method
			ra, rb := a.byIndex[ch.index], b.byIndex[ch.index]

			// One didn't answer again: settle it on the answers already given.
			if ra == nil || ra.IsError() || rb == nil || rb.IsError() {
				still = append(still, ch)
				continue
			}

			value, outcome := ch.verify(ra.Result, rb.Result)
			switch outcome {
			case verify.Agree:
				setResult(ch.index, value)
				metrics.RecordVerifiedResult(endpoint.Name, method, metrics.VerifyAgreeAfterRetry)
				log.Debug("providers agree after asking again", "method", method)
				continue
			case verify.Disagree:
				// Settle it on the newer answers, closer in time to the third's.
				firsts[ch.index], others[ch.index] = ra, rb
			}
			still = append(still, ch)
		}
		disputed = still
	}

	var third answers
	if len(disputed) > 0 {
		third = askOthers(ctx, endpoint, req, disputed, timing, log, result.provider, second.provider)
	}

	for _, ch := range disputed {
		method := req.Requests[ch.index].Method
		first, other, tie := firsts[ch.index], others[ch.index], third.byIndex[ch.index]

		attrs := []any{
			"method", method,
			"params", rpc.FormatRawBody(string(req.Requests[ch.index].Params)),
			result.provider.Name, rpc.FormatRawBody(string(first.Result)),
			second.provider.Name, rpc.FormatRawBody(string(other.Result)),
		}

		if tie != nil && !tie.IsError() {
			attrs = append(attrs, third.provider.Name, rpc.FormatRawBody(string(tie.Result)))

			if value, outcome := ch.verify(first.Result, tie.Result); outcome == verify.Agree {
				setResult(ch.index, value)
				metrics.RecordVerifiedResult(endpoint.Name, method, metrics.VerifyMajority)
				log.Error("provider outvoted: its result disagrees with the other providers", append([]any{"provider", second.provider.Name}, attrs...)...)
				continue
			}
			if value, outcome := ch.verify(other.Result, tie.Result); outcome == verify.Agree {
				setResult(ch.index, value)
				metrics.RecordVerifiedResult(endpoint.Name, method, metrics.VerifyMajority)
				log.Error("provider outvoted: its result disagrees with the other providers", append([]any{"provider", result.provider.Name}, attrs...)...)
				continue
			}
		}

		responses[at[ch.index]] = &rpc.Response{
			Version: "2.0",
			ID:      first.ID,
			Error:   rpc.NewError(rpc.CodeProvidersDisagree, "providers disagree on the result"),
		}
		metrics.RecordVerifiedResult(endpoint.Name, method, metrics.VerifyDisagree)
		log.Error("providers disagree on the result, returning an error", attrs...)
	}

	return &forwardResult{
		res:      &rpc.BatchResponse{Responses: responses, IsBatch: result.res.IsBatch},
		provider: result.provider,
		dur:      result.dur,
	}
}

// askOthers sends the checked requests of req to the endpoint's providers
// other than skip, failing over as usual, and returns their answers. Only the
// checked requests are sent, so e.g. a transaction in the same batch is never
// broadcast twice.
func askOthers(ctx context.Context, endpoint *core.Endpoint, req *rpc.BatchRequest, checks []check, timing *servertiming.Header, log *slog.Logger, skip ...*core.Provider) answers {
	sub := &rpc.BatchRequest{IsBatch: len(checks) > 1}
	for _, ch := range checks {
		sub.Requests = append(sub.Requests, req.Requests[ch.index])
	}

	r, err := forwardWithFailover(ctx, endpoint, sub, timing, log, skip...)
	if err != nil {
		return answers{}
	}

	at := responseIndex(sub, r.res)
	byIndex := make(map[int]*rpc.Response, len(checks))
	for k, ch := range checks {
		byIndex[ch.index] = r.res.Responses[at[k]]
	}
	return answers{byIndex: byIndex, provider: r.provider}
}

// askProvider sends the checked requests of req to p alone, without failing
// over to any other provider.
func askProvider(ctx context.Context, endpoint *core.Endpoint, req *rpc.BatchRequest, checks []check, timing *servertiming.Header, log *slog.Logger, p *core.Provider) answers {
	var skip []*core.Provider
	for _, q := range endpoint.Providers {
		if q != p {
			skip = append(skip, q)
		}
	}
	return askOthers(ctx, endpoint, req, checks, timing, log, skip...)
}

// sleep waits for d and reports whether it did, rather than ctx being done
// first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// responseIndex returns, for each request in req, the index of its response in
// res. A batch response may arrive in any order, so responses are matched to
// requests by id when every request has its own id and each is answered
// exactly once; otherwise they're matched by position. The caller has checked
// that req and res have the same length.
func responseIndex(req *rpc.BatchRequest, res *rpc.BatchResponse) []int {
	at := make([]int, len(req.Requests))
	for i := range at {
		at[i] = i
	}
	if !req.IsBatch {
		return at
	}

	byID := make(map[string]int, len(res.Responses))
	for j, r := range res.Responses {
		id := r.GetID()
		if _, dup := byID[id]; dup || id == "" {
			return at
		}
		byID[id] = j
	}

	matched := make([]int, len(req.Requests))
	seen := make(map[string]bool, len(req.Requests))
	for i, sub := range req.Requests {
		id := sub.GetID()
		j, ok := byID[id]
		if !ok || seen[id] {
			return at
		}
		seen[id] = true
		matched[i] = j
	}
	return matched
}
