package eth

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseHexNumber parses a hex string and returns decimal number
// Note that this function does not handle large Ethereum balances in wei.
func ParseHexNumber(hex string) (uint64, error) {
	if !strings.HasPrefix(hex, "0x") {
		return 0, fmt.Errorf("hex number must start with 0x")
	}

	i, err := strconv.ParseUint(hex[2:], 16, 64)
	if err != nil {
		return 0, err
	}

	if i == math.MaxUint64 {
		return 0, fmt.Errorf("number is too large")
	}

	return i, nil
}
