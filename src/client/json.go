package client

import (
	"bytes"
	"encoding/json"
)

// JSONEqual reports whether two JSON payloads are semantically equal,
// ignoring whitespace and object key order. It is used by the data source
// clients to detect unchanged API responses before persisting them. Payloads
// that do not decode are never equal.
func JSONEqual(a, b []byte) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	ca, err := json.Marshal(va)
	if err != nil {
		return false
	}
	cb, err := json.Marshal(vb)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}
