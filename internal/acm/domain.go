// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// This file defines the clean DOMAIN types the provider layer consumes, plus
// the wire<->domain coercion for the KNOWN scalar fields. The ACM API is
// loosely typed: integer-valued fields arrive as JSON strings ("2") or numbers
// (13128), and several boolean-ish fields arrive as 0|1. The domain layer
// absorbs that at the boundary so the Terraform layer sees int64/bool.
//
// Opaque-object fields (datadogSettings, backupOptions, alertsSettings,
// chGuardSettings, uptimeSettings, options, objectStorages, endpointsEnabled,
// annotations, customLBAnnotations, altinityOptions, backupConfigModifications,
// and the launch-only zookeeperOptions/keeperOptions) are carried through as
// raw JSON. TODO(spike): replace the raw passthrough with hand-written typed
// nested structs once the spike captures their concrete shapes.

// ---- scalar coercion helpers (the known loose-typing rules) ----

// jnToInt64 coerces a wire.Number ("2" / 2 / true / "") to int64. Empty/absent
// values become 0.
func jnToInt64(n wire.Number) (int64, error) {
	s := n.Number.String()
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// int64ToString renders an int64 as the string form ACM expects for the
// string-int fields (nodes/shards/replicas).
func int64ToString(v int64) string {
	return strconv.FormatInt(v, 10)
}

// stringToInt64 parses a string-int ("2") to int64; empty becomes 0.
func stringToInt64(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// countJSONArray returns the element count of a JSON array (0 for empty,
// non-array, or unparseable). Used for Cluster.nodes, which responses return as
// an array of node objects.
func countJSONArray(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0
	}
	return int64(len(arr))
}

// jsonStringOrArrayToCSV normalizes an ACM field that is SENT as a comma-string
// ("a,b") but RETURNED as a JSON array (["a","b"]) — e.g. DbUser.networks and
// DbUser.databases. It returns the comma-joined form so the domain value
// round-trips against the configured string. A bare JSON string is returned
// as-is; empty/null/unparseable yields "".
func jsonStringOrArrayToCSV(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return strings.Join(arr, ",")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// jnToFloat64 coerces a wire.Number to float64.
func jnToFloat64(n wire.Number) (float64, error) {
	s := n.Number.String()
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

// intBoolToBool maps the ACM 0|1 integer convention to bool. Any non-zero value
// is true.
func intBoolToBool(v int64) bool { return v != 0 }

// boolToIntBool maps bool to the ACM 0|1 integer convention.
func boolToIntBool(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// ---- domain types ----

// Cluster is the clean domain view of a ClickHouse cluster. Known scalars are
// coerced; opaque-object fields are carried as raw JSON (TODO(spike)).
type Cluster struct {
	ID            int64
	Name          string
	Type          string
	Role          string
	Status        string
	State         string
	SystemVersion string
	Endpoint      string
	EndpointHTTP  string

	// Topology — string-ints on the wire.
	Nodes    int64
	Shards   int64
	Replicas int64

	// Networking / flags — the spec declares these as bool; the domain keeps
	// them bool. (If a future capture proves a 0|1 wire form, coerce here.)
	Secure        bool
	ZoneAwareness bool
	MysqlProtocol bool
	MysqlPort     int64

	IDEnvironment int64
	IDParent      int64
	IDZookeeper   int64
	KeeperName    string
	Timezone      string
	Uptime        string

	// PublicEndpoint and ReplicateSchema are write-only at launch; ACM doesn't
	// echo them in responses. Kept here for future use if a status endpoint
	// surfaces them. clusterFromWire intentionally leaves them at the zero
	// value and the provider preserves the plan-side bool through Read.
	PublicEndpoint  bool
	ReplicateSchema bool

	// AdminPass is write-only at the API (never returned on Read). The provider
	// preserves it from prior state; see design §7.1/§9.
	AdminPass string

	// Opaque object fields — raw passthrough. TODO(spike): strong typing.
	DatadogSettings           json.RawMessage
	BackupOptions             json.RawMessage
	AlertsSettings            json.RawMessage
	ChGuardSettings           json.RawMessage
	UptimeSettings            json.RawMessage
	Options                   json.RawMessage
	ObjectStorages            json.RawMessage
	EndpointsEnabled          json.RawMessage
	Annotations               json.RawMessage
	CustomLBAnnotations       json.RawMessage
	AltinityOptions           json.RawMessage
	BackupConfigModifications json.RawMessage
	AlternateEndpoints        json.RawMessage
}

// User is the clean domain view of a cluster DB user
// (components.schemas.DbUser).
type User struct {
	ID               int64
	Login            string
	Networks         string
	Databases        string
	AccessManagement bool
	IDCluster        int64
	IDProfile        int64

	// Password is write-only at the API (never returned on Read). Preserved
	// from prior state; see design §9.
	Password string
}

// Profile is the clean domain view of a settings profile
// (components.schemas.Profile).
type Profile struct {
	ID          int64
	Name        string
	Description string
	IDCluster   int64
}

// Setting is a single cluster-level setting. Cluster `settings` is a bare
// object in the spec (no named components.schemas entry), so this is
// hand-modeled rather than generated. TODO(spike): confirm the exact field set
// (name/value/... ) from a captured /cluster/{cluster}/settings payload.
type Setting struct {
	// ID is the ACM-internal setting id (carried in computed state for
	// update/delete), when present.
	ID int64
	// Name is the config-stable key.
	Name string
	// Value is the setting value.
	Value string
}

// Environment is the clean domain view of an Altinity.Cloud environment
// (components.schemas.Environment). Only the fields the provider needs are
// promoted to typed scalars; the rest are intentionally omitted.
type Environment struct {
	ID             int64
	Name           string
	NormalizedName string
	DisplayName    string
	Type           string
	Domain         string
	Status         string
	State          string
	IDOwner        int64
	IDParent       int64
}

// NodeType is the clean domain view of an environment node type
// (components.schemas.NodeType). cpu/memory/capacity are string-ints on the
// wire.
type NodeType struct {
	ID            int64
	Scope         string
	Code          string
	Name          string
	StorageClass  string
	IsSpot        bool
	CPU           float64
	Memory        int64
	Capacity      int64
	IDEnvironment int64
}

// ---- wire -> domain coercion ----

// clusterFromWire coerces a wire.Cluster into the domain Cluster. It returns an
// error if a known scalar field fails to parse.
func clusterFromWire(w *wire.Cluster) (Cluster, error) {
	if w == nil {
		return Cluster{}, nil
	}
	var c Cluster
	var err error
	if c.ID, err = jnToInt64(w.ID); err != nil {
		return c, err
	}
	// nodes is an array of node objects in responses (the spec mis-declares it as
	// a string); expose the count.
	c.Nodes = countJSONArray(w.Nodes)
	if c.Shards, err = stringToInt64(w.Shards); err != nil {
		return c, err
	}
	if c.Replicas, err = stringToInt64(w.Replicas); err != nil {
		return c, err
	}
	if c.MysqlPort, err = jnToInt64(w.MysqlPort); err != nil {
		return c, err
	}
	if c.IDEnvironment, err = jnToInt64(w.IDEnvironment); err != nil {
		return c, err
	}
	if c.IDParent, err = jnToInt64(w.IDParent); err != nil {
		return c, err
	}
	if c.IDZookeeper, err = jnToInt64(w.IDZookeeper); err != nil {
		return c, err
	}

	c.Name = w.Name
	c.Type = w.Type
	c.Role = w.Role
	c.Status = w.Status
	c.State = w.State
	c.SystemVersion = w.SystemVersion
	c.Endpoint = w.Endpoint
	c.EndpointHTTP = w.EndpointHTTP
	c.Secure = w.Secure.Bool()
	c.ZoneAwareness = w.ZoneAwareness.Bool()
	c.MysqlProtocol = w.MysqlProtocol.Bool()
	c.KeeperName = w.KeeperName
	c.Timezone = w.Timezone
	c.Uptime = w.Uptime
	c.AdminPass = w.AdminPass

	// Opaque passthrough. TODO(spike): strong typing.
	c.DatadogSettings = w.DatadogSettings
	c.BackupOptions = w.BackupOptions
	c.AlertsSettings = w.AlertsSettings
	c.ChGuardSettings = w.ChGuardSettings
	c.UptimeSettings = w.UptimeSettings
	c.Options = w.Options
	c.ObjectStorages = w.ObjectStorages
	c.EndpointsEnabled = w.EndpointsEnabled
	c.Annotations = w.Annotations
	c.CustomLBAnnotations = w.CustomLBAnnotations
	c.AltinityOptions = w.AltinityOptions
	c.BackupConfigModifications = w.BackupConfigModifications
	c.AlternateEndpoints = w.AlternateEndpoints
	return c, nil
}

// userFromWire coerces a wire.DbUser into the domain User.
func userFromWire(w *wire.DbUser) (User, error) {
	if w == nil {
		return User{}, nil
	}
	var u User
	var err error
	if u.ID, err = jnToInt64(w.ID); err != nil {
		return u, err
	}
	if u.IDCluster, err = jnToInt64(w.IDCluster); err != nil {
		return u, err
	}
	if u.IDProfile, err = jnToInt64(w.IDProfile); err != nil {
		return u, err
	}
	u.Login = w.Login
	// networks/databases are sent as comma-strings but returned as JSON arrays;
	// normalize back to the comma form so state round-trips.
	u.Networks = jsonStringOrArrayToCSV(w.Networks)
	u.Databases = jsonStringOrArrayToCSV(w.Databases)
	u.AccessManagement = w.AccessManagement.Bool()
	u.Password = w.Password
	return u, nil
}

// profileFromWire coerces a wire.Profile into the domain Profile.
func profileFromWire(w *wire.Profile) (Profile, error) {
	if w == nil {
		return Profile{}, nil
	}
	var p Profile
	var err error
	if p.ID, err = jnToInt64(w.ID); err != nil {
		return p, err
	}
	if p.IDCluster, err = jnToInt64(w.IDCluster); err != nil {
		return p, err
	}
	p.Name = w.Name
	p.Description = w.Description
	return p, nil
}

// environmentFromWire coerces a wire.Environment into the domain Environment.
func environmentFromWire(w *wire.Environment) (Environment, error) {
	if w == nil {
		return Environment{}, nil
	}
	var e Environment
	var err error
	if e.ID, err = jnToInt64(w.ID); err != nil {
		return e, err
	}
	if e.IDOwner, err = jnToInt64(w.IDOwner); err != nil {
		return e, err
	}
	if e.IDParent, err = jnToInt64(w.IDParent); err != nil {
		return e, err
	}
	e.Name = w.Name
	e.NormalizedName = w.NormalizedName
	e.DisplayName = w.DisplayName
	e.Type = w.Type
	e.Domain = w.Domain
	e.Status = w.Status
	e.State = w.State
	return e, nil
}

// nodeTypeFromWire coerces a wire.NodeType into the domain NodeType.
func nodeTypeFromWire(w *wire.NodeType) (NodeType, error) {
	if w == nil {
		return NodeType{}, nil
	}
	var nt NodeType
	var err error
	if nt.ID, err = jnToInt64(w.ID); err != nil {
		return nt, err
	}
	if nt.CPU, err = jnToFloat64(w.CPU); err != nil {
		return nt, err
	}
	if nt.Memory, err = jnToInt64(w.Memory); err != nil {
		return nt, err
	}
	if nt.Capacity, err = jnToInt64(w.Capacity); err != nil {
		return nt, err
	}
	if nt.IDEnvironment, err = jnToInt64(w.IDEnvironment); err != nil {
		return nt, err
	}
	nt.Scope = w.Scope
	nt.Code = w.Code
	nt.Name = w.Name
	nt.StorageClass = w.StorageClass
	nt.IsSpot = w.IsSpot.Bool()
	return nt, nil
}
