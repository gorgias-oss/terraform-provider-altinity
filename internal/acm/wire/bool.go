// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// Bool is the wire representation of the ACM API's loosely-typed boolean
// scalars. The OpenAPI spec declares these as JSON `boolean`, but live payloads
// deliver them inconsistently:
//
//   - DbUser.accessManagement: returned as JSON number (0/1) by /user/{id}
//     (DbuserEdit), even though the spec says boolean.
//   - Other fields may arrive as string-bools ("true"/"false") or as null.
//
// Bool decodes all of those forms into a canonical Go bool so the domain layer
// has a single representation. Marshals as a real JSON boolean.
//
// This type is hand-written (not generated) and is the target type the
// generator emits for every `boolean` field — same pattern as Number for
// numeric scalars.
type Bool struct {
	V bool
}

// Bool returns the underlying Go bool value for domain conversion.
func (b Bool) Bool() bool { return b.V }

// UnmarshalJSON accepts JSON boolean, JSON number (0/1), JSON string
// ("true"/"false"/"1"/"0"), and null forms.
func (b *Bool) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	switch {
	case len(data) == 0 || string(data) == "null":
		b.V = false
		return nil
	case string(data) == "true":
		b.V = true
		return nil
	case string(data) == "false":
		b.V = false
		return nil
	case data[0] == '"':
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		switch s {
		case "true", "1":
			b.V = true
		default:
			b.V = false
		}
		return nil
	default:
		// Bare JSON number — interpret 0 as false, any other number as true.
		n, err := strconv.ParseFloat(string(data), 64)
		if err != nil {
			return err
		}
		b.V = n != 0
		return nil
	}
}

// MarshalJSON renders as a real JSON boolean.
func (b Bool) MarshalJSON() ([]byte, error) {
	if b.V {
		return []byte("true"), nil
	}
	return []byte("false"), nil
}
