package metrics

import (
	"fmt"
	"testing"
)

func resetMethodLabels() {
	methodLabels.Clear()
	methodLabelCount.Store(0)
}

// TestMethodLabel_RequiresAdmission: a client-chosen method name is recorded as
// "other" until a provider has answered it successfully.
func TestMethodLabel_RequiresAdmission(t *testing.T) {
	resetMethodLabels()
	t.Cleanup(resetMethodLabels)

	if got := methodLabel("eth_blockNumber"); got != OtherMethod {
		t.Fatalf("unadmitted method: got %q, want %q", got, OtherMethod)
	}
	AdmitMethod("eth_blockNumber")
	if got := methodLabel("eth_blockNumber"); got != "eth_blockNumber" {
		t.Fatalf("admitted method: got %q, want eth_blockNumber", got)
	}
	if got := methodLabel(""); got != "" {
		t.Fatalf("empty method: got %q, want empty", got)
	}
}

// TestMethodLabel_Capped: no more than maxMethodLabels names are ever admitted.
func TestMethodLabel_Capped(t *testing.T) {
	resetMethodLabels()
	t.Cleanup(resetMethodLabels)

	for i := 0; i < maxMethodLabels+50; i++ {
		AdmitMethod(fmt.Sprintf("m%d", i))
	}
	AdmitMethod("m0") // re-admitting an existing name must not consume a slot

	if n := methodLabelCount.Load(); n != maxMethodLabels {
		t.Fatalf("admitted %d labels, want %d", n, maxMethodLabels)
	}
	if got := methodLabel(fmt.Sprintf("m%d", maxMethodLabels)); got != OtherMethod {
		t.Fatalf("method past the cap: got %q, want %q", got, OtherMethod)
	}
}
