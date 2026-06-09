// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveTimeoutsWithDefaults_NullUsesDefaults(t *testing.T) {
	defaults := resolvedTimeouts{create: 45 * time.Minute, update: time.Minute, delete: 30 * time.Minute}
	got, diags := resolveTimeoutsWithDefaults(context.Background(), types.ObjectNull(timeoutsAttrTypes()), defaults)
	require.False(t, diags.HasError())
	assert.Equal(t, defaults, got)
}

func TestResolveTimeoutsWithDefaults_ExplicitOverrides(t *testing.T) {
	defaults := resolvedTimeouts{create: 45 * time.Minute, update: time.Minute, delete: 30 * time.Minute}
	obj, d := types.ObjectValue(timeoutsAttrTypes(), map[string]attr.Value{
		"create": types.StringValue("5m"),
		"update": types.StringNull(),
		"delete": types.StringNull(),
	})
	require.False(t, d.HasError())

	got, diags := resolveTimeoutsWithDefaults(context.Background(), obj, defaults)
	require.False(t, diags.HasError())
	assert.Equal(t, 5*time.Minute, got.create)  // overridden
	assert.Equal(t, 30*time.Minute, got.delete) // default preserved
	assert.Equal(t, time.Minute, got.update)    // default preserved
}

// timeoutsAttrTypes mirrors the timeouts block's attribute types (create/update/
// delete strings) for constructing test objects.
func timeoutsAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"create": types.StringType,
		"update": types.StringType,
		"delete": types.StringType,
	}
}
