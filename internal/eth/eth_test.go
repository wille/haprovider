package eth

import (
	"testing"
)

func TestParseHexNumber(t *testing.T) {
	i, _ := ParseHexNumber("0x11")

	if i != 17 {
		t.Errorf("ParseHexNumber(%q) = %v, want %v", "0x11", i, 17)
	}

	_, err := ParseHexNumber("0x10000000000000000000000000000000000000000000000000000000000000000")
	if err == nil {
		t.Errorf("Did not fail on out of range number")
	}
}
