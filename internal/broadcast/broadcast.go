// Package broadcast describes how a chain's transaction-submitting methods are
// broadcast to every provider of an endpoint: the hash a provider should answer
// with, and the errors that mean it already had the transaction. Each chain
// decides which of its methods submit a transaction (see
// chain.Chain.Broadcaster).
package broadcast

import (
	"encoding/json"
	"strings"
)

// Spec is how calls to one transaction-submitting method are broadcast.
type Spec struct {
	// Hash returns the hash of the transaction a call with params submits, in
	// the form providers answer with. An error means it can't be computed (a
	// format we don't read): the transaction is still broadcast, but a
	// provider's answer is taken without checking it.
	Hash func(params json.RawMessage) (string, error)

	// SameHash reports whether two hashes name the same transaction.
	SameHash func(a, b string) bool

	// AlreadyKnown reports whether an error message means the provider already
	// had the transaction, which once one node has it is the usual answer from
	// the others.
	AlreadyKnown func(message string) bool
}

// ContainsAny reports whether s contains any of substrs, ignoring case.
func ContainsAny(s string, substrs ...string) bool {
	s = strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(s, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
