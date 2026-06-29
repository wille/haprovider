package rpc

import (
	"encoding/json"
	"testing"
)

func TestResponseMarshal_ErrorResponseOmitsResultMethodParams(t *testing.T) {
	res := &Response{
		Version: "2.0",
		ID:      ID("1"),
		Result:  nil,
		Method:  "",
		Params:  nil,
		Error:  NewError(-32601, "the method does not exist/is not available"),
	}

	b, err := SerializeResponse(res)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := m["result"]; ok {
		t.Fatalf("error response should not include result, got: %s", string(b))
	}
	if _, ok := m["method"]; ok {
		t.Fatalf("error response should not include method, got: %s", string(b))
	}
	if _, ok := m["params"]; ok {
		t.Fatalf("error response should not include params, got: %s", string(b))
	}
	if _, ok := m["error"]; !ok {
		t.Fatalf("error response should include error, got: %s", string(b))
	}
}

func TestResponseMarshal_NotificationOmitsResult(t *testing.T) {
	res := &Response{
		Version: "2.0",
		Method:  "eth_subscription",
		Params:  json.RawMessage(`{"subscription":"0x1","result":{"foo":"bar"}}`),
	}

	b, err := SerializeResponse(res)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := m["result"]; ok {
		t.Fatalf("notification should not include top-level result, got: %s", string(b))
	}
	if m["method"] != "eth_subscription" {
		t.Fatalf("expected method eth_subscription, got: %v", m["method"])
	}
}

func TestResponseMarshal_SuccessResponseIncludesNullResult(t *testing.T) {
	res := &Response{
		Version: "2.0",
		ID:      ID("1"),
		Result:  nil,
	}

	b, err := SerializeResponse(res)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := m["result"]; !ok {
		t.Fatalf("success response must include result even if null, got: %s", string(b))
	}
}
