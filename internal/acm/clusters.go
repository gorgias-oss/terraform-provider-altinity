// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// LaunchRequest is the body for POST /environment/{environment}/clusters/launch.
// Known scalar fields are typed; the schema-less object fields are carried as
// raw JSON. The request schema is inline in the spec (no reusable component),
// so this is hand-modeled.
//
// Loose-typing note: nodes/shards/replicas are sent as strings (the ACM
// convention); secure/mysqlProtocol/publicEndpoint/replicateSchema are JSON
// booleans (the backend rejects/ignores int 0|1 — same as keeper `ha`).
type LaunchRequest struct {
	Name         string `json:"name"`
	Type         string `json:"type,omitempty"`
	Nodes        string `json:"nodes,omitempty"`
	Shards       string `json:"shards,omitempty"`
	Replicas     string `json:"replicas,omitempty"`
	NodeType     string `json:"nodeType,omitempty"`
	Memory       string `json:"memory,omitempty"`
	Version      string `json:"version,omitempty"`
	VersionImage string `json:"versionImage,omitempty"`
	Size         string `json:"size,omitempty"`
	Disks        string `json:"disks,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
	Throughput   string `json:"throughput,omitempty"`
	IOPS         string `json:"iops,omitempty"`
	DataPath     string `json:"dataPath,omitempty"`
	Zookeeper    string `json:"zookeeper,omitempty"`
	KeeperName   string `json:"keeperName,omitempty"`

	// JSON booleans (the ACM backend rejects/ignores int 0|1 for these — same
	// lesson as keeper `ha`). Sent unconditionally (no omitempty) so false is
	// explicit, matching the ACM UI payload.
	Secure          bool `json:"secure"`
	MysqlProtocol   bool `json:"mysqlProtocol"`
	PublicEndpoint  bool `json:"publicEndpoint"`
	ReplicateSchema bool `json:"replicateSchema"`
	ZoneAwareness   bool `json:"zoneAwareness,omitempty"`

	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	HTTPPort  int    `json:"httpPort,omitempty"`
	SSHPort   int    `json:"sshPort,omitempty"`
	MysqlPort int    `json:"mysqlPort,omitempty"`
	LBType    string `json:"lbType,omitempty"`

	// AdminUser is the cluster admin username (ACM defaults to "admin"); required
	// alongside AdminPass — launching with a password but no user errors.
	AdminUser string `json:"adminUser,omitempty"`
	// AdminPass is write-only; sent on launch, never returned on Read.
	AdminPass string `json:"adminPass,omitempty"`

	IPWhitelist string   `json:"ipWhitelist,omitempty"`
	Role        string   `json:"role,omitempty"`
	Timezone    string   `json:"timezone,omitempty"`
	Uptime      string   `json:"uptime,omitempty"`
	Azlist      []string `json:"azlist,omitempty"`

	SourceCluster string `json:"sourceCluster,omitempty"`
	BackupSource  string `json:"backupSource,omitempty"`

	// Opaque object fields — raw passthrough. TODO(spike): replace with
	// hand-written typed nested structs once the spike captures their shapes.
	ZookeeperOptions    json.RawMessage `json:"zookeeperOptions,omitempty"`
	KeeperOptions       json.RawMessage `json:"keeperOptions,omitempty"`
	BackupOptions       json.RawMessage `json:"backupOptions,omitempty"`
	RestoreOptions      json.RawMessage `json:"restoreOptions,omitempty"`
	DatadogSettings     json.RawMessage `json:"datadogSettings,omitempty"`
	UptimeSettings      json.RawMessage `json:"uptimeSettings,omitempty"`
	AlternateEndpoints  json.RawMessage `json:"alternateEndpoints,omitempty"`
	CustomLBAnnotations json.RawMessage `json:"customLBAnnotations,omitempty"`
	Annotations         json.RawMessage `json:"annotations,omitempty"`
	ChGuardSettings     json.RawMessage `json:"chGuardSettings,omitempty"`
}

// RescaleRequest is the body for PUT /cluster/{id}/rescale. The spec-declared
// fields are nodeType, shards, replicas, size, disks, onlyNew, throughput,
// forceNonReplicated, azlist; storageClass / iops are undocumented but
// historically accepted.
//
// `nodes` is intentionally NOT sent: total nodes in a ClickHouse cluster is
// derived from shards × replicas, and sending a separate, possibly stale
// `nodes` alongside an updated shards/replicas confuses ACM's SQL generator
// (observed live: plan changed replicas=1→2 with node_count stale at 1, and
// the rescale produced inconsistent state). Topology is authoritatively
// controlled by shards × replicas.
type RescaleRequest struct {
	Shards     string   `json:"shards,omitempty"`
	Replicas   string   `json:"replicas,omitempty"`
	NodeType   string   `json:"nodeType,omitempty"`
	Size       string   `json:"size,omitempty"`
	Disks      string   `json:"disks,omitempty"`
	Throughput string   `json:"throughput,omitempty"`
	Azlist     []string `json:"azlist,omitempty"`

	// Undocumented in the spec but historically accepted by ACM; kept for
	// backward compatibility with existing deployments that exercise these
	// fields. The spec covers them via NodeType/Shards/Replicas/Size.
	StorageClass string `json:"storageClass,omitempty"`
	IOPS         string `json:"iops,omitempty"`
}

// UpgradeRequest is the body for PUT /cluster/{id}/upgrade. Covers the version
// sub-domain (design §7.2).
type UpgradeRequest struct {
	Version      string `json:"version,omitempty"`
	VersionImage string `json:"versionImage,omitempty"`
}

// EditRequest is the body for POST /cluster/{id} (operationId ClusterEdit,
// "Modifies a cluster general information"). It covers config-only attributes
// ACM mutates in place on a running cluster — no relaunch, no data movement.
// Scalar fields are pointers so "leave unchanged" (nil, omitted from the JSON)
// is distinguishable from "clear" (pointer to the zero value, sent explicitly;
// note `omitempty` only drops nil pointers, never a pointer to "").
//
// Spec-confirmed fields NOT modeled here: mysqlProtocol and zoneAwareness are
// declared `integer enum [0,1]` in the edit body, but the same spec made that
// claim for the LAUNCH body where the backend live-rejects ints and wants JSON
// booleans (see LaunchRequest). Until a live spike settles the edit-side wire
// type, the corresponding attributes stay RequiresReplace in the resource.
type EditRequest struct {
	// Name renames the cluster. ACM derives endpoint hostnames from the
	// cluster name, so a rename also changes the cluster's endpoints.
	Name *string `json:"name,omitempty"`
	// Role is the cluster role code ("prod" or "dev").
	Role   *string `json:"role,omitempty"`
	LBType *string `json:"lbType,omitempty"`
	// IPWhitelist is the comma-separated CIDR allow-list applied to the
	// cluster's endpoints. An explicit empty string clears the list.
	IPWhitelist *string `json:"ipWhitelist,omitempty"`
	MysqlPort   *int    `json:"mysqlPort,omitempty"`
	Timezone    *string `json:"timezone,omitempty"`
	Uptime      *string `json:"uptime,omitempty"`

	// Opaque object passthroughs — same raw-JSON convention as LaunchRequest.
	// TODO(spike): typed nested blocks.
	UptimeSettings     json.RawMessage `json:"uptimeSettings,omitempty"`
	AlternateEndpoints json.RawMessage `json:"alternateEndpoints,omitempty"`
	DatadogSettings    json.RawMessage `json:"datadogSettings,omitempty"`
}

// clusterIDArg renders an int64 cluster id as the path-arg string.
func clusterIDArg(id int64) string { return strconv.FormatInt(id, 10) }

// LaunchCluster creates a cluster in the given environment
// (POST /environment/{environment}/clusters/launch). It returns the created
// cluster as read back from the launch response. Per design §7.1 the caller is
// responsible for persisting the returned id to state BEFORE polling.
func (c *Client) LaunchCluster(ctx context.Context, environmentID string, req LaunchRequest) (Cluster, error) {
	var w wire.Cluster
	args := map[string]string{"environment": environmentID}
	if err := c.doJSON(ctx, wire.OpClusterLaunch, args, req, &w); err != nil {
		return Cluster{}, err
	}
	return clusterFromWire(&w)
}

// ListClusters returns every cluster in an environment
// (GET /environment/{environment}/clusters).
func (c *Client) ListClusters(ctx context.Context, environmentID string) ([]Cluster, error) {
	var ws []wire.Cluster
	args := map[string]string{"environment": environmentID}
	if err := c.doJSON(ctx, wire.OpClusterList, args, nil, &ws); err != nil {
		return nil, err
	}
	out := make([]Cluster, 0, len(ws))
	for i := range ws {
		cl, err := clusterFromWire(&ws[i])
		if err != nil {
			return nil, err
		}
		out = append(out, cl)
	}
	return out, nil
}

// FindClusterInEnv locates a cluster by id within its environment via
// ListClusters. The bool reports whether it exists. This is the reliable
// existence check for drift detection: ACM returns 403 (not 404) for a GET of a
// non-existent or inaccessible cluster id (e.g. the id 0 left by a failed
// launch), so a missing cluster cannot be told apart from a permission error on
// a per-id GET. Listing the environment avoids that ambiguity.
func (c *Client) FindClusterInEnv(ctx context.Context, environmentID string, id int64) (Cluster, bool, error) {
	clusters, err := c.ListClusters(ctx, environmentID)
	if err != nil {
		return Cluster{}, false, err
	}
	for _, cl := range clusters {
		if cl.ID == id {
			return cl, true, nil
		}
	}
	return Cluster{}, false, nil
}

// FindClusterByName locates a cluster by name within its environment via
// ListClusters. Cluster names are unique per environment, so this is used to
// make Create idempotent: adopt an existing cluster of the same name instead of
// launching a duplicate (and to recover after a launch that started server-side
// but did not return cleanly).
func (c *Client) FindClusterByName(ctx context.Context, environmentID, name string) (Cluster, bool, error) {
	clusters, err := c.ListClusters(ctx, environmentID)
	if err != nil {
		return Cluster{}, false, err
	}
	for _, cl := range clusters {
		if cl.Name == name {
			return cl, true, nil
		}
	}
	return Cluster{}, false, nil
}

// GetCluster reads a cluster by id (GET /cluster/{id}). NOTE: ACM returns 403
// (not 404) for a non-existent id, so prefer FindClusterInEnv for existence
// checks; GetCluster is used where the environment is not yet known (import).
func (c *Client) GetCluster(ctx context.Context, id int64) (Cluster, error) {
	var w wire.Cluster
	args := map[string]string{"id": clusterIDArg(id)}
	if err := c.doJSON(ctx, wire.OpClusterShow, args, nil, &w); err != nil {
		return Cluster{}, err
	}
	return clusterFromWire(&w)
}

// GetClusterStatus returns the cluster's lifecycle status ("pending" ->
// "ready") for the poll loop. It reads the cluster OBJECT's status field via
// GetCluster, NOT the /cluster/{id}/status endpoint (use GetClusterAction
// for that). Use this for Create where the cluster transitions pending →
// online; use GetClusterAction for Update where the cluster stays online
// throughout a long-running operation.
func (c *Client) GetClusterStatus(ctx context.Context, id int64) (string, error) {
	cl, err := c.GetCluster(ctx, id)
	if err != nil {
		return "", err
	}
	return cl.Status, nil
}

// ClusterAction is the operation-level status of a cluster, read from
// /cluster/{cluster}/status. Captured live; the OpenAPI spec doesn't declare
// the response shape so this is hand-modeled from observed payloads.
//
// Action is the operation state. The only confirmed terminal-idle value is
// "Completed". During an operation it reports something else (likely
// "Rescaling", "Upgrading", etc. — to be confirmed). Anything other than
// "Completed" is treated as in-progress by PollUntilIdle.
//
// Progress is the percent-complete of the current/last operation, observed
// as 0 when idle (it doesn't pin at 100); not a reliable terminal signal on
// its own — use Action for the terminal check.
//
// HealthPassed/HealthTotal carry the cluster's operational health check
// summary; HealthPassed == HealthTotal == good. Used as a soft check.
type ClusterAction struct {
	Action       string
	Progress     int
	HealthTotal  int
	HealthPassed int
}

// clusterStatusResponse is the on-the-wire shape of /cluster/{cluster}/status.
// Hand-modeled (the spec body is empty). Sub-objects are pointers so a
// missing field decodes as nil rather than zero.
type clusterStatusResponse struct {
	Action         string `json:"action"`
	ActionProgress *struct {
		Percent int `json:"percent"`
	} `json:"actionProgress,omitempty"`
	Health *struct {
		Total  int `json:"total"`
		Passed int `json:"passed"`
	} `json:"health,omitempty"`
}

// GetClusterAction returns the operation-level status of a cluster (the
// /cluster/{cluster}/status endpoint, operationId ClusterStatus). Used by
// PollUntilIdle to wait until a mutating operation (rescale/upgrade/backup/
// password) has actually completed server-side — the top-level cluster status
// stays "online" throughout, so it's not a reliable signal for Update.
func (c *Client) GetClusterAction(ctx context.Context, id int64) (ClusterAction, error) {
	var r clusterStatusResponse
	args := map[string]string{"cluster": clusterIDArg(id)}
	if err := c.doJSON(ctx, wire.OpClusterStatus, args, nil, &r); err != nil {
		return ClusterAction{}, err
	}
	out := ClusterAction{Action: r.Action}
	if r.ActionProgress != nil {
		out.Progress = r.ActionProgress.Percent
	}
	if r.Health != nil {
		out.HealthTotal = r.Health.Total
		out.HealthPassed = r.Health.Passed
	}
	return out, nil
}

// RescaleCluster applies a compute/storage rescale (PUT /cluster/{id}/rescale).
// Poll-required: the caller polls to terminal-healthy afterwards (design §7.2).
func (c *Client) RescaleCluster(ctx context.Context, id int64, req RescaleRequest) error {
	args := map[string]string{"id": clusterIDArg(id)}
	return c.doJSON(ctx, wire.OpClusterRescale, args, req, nil)
}

// UpgradeCluster applies a version upgrade (PUT /cluster/{id}/upgrade).
// Poll-required (design §7.2).
func (c *Client) UpgradeCluster(ctx context.Context, id int64, req UpgradeRequest) error {
	args := map[string]string{"id": clusterIDArg(id)}
	return c.doJSON(ctx, wire.OpClusterUpgrade, args, req, nil)
}

// EditCluster applies in-place edits to a cluster's general configuration
// (POST /cluster/{id}). Config-only per design §7.2: the caller still polls
// to idle/healthy afterwards in case ACM schedules an operator action (e.g.
// re-rendering load balancer rules after an ipWhitelist change).
func (c *Client) EditCluster(ctx context.Context, id int64, req EditRequest) error {
	args := map[string]string{"id": clusterIDArg(id)}
	return c.doJSON(ctx, wire.OpClusterEdit, args, req, nil)
}

// TerminateCluster deletes a cluster (DELETE /cluster/{id}/{terminate}). The
// {terminate} path param is an int flag (confirmed live): "1" = terminate
// (delete the cluster and its data), "0" = stop. The literal word "terminate"
// 404s. After this returns, the caller polls until the cluster reports
// not-found (design §7.1).
func (c *Client) TerminateCluster(ctx context.Context, id int64) error {
	args := map[string]string{
		"id":        clusterIDArg(id),
		"terminate": "1",
	}
	return c.doJSON(ctx, wire.OpClusterRemove, args, nil, nil)
}
