// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCompareVersionCodes(t *testing.T) {
	// Newer major / patch / build wins.
	assert.Greater(t, compareVersionCodes("25.8.16.10002.altinitystable", "24.8.14.10545.altinitystable"), 0)
	assert.Less(t, compareVersionCodes("22.3.15.33", "25.3.14.14"), 0)
	assert.Greater(t, compareVersionCodes("25.8.23.13", "25.8.16.10002.altinitystable"), 0)
	assert.Equal(t, 0, compareVersionCodes("25.8", "25.8"))

	// Stream switch at the same numeric prefix — equal, NOT a downgrade.
	assert.Equal(t, 0, compareVersionCodes("25.8.16.altinitystable", "25.8.16.altinityantalya"))
	// A higher numeric component still wins even when the trailing tag differs.
	assert.Greater(t, compareVersionCodes("25.8.16.altinitystable", "25.8.15.altinityantalya"), 0)

	// Prefix-equivalent: a strict numeric prefix is equal to the longer form,
	// so simplifying ("25.8" -> the long code, or vice versa) does not trip
	// the downgrade guard.
	assert.Equal(t, 0, compareVersionCodes("25.8", "25.8.16.10002.altinitystable"))
	assert.Equal(t, 0, compareVersionCodes("25.8.16.10002.altinitystable", "25.8"))
}

// TestCompareVersionCodes_AntalyaVsStableSameVersion locks the
// load-bearing real-world behavior: same major.minor.patch, Antalya has
// build .20002, Stable has build .10002. The numeric-build comparison at
// index 3 wins BEFORE the stream tag at index 4 is consulted.
//
// Antalya → Stable is a downgrade (build number went down).
// Stable → Antalya is an upgrade (build number went up).
// The stream tag itself doesn't establish ordering — the build number does.
func TestCompareVersionCodes_AntalyaVsStableSameVersion(t *testing.T) {
	antalya := "25.8.16.20002.altinityantalya"
	stable := "25.8.16.10002.altinitystable"

	assert.Greater(t, compareVersionCodes(antalya, stable), 0, "Antalya .20002 outranks Stable .10002")
	assert.Less(t, compareVersionCodes(stable, antalya), 0, "Stable .10002 < Antalya .20002 — switching is a downgrade")

	// Hypothetical: same build number, different streams (would only happen if
	// ACM ever ships them with the same .NNNNN). Treat as equal — neither
	// direction is a downgrade per the build-number rule.
	assert.Equal(t, 0, compareVersionCodes("25.8.16.10002.altinityantalya", "25.8.16.10002.altinitystable"))
}

// TestCompareVersionCodes_EmptyStrings locks defensive behavior: empty version
// strings (a misconfigured data source response, or an Unknown plan value that
// somehow leaked through) must not panic and must not be sorted ABOVE a real
// version (which would silently invert the downgrade guard).
func TestCompareVersionCodes_EmptyStrings(t *testing.T) {
	assert.LessOrEqual(t, compareVersionCodes("", "25.8.16.10002.altinitystable"), 0)
	assert.Equal(t, 0, compareVersionCodes("", ""))
}

// TestCompareVersionCodes_MixedNumericNonNumericAtSamePosition documents the
// edge case where one side reaches a non-numeric component while the other
// still has a numeric one at the same index. ACM doesn't ship versions in
// this shape today, but the function must not panic and must rank the
// numeric side as more specific.
func TestCompareVersionCodes_MixedNumericNonNumericAtSamePosition(t *testing.T) {
	// "25.8.altinitystable" — non-numeric at index 2.
	// "25.8.16"            — numeric at index 2.
	// 25.8.16 is more specific (has a real patch number).
	assert.Less(t, compareVersionCodes("25.8.altinitystable", "25.8.16"), 0)
	assert.Greater(t, compareVersionCodes("25.8.16", "25.8.altinitystable"), 0)
}
