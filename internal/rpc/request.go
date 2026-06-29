package rpc

import (
	"encoding/json"
)

type Request struct {
	Version string `json:"jsonrpc"`
	ID      ID     `json:"id,omitempty"`
	Method  string `json:"method,omitempty"`

	// Params is raw JSON, so a proxied request is forwarded upstream verbatim
	// without a decode→re-encode round-trip. NewRequest marshals Go values into it.
	Params json.RawMessage `json:"params,omitempty"`
}

// NewRequest builds a request, marshaling params (a Go value) into raw JSON. A
// nil params is left unset so it's omitted from the wire output.
func NewRequest(id string, method string, params any) *Request {
	r := &Request{
		Version: "2.0",
		ID:      StringID(id),
		Method:  method,
	}
	if params != nil {
		if b, err := json.Marshal(params); err == nil {
			r.Params = b
		}
	}
	return r
}

func DecodeRequest(b []byte) (*Request, error) {
	var req Request
	err := json.Unmarshal(b, &req)
	if err != nil {
		return nil, err
	}

	return &req, nil
}

func SerializeRequest(req *Request) ([]byte, error) {
	return json.Marshal(req)
}

func (r *Request) GetID() string {
	return r.ID.String()
}
