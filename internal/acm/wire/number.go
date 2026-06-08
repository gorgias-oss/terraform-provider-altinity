// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// Number is the wire representation of the ACM API's loosely-typed numeric
// scalars. The spec declares many fields as integer/number, but live payloads
// deliver them inconsistently: as JSON numbers (13128), as string-ints ("2"),
// as JSON booleans (true/false, treated as 1/0 — e.g. environment.autoPush),
// or as null. Number decodes all of these into a canonical json.Number so the
// domain layer can coerce a single representation.
//
// This type is hand-written (not generated) and is the target type the
// generator emits for every integer/number field; see tools/specgen.
type Number struct {
	json.Number
}

// UnmarshalJSON accepts number, string, boolean, and null forms.
func (n *Number) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	switch {
	case len(data) == 0 || string(data) == "null":
		n.Number = ""
		return nil
	case data[0] == '"':
		// String form: unwrap to the inner literal.
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		n.Number = json.Number(s)
		return nil
	case string(data) == "true":
		n.Number = json.Number("1")
		return nil
	case string(data) == "false":
		n.Number = json.Number("0")
		return nil
	default:
		// Bare JSON number.
		n.Number = json.Number(data)
		return nil
	}
}

// MarshalJSON renders the canonical numeric form, or null when empty.
func (n Number) MarshalJSON() ([]byte, error) {
	if n.Number == "" {
		return []byte("null"), nil
	}
	// Validate it is a number; if not, quote it to stay valid JSON.
	if _, err := strconv.ParseFloat(string(n.Number), 64); err != nil {
		return json.Marshal(string(n.Number))
	}
	return []byte(n.Number), nil
}

// Std returns the underlying json.Number for the domain coercion helpers.
func (n Number) Std() json.Number { return n.Number }
