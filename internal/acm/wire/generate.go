// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

// Package wire holds the faithful, generated wire-level representation of the
// ACM REST API: the endpoint registry and the JSON structs for the named
// schemas we consume. Everything in *_gen.go is produced by tools/specgen from
// the vendored reference.json and MUST NOT be edited by hand.
//
// The hand-written domain layer (../domain.go) coerces these loose wire types
// into clean Go types for the provider.
package wire

//go:generate go run github.com/gorgias-oss/terraform-provider-altinity/tools/specgen
