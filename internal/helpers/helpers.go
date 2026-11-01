// Package helpers contains small shared utilities.
package helpers

import "encoding/json"

// RawString renders a raw JSON value as a plain string: JSON string values
// are unwrapped, anything else is kept as-is.
func RawString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return string(raw)
	}
	return s
}
