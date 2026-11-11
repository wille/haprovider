package solana

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// Real mainnet transactions, as getTransaction returned them (base64), with
// the signatures the chain knows them by.
const (
	legacyTx    = "AXFyMDgKmKgxKL/k/qz+aCWALgrtWCwRXXPTir61gEedxcMfSQKxe7DkjSVrfd00SlfDORCEsnVe3djvwX84wg4BAAoUV/GugHzDRK1hptLO52G4B4XQhgmWYoUb2sltJMLlt+ND5oE0yZmYHR2iE//wqA20eO095IHaCWJNKnvqg3KTRURIPY+ajQYasKHfG+KOTKrZr2LgTkshGmdbYxKbfsggSMgrfpAIR8gtwtQ+Pb323CBuYxGj7Jqpf6pVTjEeH65JI5p7ztw56KuSCk2FBaIlcHYTFnZ1j8hj/pfcEd1BAEoeQuy7bCLd20PCRgKYMrNiYet0LR/l+e3pmICfBcxAUTX+nVBSICNYO0X9y3elU9VsZrH5V0Ft1771sM8jCXBYGd7ndSsNcfSev2rxxpdhD86Oa9IVTglgUTY4N9DRxHPqbbY6Uoh5+af8RK6G4FbAXvhyUS1DVvaYlRrBUZBiyKQNd3obJ8MgoKn0aDNZI2knhSzglhQUld9n9BoERkAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAIyXJY9OJInxuz0QKRSODYMLWhOZ2v8QhASOe9jb6fhZAwZGb+UhFzL/7K26csOb57yM5bvF9xJrLEObOkAAAACycNZ/qYxRzwITBRNYliuvNXQr7VnJ2URenA0MhcfNkeWUyyMbSLajLo5HV0qgxjj6yNYqTQDnAhnHivbDcfdPBOnhL7yE6CbJMszp4mQMzhVZDBxic7CSVwi6O4UgsLwFSlNamSkhBk0k6HFg2jh8fDW13bySu4HkH6hAQQVEjQabiFf+q4GE+2h/Y0YYwDXaxDncGus7VZig8AAAAAABBt324ddloZPZy+FGzut5rBy0he1fWzeROoz1hX7/AKkG3fbh7nWP3hhCXbzkbM3athr8TYO5DSf+vfko2KGL/OU6Cu1yPdn65COcwX2DB2XO7dpoK/FGxtSybyr3LNBkCgwABQIP6gcACwYACQAOChMBAQsGAAYAEQoSAQELBgAJAA4KEwEBCwYABgARChIBAQ8RAgMECQYBBw4RABMSEA0PBQgazALDkTWRkc1+/v//w/7//xAnAgAAAAAAAQAPEAMCAAEHCQYOERMSEA0PBQgYcL9lqxyQf7t+/v//w/7//wIAAAAAAAEADwUCAAANDwg7fNR2W5hunRIDBgAAAQkMAAkDIE4AAAAAAAA="
	legacyTxSig = "3GZ4FuSkMiijLnYfmYPSWfvULuT5VoHh7rBenM6cJSubGtWYFnJwCu69S3GiZ1k8WYTVY25TdtoFpAs4MeEAE4tV"

	v0Tx    = "AQa6PqSBlh8+30At/b0M0Uu5XnVaQ6O50YNueJw1hABX7tFN5ZNpTFodQsINLOjV4Ca5otkA2AbLxuUJ9sajUQWAAQARG6Kh3wiOVddP86Dp2JLK3sZ1cbzLyzWso93AUVo9RY7Q7ML5sEwqreQvTN7+mnMNTcJQv8qliSIl94BAO42sN/ln/oaQWGISLkBJ9+hj6G9AeCew4gao7dVBb+AFHqrLtUi4fioK1Dhy7KmLf7ueC7XSzP8Zwyj+/n/2igzW9hs68tyUwvJy0xcO17LMTin7N3cMmABkP2ytB3Da1UAdApmcYReThDsgd344tUE5bxnPUcLDiaheWRuDZOBdRCwkZQe0ZyjFA6fIFZjsUWe5tjKg2nvc6Y8HxZZ7EO1veKHOCt5eJMCba2Wkp+45DaEtmlP7KJ5UDOlRsU9fOwVFvIsbCgpKok4PSuaza+WWGYnYBMB8EUOeiEWW3VoWZGa9Ggx9HZ2C2BoSA0B31CW/4sADlNUw2ChFC7R4GayjACORAwZGb+UhFzL/7K26csOb57yM5bvF9xJrLEObOkAAAACMlyWPTiSJ8bs9ECkUjg2DC1oTmdr/EIQEjnvY2+n4WQabiFf+q4GE+2h/Y0YYwDXaxDncGus7VZig8AAAAAABAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAG3fbh12Whk9nL4UbO63msHLSF7V9bN5E6jPWFfv8AqQwU3vyCXsZ2lCUIGLtlQGX0KY0xVtVxtNT4CQwY6ahjiQumRP4fVaoZ8RzS0uwU0yM7bgpL6u73K2mFjiHhcNbujGOqN0v2+U9NmqCH5bcmQL1Sipzhspj0HaEjb3EWb2ODcwAOoiyyZNNK/2SgS176v7t03c0EiZexmBVH19EQBt324e51j94YQl285GzN2rYa/E2DuQ0n/r35KNihi/zlSnCVKIOfYcC5uGB5iRwTkhbkenG2L7c77HIWlFh0XmTd3ysoCa+2oChCjrOQIcAiLxTXwry5doQSjJeRg4kAo9e7En5YrcEspo+DQ37C4cP5gg3pPlj5F4opGN2q97RBJG7MfXj+geQXc6RpZUGZN5I6B2RHl99vPrUUQmAQyww1/6kFWo5Wjaj3vAdWFSdM8ckspB9AAJxRaqQUwnxw9QbAK6KpTBOQB06ZRZ0IVzUDggfCnm73kRpifrrSDfWHcBV+6+tnimVduZs39rEybHZX25DPuKh6vvjHtvLIaTawGJG5IbEDr7y75fKZ55tt6Y13zOAh9fKZASzN7rA1BgoABQIgoQcACwYAAQAMDQ4BAQ0CAAEMAgAAAGsESgAAAAAADgEBAREPGgIAEBEMAwEEBRIGEw4NCxQPBxUWCBcYGRoJGcYuFVK02ehwawRKAAAAAACM6MyFAAAAAAAOAwEAAAEJAA=="
	v0TxSig = "8oUUDeMpWPBKG6L7gLFoFBC4gTHnuLqxiNsChXSXu89vW2B6PkzMAMFyuVLC4EDdtHcJCT8iS6YdwVmX9zaSp4c"

	// A v1 transaction: version byte 0x81, signatures at the end.
	v1Tx = "gQEAEg8AAAAshwEztH37Cvyo83WJnk0vQ83AXKKeez2ulDO7PaERUwUmCHHJBF+qK5IenK0it0FnNgkwWzsmn4qmAUKv2Sg+NjYByCHzqPCP74jcMUJKdoCujJaBcEzx5fXIjieZt/mCIQTHvqsQDjONaifM89b2YbDF+tBJaW2aW0CVVQTtPY9LFmX8QUmYBK5Oo1MtvaQds0vki6NhDhuYe4/geW48MfgzVosRMbVrjnhRrsnM0Lisfb9/n7GsWXB/EHWDqA5sDTtg9SzsFkRMLmvViEstCPm2F55eWe+8/bTiTDdaIklOPW5HLmekbqa0vQurnf014rTHLx1tWcLquVyUJXOtIvE/82S1NBW3zQc3kgMrfvJqyJg9ijXLtb/WjA1TnKQxg0T8H3j5SjPQkJxea1+wIVcK2Nutjej9s9IO0c2Z645OTrV1L4PBvC1JgVxOpeotDL1ndWvnY+vaeyeAVzhffAdXIlTEaoNoBR5sbJGTuX85X/fW/R4aRdLfy805YHR0jHp46G1M8xEOWHVym7KdbCpoRLYizXNEf91Nv3qQ8oA4mg8RZjo6cl2INwF0vbWlZKppP1HTilWRt20wPkKI7V+l24Z8ecBh8dnAhWVZ9aS0LXVYP1PkU+UZ7RxwCct1DbMEkbmvmOpAsYA0WEUmA76y7+AJXtUuNTmCmK/9OOvFuHDhLdN5iRVh0un6jyZDGDTrc28vJPwqKk3/H9XcpN/Xqo+wYNgpG0xNR12v92LJa9wNrOs2wBLq0S7TqUhBYdkQZDoRohFhvOfpzeA4+per21KmsORHVzguDljqlO5Y2e/091XcoKMfk8d73kpfZTt+sOmceGYP6zyf5/pGUQ7yy7m3YO3bGFcGMDBjrTPXtXKW6gLU4DNeMc6vpMxC3QAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAVbg9pNmWs9E2xVovxdbqlGJy5f10v87ZV0rtv1tGLAGm4hX/quBhPtof2NGGMA12sQ53BrrO1WYoPAAAAAAAQbd9uHXZaGT2cvhRs7reawctIXtX1s3kTqM9YV+/wCpBt324e51j94YQl285GzN2rYa/E2DuQ0n/r35KNihi/wMNf+pBVqOVo2o97wHVhUnTPHJLKQfQACcUWqkFMJ8cBE7ysTClMUtauCTUWTwSRyaUfdLyD2MgqvVqUFimUdhOoZeae4PVIDKvPZjV+TcLxjVjUXB6nSJ+zcj2Xk8cqZBV7BYDzHF/ORKYlgtvPnXjudZQ6CEo5OzUDaNIomTCEvZScQ2AsM/IHeQ7RajUkyhuZdc8SGiqQz/7H34torNUaq/viob14PRkh/mt61ggd8XkNvYHaDJHWNPEJkJN6xWzdVusnFXJiJt35Li40in0QfPZriy/T5lCPNHB1KMT2+atKTxlY3AqclMP7csB5lYQ+2kheOiTxDGk5n4GZQPjJclj04kifG7PRApFI4NgwtaE5na/xCEBI572Nvp+Fms8TbrAfwcTog9I8i1hEq1mjf2at1XxemsO1PgWdNcZMb6evO+2606PWXzaqvJdDGxu+TC0vbg5HymAgNFL11h+gkRpUhjQS1jH04HhwMpbANfDRMzoNnIg41ztxD+bi0K8cNDIYjKOmNRNaE6GJUazr0p5qwtrqYL8lyHXEAL86APAAAAAAAAgBoGAAAAAAQhBgEAIQYBABQCDAAeIxoAFAIMAAAMAB8UGAEACQAjFBcBAAICAAAAEwIAAAAAAAAbHxYYFyEQAQgNEQcOAAwLBQMaJAoSIBkUIhULFwkdHAYPE8+i9hKqbb83qMsBAAAAAACKNeneAAAAAAEAAAQCAAAAECcAAAAAAAC6RFniBU9hz4zAvfV0ygUbSoPFBTjnjfz3QW74IY4bzd8ozPWqrbJNSpkjlLTVw0bDPQh24zZkjjHd9+n57vIP"
)

