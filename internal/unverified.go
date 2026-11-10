package internal

import (
	"log/slog"
	"sync"
	"time"

	"github.com/wille/haprovider/internal/core"
)

// unverifiedWarnInterval is the least time between two warnings about results
// sent unverified on one endpoint. A var so tests can shorten it.
var unverifiedWarnInterval = time.Minute

// unverifiedWarnings throttles the warning about results sent unverified, per
// endpoint (*core.Endpoint to *unverifiedWarning).
var unverifiedWarnings sync.Map

// unverifiedWarning counts an endpoint's results sent unverified since its
// last warning.
type unverifiedWarning struct {
	mu    sync.Mutex
	last  time.Time
	count int
}

// warnUnverified logs a warning that a cross-checked result went out
// unverified, so a wrong answer from answered (the provider that gave it)
// couldn't have been caught. While a second provider is down every checked
// read goes out unverified, so it's logged at most once per
// unverifiedWarnInterval per endpoint, with the number since the last warning
// and the details of the latest one. Each one is still counted in
// haprovider_verified_results_total and logged at debug level.
func warnUnverified(endpoint *core.Endpoint, answered *core.Provider, log *slog.Logger, reason string, attrs ...any) {
	v, _ := unverifiedWarnings.LoadOrStore(endpoint, &unverifiedWarning{})
	w := v.(*unverifiedWarning)

	w.mu.Lock()
	w.count++
	if !w.last.IsZero() && time.Since(w.last) < unverifiedWarnInterval {
		w.mu.Unlock()
		return
	}
	count := w.count
	w.count = 0
	w.last = time.Now()
	w.mu.Unlock()

	// How many providers could have given a second opinion: usually 0 when
	// none answered, which means the endpoint is down to one provider.
	online := 0
	for _, p := range endpoint.Providers {
		if p != answered && p.IsOnline() {
			online++
		}
	}

	log.Warn("results not verified: "+reason, append([]any{
		"answered_by", answered.Name,
		"other_providers_online", online,
		"unverified_since_last_warning", count,
	}, attrs...)...)
}
