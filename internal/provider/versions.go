// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strconv"
	"strings"
)

// compareVersionCodes compares two ACM ClickHouse version codes by their
// leading numeric dotted components. Returns >0 if a is newer, <0 if older,
// 0 if equal up to the compared parts.
//
// Background: ACM returns version codes shaped `major.minor.patch.build.stream`
// — e.g. `25.8.16.10002.altinitystable`. `strings.Split` on `.` yields the
// five components; the first four are numeric, the fifth is a stream
// identifier (`altinitystable`, `altinityantalya`, etc.) that is never
// `strconv.Atoi`-parseable on its own.
//
// Stream suffix semantics:
//   - When both versions reach a non-numeric component at the same position
//     (typically the stream tag), they are considered equal — switching
//     streams at the same numeric version is not a downgrade.
//   - When only one version is non-numeric at a position, the numeric side
//     is treated as more specific (newer). In practice this only matters
//     if ACM ever returns a version like "25.8" with no stream tag against
//     "25.8.altinitystable" with one — unlikely but defended.
//
// Prefix-equivalent semantics: when one input is a strict numeric prefix of
// the other (e.g. `"25.8"` vs `"25.8.16.10002.altinitystable"`), they are
// equal. The shorter form means "this version family"; refusing the
// equivalence would mis-flag a config simplification as a downgrade.
//
// Antalya vs Stable at the SAME ClickHouse version: ACM ships these with
// different build numbers — Antalya at `*.20002.altinityantalya`, Stable at
// `*.10002.altinitystable`. The numeric build component (index 3) makes
// Antalya rank higher, so switching Antalya → Stable is correctly flagged
// as a downgrade by the build-number comparison at index 3, BEFORE the
// stream tag at index 4 is ever consulted. The "stream-equal" branch only
// fires when both build numbers match.
func compareVersionCodes(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		if ea != nil && eb != nil {
			// Both hit a non-numeric suffix at the same position: numeric
			// parts are equal; stream identity does not establish ordering.
			return 0
		}
		if ea != nil {
			return -1 // a's component is non-numeric; b is more specific
		}
		if eb != nil {
			return 1 // b's component is non-numeric; a is more specific
		}
		if na != nb {
			return na - nb
		}
	}
	// All common numeric components matched; one input is a prefix of the
	// other (or they're identical). Treat as equal — see prefix-equivalent
	// semantics above.
	return 0
}