func TestTransactionSignature(t *testing.T) {
	for tx, want := range map[string]string{legacyTx: legacyTxSig, v0Tx: v0TxSig} {
		raw, _ := base64.StdEncoding.DecodeString(tx)

		// As web3.js sends it (base64), and in the default encoding (base58).
		for _, params := range []any{
			[]any{tx, map[string]string{"encoding": "base64"}},
			[]any{base58Encode(raw)},
		} {
			b, _ := json.Marshal(params)
			got, err := transactionSignature(b)
			if err != nil {
				t.Fatalf("transactionSignature: %v", err)
			}
			if got != want {
				t.Errorf("signature = %s, want %s", got, want)
			}
		}
	}
}

// TestTransactionSignature_V1: a v1 transaction isn't read, rather than risk
// naming the wrong signature; it's still broadcast, just not checked.
func TestTransactionSignature_V1(t *testing.T) {
	b, _ := json.Marshal([]any{v1Tx, map[string]string{"encoding": "base64"}})
	if sig, err := transactionSignature(b); err == nil {
		t.Errorf("transactionSignature = %s, want an error for a v1 transaction", sig)
	}
}

func TestTransactionSignature_Invalid(t *testing.T) {
	for _, params := range []string{
		`[]`,
		`[1]`,
		`["not base58 0OIl"]`,
		`["AQ==",{"encoding":"base64"}]`,
		`["AA==",{"encoding":"base64"}]`,
		`["x",{"encoding":"jsonParsed"}]`,
	} {
		if _, err := transactionSignature(json.RawMessage(params)); err == nil {
			t.Errorf("transactionSignature(%s): want an error", params)
		}
	}
}

