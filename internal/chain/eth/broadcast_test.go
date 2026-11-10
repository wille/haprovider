package eth

import (
	"encoding/json"
	"testing"
)

// Real mainnet transactions, as eth_getRawTransactionByHash returned them, with
// the hashes the chain knows them by.
const (
	legacyTx     = "0xf86c8251ae847ac5795e825208944800e78bad59596f3c9457aeb273c2b5a957680087038d7ea4c680008025a04a1f902b626cfcedcc18ecc7ed141f32e1270f3005d32d22bd70c4668fb1afbfa023ec80cc23d46aaa91c2fb89691c984c0297dfbe160f2d14116553738ef1752c"
	legacyTxHash = "0xa1d29893c0c2b674ca8067533c1094b3ccdf200ee43d5bfff384cdd2ed9d6905"

	dynamicFeeTx     = "0x02f8af018084ee6b280084f2bdb08182b58f94dac17f958d2ee523a2206206994597c13d831ec780b844a9059cbb0000000000000000000000009e48d6e12b76c031cc1d480ddc2b802a5d36da9d000000000000000000000000000000000000000000000000000000001dcd6500c080a0e0c296425a2da0232055f4b696f4b49de97e6120cb8c87181186463a80b5cd6aa07276e331e95fea3db79d00e2aa14b8fce33a9437be2baddbc064b49cf9c5441e"
	dynamicFeeTxHash = "0x74e2f2c83b2812f98a33b131642718defb25049fb177fc1e2b3df69ae9551fe4"
)

func TestRawTransactionHash(t *testing.T) {
	for raw, want := range map[string]string{
		legacyTx:     legacyTxHash,
		dynamicFeeTx: dynamicFeeTxHash,
	} {
		params, _ := json.Marshal([]string{raw})
		got, err := rawTransactionHash(params)
		if err != nil {
			t.Fatalf("rawTransactionHash: %v", err)
		}
		if got != want {
			t.Errorf("hash = %s, want %s", got, want)
		}
	}
}

func TestRawTransactionHash_Invalid(t *testing.T) {
	for _, params := range []string{`[]`, `["f86c"]`, `["0xzz"]`, `["0x"]`, `[1]`, `["0x01","0x02"]`} {
		if _, err := rawTransactionHash(json.RawMessage(params)); err == nil {
			t.Errorf("rawTransactionHash(%s): want an error", params)
		}
	}
}

func TestBroadcaster(t *testing.T) {
	spec := Broadcaster("eth_sendRawTransaction")
	if spec == nil {
		t.Fatal("eth_sendRawTransaction should be broadcast")
	}
	for _, method := range []string{"eth_sendTransaction", "eth_call", "eth_getBalance"} {
		if Broadcaster(method) != nil {
			t.Errorf("%s should not be broadcast", method)
		}
	}

	if !spec.SameHash(dynamicFeeTxHash, "0x74E2F2C83B2812F98A33B131642718DEFB25049FB177FC1E2B3DF69AE9551FE4") {
		t.Error("hashes differing only in letter case are the same")
	}

	for message, known := range map[string]bool{
		"rpc -32000: already known":                              true,
		"rpc -32000: ALREADY_EXISTS: already known":              true,
		"rpc -32010: AlreadyKnown":                               true,
		"rpc -32000: Known transaction":                          true,
		"rpc -32000: nonce too low":                              false,
		"rpc -32000: insufficient funds for gas * price + value": false,
	} {
		if got := spec.AlreadyKnown(message); got != known {
			t.Errorf("AlreadyKnown(%q) = %v, want %v", message, got, known)
		}
	}
}
