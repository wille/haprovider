package rpc

import (
	"encoding/json"
	"fmt"
)

// ID is a JSON-RPC request/response identifier. Per JSON-RPC 2.0 it must be a
// string, a number, or null — an object, array, or bool is rejected at decode
// time. The raw JSON is preserved so a proxied id is echoed back byte-for-byte.
type ID json.RawMessage

var (
	_ json.Unmarshaler = (*ID)(nil)
	_ json.Marshaler   = ID(nil)
)

// UnmarshalJSON validates that the id is a string, number, or null. The decoder
// hands us a complete, well-formed JSON token, so we only need to check its kind.
func (id *ID) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return fmt.Errorf("empty id")
	}

	switch b[0] {
	case '"': // string
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9': // number
	case 'n': // null
		if string(b) != "null" {
			return fmt.Errorf("invalid id: %s", FormatRawBody(string(b)))
		}
	default:
		return fmt.Errorf("invalid id: must be a string, number, or null, got %s", FormatRawBody(string(b)))
	}

	*id = append((*id)[:0], b...)
	return nil
}

// MarshalJSON emits the raw id, or null if unset.
func (id ID) MarshalJSON() ([]byte, error) {
	if len(id) == 0 {
		return []byte("null"), nil
	}
	return []byte(id), nil
}

// String returns a stable key for the id: the unquoted value for string ids and
// the literal text for numbers. Used for batch/subscription matching and logs.
func (id ID) String() string {
	if len(id) == 0 {
		return ""
	}
	if id[0] == '"' {
		var s string
		if err := json.Unmarshal(id, &s); err == nil {
			return s
		}
	}
	return string(id)
}

// IsNull reports whether the id is null or absent.
func (id ID) IsNull() bool {
	return len(id) == 0 || string(id) == "null"
}

// CheckResponseIDs returns an error if res can't be the answer to req: an
// answer whose id belongs to none of the requests, or two answers with the
// same id. Ids are compared by value (see String), so 1 and "1" match: some
// gateways change an id's type, and that's no reason to reject the answer. An
// error answer may have a null id, as JSON-RPC allows when the server couldn't
// read the request's id (e.g. a parse error or a rate limit). When req itself
// has a missing or repeated id, its answers can't be matched, so nothing is
// checked. The caller has checked that req and res have the same length.
func CheckResponseIDs(req *BatchRequest, res *BatchResponse) error {
	want := make(map[string]bool, len(req.Requests))
	for _, r := range req.Requests {
		if r.ID.IsNull() || want[r.ID.String()] {
			return nil
		}
		want[r.ID.String()] = true
	}

	seen := make(map[string]bool, len(res.Responses))
	for _, r := range res.Responses {
		if r.ID.IsNull() {
			if r.IsError() {
				continue
			}
			return fmt.Errorf("a result without an id")
		}

		id := r.ID.String()
		if !want[id] {
			return fmt.Errorf("an answer to id %s, which was not requested", FormatRawBody(string(r.ID)))
		}
		if seen[id] {
			return fmt.Errorf("two answers to id %s", FormatRawBody(string(r.ID)))
		}
		seen[id] = true
	}
	return nil
}

// StringID builds a string-valued id, used when we originate a request.
func StringID(s string) ID {
	if b, err := json.Marshal(s); err == nil {
		return ID(b)
	}
	return nil
}
