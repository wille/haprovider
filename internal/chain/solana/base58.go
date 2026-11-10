package solana

import (
	"fmt"
	"math/big"
	"strings"
)

var big58 = big.NewInt(58)

// base58Encode encodes b in base58, with a leading '1' per leading zero byte.
func base58Encode(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}

	n := new(big.Int).SetBytes(b)
	var out []byte
	mod := new(big.Int)
	for n.Sign() > 0 {
		n.DivMod(n, big58, mod)
		out = append(out, base58Alphabet[mod.Int64()])
	}
	for range zeros {
		out = append(out, '1')
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// base58Decode decodes a base58 string, with a leading zero byte per leading
// '1'.
func base58Decode(s string) ([]byte, error) {
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}

	n := new(big.Int)
	for _, r := range s {
		i := strings.IndexRune(base58Alphabet, r)
		if i < 0 {
			return nil, fmt.Errorf("invalid base58 character %q", r)
		}
		n.Mul(n, big58)
		n.Add(n, big.NewInt(int64(i)))
	}

	return append(make([]byte, zeros), n.Bytes()...), nil
}
