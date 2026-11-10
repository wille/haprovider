package verify

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func parseNumber(raw json.RawMessage) (uint64, error) {
	return strconv.ParseUint(string(raw), 10, 64)
}

func TestLowest(t *testing.T) {
	lowest := Lowest(parseNumber)

	got, outcome := lowest(json.RawMessage("105"), json.RawMessage("100"))
	assert.Equal(t, Agree, outcome)
	assert.JSONEq(t, "100", string(got), "the lower height is returned")

	got, outcome = lowest(json.RawMessage("100"), json.RawMessage("105"))
	assert.Equal(t, Agree, outcome)
	assert.JSONEq(t, "100", string(got))

	_, outcome = lowest(json.RawMessage("100"), json.RawMessage(`"x"`))
	assert.Equal(t, Inconclusive, outcome, "a malformed second answer can't be compared")

	_, outcome = lowest(json.RawMessage(`"x"`), json.RawMessage("100"))
	assert.Equal(t, Disagree, outcome, "a malformed first answer can't be vouched for")
}

func TestEqual(t *testing.T) {
	equal := Equal(JSON)

	got, outcome := equal(json.RawMessage(`{"a":1,"b":[2]}`), json.RawMessage(`{ "b":[2], "a":1 }`))
	assert.Equal(t, Agree, outcome, "key order and whitespace don't matter")
	assert.Equal(t, `{"a":1,"b":[2]}`, string(got), "the first answer is returned")

	_, outcome = equal(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":2}`))
	assert.Equal(t, Disagree, outcome)

	_, outcome = equal(json.RawMessage(`18446744073709551615`), json.RawMessage(`18446744073709551614`))
	assert.Equal(t, Disagree, outcome, "large numbers aren't rounded through float64")

	_, outcome = equal(json.RawMessage(`1`), json.RawMessage(`{`))
	assert.Equal(t, Inconclusive, outcome)

	_, outcome = equal(json.RawMessage(`{`), json.RawMessage(`1`))
	assert.Equal(t, Disagree, outcome)
}

func TestJSON_TrailingData(t *testing.T) {
	_, err := JSON(json.RawMessage(`1 2`))
	assert.Error(t, err)
}
