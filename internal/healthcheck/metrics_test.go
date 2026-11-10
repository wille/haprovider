package healthcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/wille/haprovider/internal/core"
	"github.com/wille/haprovider/internal/metrics"
)

var (
	metricsOnce    sync.Once
	metricsHandler http.Handler
)

// scrape returns what the metrics endpoint serves. The handler registers the
// collectors globally, so it's built once per test binary.
func scrape(t *testing.T) string {
	t.Helper()
	metricsOnce.Do(func() { metricsHandler = metrics.MetricsHandler() })
	w := httptest.NewRecorder()
	metricsHandler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return w.Body.String()
}

// TestRun_BlockHeightMetric: a provider's reported block height is exposed,
// including when it's judged behind (that's when it's worth seeing), and the
// series goes away while the provider isn't reporting one.
func TestRun_BlockHeightMetric(t *testing.T) {
	const series = `haprovider_provider_block_height{endpoint="metrics-test",provider="p"}`

	p := ethProvider(t, healthyEthMethods()) // reports block 256
	p.Endpoint.Name = "metrics-test"
	assert.NoError(t, Run(context.Background(), p))
	assert.Contains(t, scrape(t), series+" 256")

	// Judged behind the median of its peers: still exposed.
	tol := 10
	p.Endpoint.BlockLagTolerance = &tol
	for _, name := range []string{"q1", "q2"} {
		q := &core.Provider{Name: name, Endpoint: p.Endpoint}
		q.RecordHeight(1000, time.Now())
		p.Endpoint.Providers = append(p.Endpoint.Providers, q)
	}
	if err := Run(context.Background(), p); assert.Error(t, err) {
		assert.Contains(t, err.Error(), "node is behind")
	}
	assert.Contains(t, scrape(t), series+" 256")
	assert.Contains(t, scrape(t), `haprovider_provider_block_height{endpoint="metrics-test",provider="q1"} 1000`)

	// On the wrong chain: no longer reporting, so no series.
	p.Endpoint.ChainID = "5" // the node reports chain 1
	assert.Error(t, Run(context.Background(), p))
	assert.NotContains(t, scrape(t), series)
}
