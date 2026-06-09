// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"encoding/json"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// BackupRequest is the body for POST /cluster/{id}/backup (the backup-schedule
// sub-domain, design §7.2 — config-only, no poll). The backup configuration is
// a schema-less object in the spec.
//
// TODO(spike): replace the raw Options passthrough with a hand-written typed
// struct once the spike captures the backupOptions shape (the env.json fixture
// shows fields like provider/bucket/schedule/day/time/keep/compressionFormat,
// but the per-cluster body shape must be confirmed).
type BackupRequest struct {
	// Options carries the backup-schedule configuration object verbatim.
	Options json.RawMessage `json:"backupOptions,omitempty"`
}

// ConfigureBackup sets the backup schedule for a cluster
// (POST /cluster/{id}/backup). Config-only; no polling required.
func (c *Client) ConfigureBackup(ctx context.Context, clusterID int64, req BackupRequest) error {
	args := map[string]string{"id": clusterIDArg(clusterID)}
	return c.doJSON(ctx, wire.OpClusterBackupCreate, args, req, nil)
}
