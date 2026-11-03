package metrics

import (
	"sync"
	"sync/atomic"
)

// OtherMethod is the method label used for any method that has not been
// admitted as a label of its own (see AdmitMethod).
const OtherMethod = "other"

// maxMethodLabels caps how many distinct method labels are ever created. It's a
// backstop: admission already requires a provider to have answered the method
// successfully, which a real chain does for a bounded set of methods.
const maxMethodLabels = 1000

var (
	methodLabels     sync.Map // method name -> struct{}
	methodLabelCount atomic.Int64
)

// AdmitMethod allows method to be used as a metric label. Call it once a
// provider has answered the method successfully: method names come from
// clients, and labeling by every name a client sends would let anyone create
// unbounded time series (memory growth here and in Prometheus). Names a
// provider rejects as unknown are never admitted and are counted as
// OtherMethod. Once maxMethodLabels names are admitted, no more are.
func AdmitMethod(method string) {
	if method == "" {
		return
	}
	if _, ok := methodLabels.Load(method); ok {
		return
	}
	if methodLabelCount.Add(1) > maxMethodLabels {
		methodLabelCount.Add(-1)
		return
	}
	if _, loaded := methodLabels.LoadOrStore(method, struct{}{}); loaded {
		methodLabelCount.Add(-1)
	}
}

// methodLabel returns the label to record for method: the method itself once
// admitted, "" for an unattributed request, and OtherMethod otherwise.
func methodLabel(method string) string {
	if method == "" {
		return ""
	}
	if _, ok := methodLabels.Load(method); ok {
		return method
	}
	return OtherMethod
}
