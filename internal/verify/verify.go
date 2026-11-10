// Package verify holds the building blocks for cross-checking one provider's
// answer against another's: the outcome of a comparison, and the comparisons
// chains share. Each chain decides which of its methods are cross-checked and
// how (see chain.Chain.Verifier).
package verify

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
)

// Outcome is the result of comparing two providers' answers to one request.
type Outcome int

const (
	// Agree means the two answers say the same thing.
	Agree Outcome = iota

	// Disagree means the answers contradict each other, or the first one is
	// malformed: at most one of them can be right.
	Disagree

	// Inconclusive means the second answer is malformed and can't be compared,
	// so the first stands unverified.
	Inconclusive
)

func (o Outcome) String() string {
	switch o {
	case Agree:
		return "agree"
	case Disagree:
		return "disagree"
	case Inconclusive:
		return "inconclusive"
	}
	return "unknown"
}

// Func compares the result one provider returned for a request (a) with
// another provider's result for the same request (b). It returns the result to
// send the client, which is a unless the comparison picks between them (see
// Lowest), and the outcome.
type Func func(a, b json.RawMessage) (json.RawMessage, Outcome)

// Lowest compares two block heights read with parse and returns the lower one.
// Two honest providers are often a block or two apart, so heights never
// disagree; taking the lower one means a provider reporting a too-high height
// can't inflate it (one reporting a too-low height only delays the client).
func Lowest(parse func(json.RawMessage) (uint64, error)) Func {
	return func(a, b json.RawMessage) (json.RawMessage, Outcome) {
		ha, errA := parse(a)
		hb, errB := parse(b)
		switch {
		case errB != nil:
			return a, Inconclusive
		case errA != nil:
			return a, Disagree
		case hb < ha:
			return b, Agree
		default:
			return a, Agree
		}
	}
}

// Equal compares two results by what normalize makes of them: they agree when
// normalize returns deeply equal values. normalize extracts the part of the
// result that matters and puts it in a canonical form, e.g. a hex quantity as
// a number, so encodings that differ only in form still agree.
func Equal(normalize func(json.RawMessage) (any, error)) Func {
	return func(a, b json.RawMessage) (json.RawMessage, Outcome) {
		na, errA := normalize(a)
		nb, errB := normalize(b)
		switch {
		case errB != nil:
			return a, Inconclusive
		case errA != nil:
			return a, Disagree
		case reflect.DeepEqual(na, nb):
			return a, Agree
		default:
			return a, Disagree
		}
	}
}

// JSON decodes raw into a generic value for comparison with reflect.DeepEqual:
// object key order and whitespace don't matter, and numbers are kept as their
// literal text (json.Number) rather than rounded through float64.
func JSON(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}