func TestBase58(t *testing.T) {
	// Bitcoin's base58 test vectors (same alphabet).
	for hexIn, want := range map[string]string{
		"":                     "",
		"61":                   "2g",
		"626262":               "a3gV",
		"636363":               "aPEr",
		"516b6fcd0f":           "ABnLTmg",
		"bf4f89001e670274dd":   "3SEo3LWLoPntC",
		"572e4794":             "3EFU7m",
		"ecac89cad93923c02321": "EJDM8drfXA6uyA",
		"10c8511e":             "Rt5zm",
		"00000000000000000000": "1111111111",
		"00eb15231dfceb60925886b67d065299925915aeb172c06647": "1NS17iag9jJgTHD1VXjvLCEnZuQ3rJDE9L",
	} {
		in := mustHex(t, hexIn)
		if got := base58Encode(in); got != want {
			t.Errorf("base58Encode(%s) = %q, want %q", hexIn, got, want)
		}
		back, err := base58Decode(want)
		if err != nil || string(back) != string(in) {
			t.Errorf("base58Decode(%q) = %x, %v; want %s", want, back, err, hexIn)
		}
	}
}

func TestBroadcaster(t *testing.T) {
	c := &Chain{}
	spec := c.Broadcaster("sendTransaction")
	if spec == nil {
		t.Fatal("sendTransaction should be broadcast")
	}
	if c.Broadcaster("getBalance") != nil {
		t.Error("getBalance should not be broadcast")
	}
	if !spec.AlreadyKnown("rpc -32002: Transaction simulation failed: This transaction has already been processed") {
		t.Error("already processed should count as already known")
	}
	if spec.AlreadyKnown("rpc -32002: Transaction simulation failed: Blockhash not found") {
		t.Error("blockhash not found is a rejection")
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
