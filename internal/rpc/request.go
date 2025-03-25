package rpc

import "encoding/json"

type Request struct {
	Version string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method,omitempty"`
	Params  any    `json:"params,omitempty"`
}

func NewRequest(id string, method string, params any) *Request {
	return &Request{
		Version: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
}

func SerializeRequest(req *Request) []byte {
	b, _ := json.Marshal(req)
	return b
}

func DecodeRequest(b []byte) (*Request, error) {
	var req Request
	err := json.Unmarshal(b, &req)
	if err != nil {
		return nil, err
	}

	return &req, nil
}
