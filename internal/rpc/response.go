package rpc

import (
	"encoding/json"
	"fmt"
)

type Response struct {
	Version string `json:"jsonrpc"`

	// ID might be a string or number
	ID     any    `json:"id,omitempty"`
	Result any    `json:"result"`
	Method string `json:"method,omitempty"`
	Params any    `json:"params,omitempty"`
	Error  any    `json:"error,omitempty"`
}

func (r *Response) IsError() bool {
	return r.Error != nil
}

func (r *Response) GetError() (int, error) {
	err, ok := r.Error.(map[string]any)
	if !ok {
		return 0, fmt.Errorf("unable to parse error: %v", r.Error)
	}

	errorCode := err["code"].(float64)

	return int(errorCode), fmt.Errorf("%d %s", int(errorCode), err["message"])
}

func SerializeResponse(req *Response) []byte {
	b, err := json.Marshal(req)

	if err != nil {
		panic(err)
	}

	return b
}

func DecodeResponse(b []byte) (*Response, error) {
	var res Response
	err := json.Unmarshal(b, &res)
	if err != nil {
		return nil, err
	}

	return &res, nil
}
