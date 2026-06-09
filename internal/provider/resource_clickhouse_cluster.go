// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

// Ensure the resource satisfies the framework interfaces.
var (
	_ resource.Resource                     = (*clusterResource)(nil)
	_ resource.ResourceWithConfigure        = (*clusterResource)(nil)
	_ resource.ResourceWithImportState      = (*clusterResource)(nil)
	_ resource.ResourceWithValidateConfig   = (*clusterResource)(nil)
)

// Default operation timeouts (design §7.3). The timeouts block lets the user
// override these per operation. We model the block by hand (a plain
// SingleNestedAttribute of duration strings) because the optional
// terraform-plugin-framework-timeouts module is not a dependency of this
// project and go.mod is frozen.
const (
	defaultCreateTimeout = 30 * time.Minute
	defaultUpdateTimeout = 20 * time.Minute
	defaultDeleteTimeout = 20 * time.Minute
)

// postMutationWarmup is the wait inserted between a mutating operation
// (rescale/upgrade/backup/admin_password) and the first status poll. ACM
// reflects the operation in its status endpoints with a ~3-5s lag (observed
// live: cluster.status flips ready→pending several seconds after the POST
// returns 200). Without this wait, polling immediately returns the stale
// pre-op state and apply exits while ACM is still scheduling the work.
//
// `var` not `const` so tests can shrink it to ~0 (see resource_clickhouse_cluster_test.go).
var postMutationWarmup = 10 * time.Second

// postCreateSettleDelay is the wait after a cluster Create finishes polling
// (PollUntilHealthy + PollUntilIdle both return) but before exiting the
// Create RPC. Even though both polls report a cluster-level "done" signal,
// ACM's operator continues to push per-resource state (profiles, settings,
// initial schemas) to ClickHouse asynchronously for several more seconds.
// An immediately-following Create of a downstream resource (analytics_ro
// user, etc.) can race this push: ACM's synchronous SQL generator looks up
// not-yet-propagated state and emits malformed SQL (observed live:
// `GRANT SELECT ON profile.` with empty trailing dot), which ClickHouse
// rejects with Code 62 SYNTAX_ERROR.
//
// Belt-and-suspenders: downstream Creates also use acm.RetryOnTransientCreateRace
// to absorb the same race in long-tail cases where this settle isn't enough.
//
// `var` not `const` so tests can shrink it to ~0. Operators can override at
// runtime via the ALTINITYCLOUD_CLUSTER_SETTLE_DELAY env var (any
// time.ParseDuration string, e.g. "10s", "1m"). Use case: CI pipelines that
// run many parallel applies and prefer to let RetryOnTransientCreateRace
// absorb the race rather than pay the unconditional 30s tax per cluster.
var postCreateSettleDelay = 30 * time.Second

// postCreateSettleDelayEnvVar opts the operator into a different settle
// delay without a schema change. Invalid values fall back to the default
// with a Warn log so the operator notices.
const postCreateSettleDelayEnvVar = "ALTINITYCLOUD_CLUSTER_SETTLE_DELAY"

// effectivePostCreateSettleDelay reads the override env var, falling back
// to the package default. Resolved at each Create so the same provider
// binary can be reconfigured per-shell without restart.
func effectivePostCreateSettleDelay(ctx context.Context) time.Duration {
	v := os.Getenv(postCreateSettleDelayEnvVar)
	if v == "" {
		return postCreateSettleDelay
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		tflog.Warn(ctx, "ignoring invalid "+postCreateSettleDelayEnvVar+"; falling back to default",
			map[string]any{"value": v, "error": err.Error(), "default": postCreateSettleDelay.String()})
		return postCreateSettleDelay
	}
	tflog.Debug(ctx, "post-create settle delay overridden via env var",
		map[string]any{"value": d.String()})
	return d
}

// clusterResource is the altinity_clickhouse_cluster resource. It owns compute,
// storage, version, ZK/Keeper, AZ, networking, backup schedule, Datadog and
// uptime — but NOT settings/profiles/users (those are satellite resources, see
// design §5).
type clusterResource struct {
	client *acm.Client
}

// NewClusterResource is the constructor registered with the provider for
// altinity_clickhouse_cluster.
func NewClusterResource() resource.Resource {
	return &clusterResource{}
}

// timeoutsModel maps the nested timeouts block.
type timeoutsModel struct {
	Create types.String `tfsdk:"create"`
	Update types.String `tfsdk:"update"`
	Delete types.String `tfsdk:"delete"`
}

// clusterResourceModel maps the altinity_clickhouse_cluster schema. Topology
// and identity fields that ACM cannot mutate in place are protected by
// RequiresReplace plan modifiers (design §7.2 conservative default).
type clusterResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Environment types.String `tfsdk:"environment"`
	Name        types.String `tfsdk:"name"`
	Type        types.String `tfsdk:"type"`
	Role        types.String `tfsdk:"role"`

	// Compute.
	NodeCount types.Int64  `tfsdk:"node_count"`
	Shards    types.Int64  `tfsdk:"shards"`
	Replicas  types.Int64  `tfsdk:"replicas"`
	NodeType  types.String `tfsdk:"node_type"`
	Memory    types.String `tfsdk:"memory"`

	// Storage.
	Size         types.String `tfsdk:"size"`
	StorageClass types.String `tfsdk:"storage_class"`
	VolumeType   types.String `tfsdk:"volume_type"`
	IOPS         types.Int64  `tfsdk:"iops"`
	Throughput   types.Int64  `tfsdk:"throughput"`
	Disks        types.Int64  `tfsdk:"disks"`
	DataPath     types.String `tfsdk:"data_path"`

	// Version.
	Version      types.String `tfsdk:"version"`
	VersionImage types.String `tfsdk:"version_image"`

	// ZooKeeper / Keeper (topology — immutable).
	Zookeeper     types.String `tfsdk:"zookeeper"`
	KeeperName    types.String `tfsdk:"keeper_name"`
	ZoneAwareness types.Bool   `tfsdk:"zone_awareness"`
	// Azlist is a framework List (not a Go slice) so the framework can
	// represent an "unknown" value sourced from a data source (e.g.
	// `azlist = data.altinity_zones.this.zones`). Decoding unknown into a
	// plain []string fails with "Received unknown value, however the target
	// type cannot handle unknown values."
	Azlist types.List `tfsdk:"azlist"`

	// Networking.
	Secure         types.Bool   `tfsdk:"secure"`
	PublicEndpoint types.Bool   `tfsdk:"public_endpoint"`
	Host           types.String `tfsdk:"host"`
	Port           types.Int64  `tfsdk:"port"`
	HTTPPort       types.Int64  `tfsdk:"http_port"`
	SSHPort        types.Int64  `tfsdk:"ssh_port"`
	LBType         types.String `tfsdk:"lb_type"`
	IPWhitelist    types.String `tfsdk:"ip_whitelist"`

	MysqlProtocol   types.Bool  `tfsdk:"mysql_protocol"`
	MysqlPort       types.Int64 `tfsdk:"mysql_port"`
	ReplicateSchema types.Bool  `tfsdk:"replicate_schema"`

	Timezone types.String `tfsdk:"timezone"`
	Uptime   types.String `tfsdk:"uptime"`

	// Admin credentials.
	AdminUser types.String `tfsdk:"admin_user"`
	// Secrets (Sensitive plain attrs; preserved from prior state on Read).
	AdminPass types.String `tfsdk:"admin_password"`

	// Computed read-back attributes.
	Status        types.String `tfsdk:"status"`
	State         types.String `tfsdk:"state"`
	SystemVersion types.String `tfsdk:"system_version"`
	Endpoint      types.String `tfsdk:"endpoint"`
	EndpointHTTP  types.String `tfsdk:"endpoint_http"`

	// Opaque nested blocks — TODO(spike): model as strongly-typed nested blocks
	// once the spike captures their concrete shapes (design §6). Carried here as
	// raw JSON-string passthroughs so the schema, mapping and tests compile and
	// exercise control flow. Do NOT invent the sub-field shapes.
	Datadog            types.String `tfsdk:"datadog"`             // TODO(spike): datadogSettings
	BackupOptions      types.String `tfsdk:"backup_options"`      // TODO(spike): backupOptions / backup schedule
	UptimeSettings     types.String `tfsdk:"uptime_settings"`     // TODO(spike): uptimeSettings
	AlternateEndpoints types.String `tfsdk:"alternate_endpoints"` // TODO(spike): alternateEndpoints

	// AdoptExisting opts into idempotent-create take-over of a same-named cluster
	// already present in the environment. Default false — Create errors loudly
	// rather than silently adopting (which could put another team's cluster
	// under management).
	AdoptExisting types.Bool `tfsdk:"adopt_existing"`

	Timeouts types.Object `tfsdk:"timeouts"`
}

func (r *clusterResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_clickhouse_cluster"
}

func (r *clusterResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage an Altinity.Cloud ClickHouse cluster (launch / rescale / upgrade / backup / terminate). " +
			"Does NOT manage settings, profiles or users — use the satellite resources for those (design §5).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The ACM cluster id (integer, stored as string).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},

			// ---- identity / topology (RequiresReplace: conservative default) ----
			"environment": schema.StringAttribute{
				Required:    true,
				Description: "The ACM environment id to launch the cluster in. Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The cluster name. Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"type": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "The cluster type. Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"role": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("prod"),
				Description: "Cluster role — REQUIRED by ACM at launch (omitting it returns HTTP 500). " +
					"Use the API codes \"prod\" or \"dev\" (shown as Production / Development in the ACM UI; " +
					"the UI labels themselves are rejected). Defaults to \"prod\". Changing it forces replacement.",
				Validators: []validator.String{clusterRoleValidator{}},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},

			// ---- compute (node_count/shards/replicas mutable via rescale) ----
			"node_count": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Description: "Number of nodes (maps to ACM `nodes`). Mutable in place via rescale.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"shards": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Description: "Number of shards. Mutable in place via rescale — adding shards " +
					"is a partition expansion; reducing shards is a data-loss operation that " +
					"ACM may refuse server-side. Plan accordingly.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"replicas": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Description: "Number of replicas per shard. Mutable in place via rescale " +
					"(scale-out adds redundancy; scale-in removes replicas).",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"node_type": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "The node type code (e.g. n2d-standard-2). Mutable in place via rescale.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"memory": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Memory request. Mutable in place via rescale.",
				PlanModifiers: []planmodifier.String{
					useStateOrNullString{},
				},
			},

			// ---- storage (mutable via rescale) ----
			"size": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Storage size per node. Mutable in place via rescale.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"storage_class": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Storage class. Mutable in place via rescale.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"volume_type": schema.StringAttribute{
				// Optional-only (NOT Computed): the ACM API does not read this
				// field back, so making it Computed nulls it in state and then
				// re-plans it as unknown, which — combined with RequiresReplace —
				// forces a spurious cluster replacement on any later change. Leaving
				// it unset keeps a stable null; setting it still forces replace.
				Optional:    true,
				Description: "Volume type. Changing this forces replacement (TODO(spike): confirm in-place path).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"iops": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Description: "Provisioned IOPS. Mutable in place via rescale.",
				PlanModifiers: []planmodifier.Int64{
					useStateOrNullInt64{},
				},
			},
			"throughput": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Description: "Provisioned throughput. Mutable in place via rescale.",
				PlanModifiers: []planmodifier.Int64{
					useStateOrNullInt64{},
				},
			},
			"disks": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(1),
				Description: "Number of disks (default 1). Mutable in place via rescale.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"data_path": schema.StringAttribute{
				// Optional-only (NOT Computed): not read back by ACM — see volume_type.
				Optional:    true,
				Description: "Data path (storage policy). Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},

			// ---- version (mutable via upgrade — forward-only) ----
			"version": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "ClickHouse version. Mutable in place via upgrade — forward-only; a downgrade is rejected at plan time (ClickHouse does not support in-place downgrades).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					versionDowngradeGuard{},
				},
			},
			"version_image": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Explicit version image. Mutable in place via upgrade.",
				PlanModifiers: []planmodifier.String{
					useStateOrNullString{},
				},
			},

			// ---- ZK / Keeper / AZ (topology — RequiresReplace) ----
			"zookeeper": schema.StringAttribute{
				// Optional-only (NOT Computed): not read back by ACM — see volume_type.
				Optional:    true,
				Description: "ZooKeeper mode/selection (e.g. \"launch\"). Mutually exclusive with keeper_name. Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"keeper_name": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Keeper name. Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"zone_awareness": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Spread nodes across availability zones. Changing this forces replacement.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"azlist": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Availability zones to place nodes in (e.g. from the altinity_zones data source). " +
					"Mutable in place via rescale (changing the AZ set rebalances nodes; it does NOT destroy the cluster).",
			},

			// ---- networking (defaults match the ACM UI launch payload) ----
			"secure": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Enable TLS (default true). Changing this forces replacement (TODO(spike): confirm in-place path).",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"public_endpoint": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Expose a public endpoint (default true). Changing this forces replacement.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"host": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("localhost"),
				Description: "Cluster host (ACM-UI default \"localhost\"). ACM uses this as the internal bind host; " +
					"operators typically leave it at the default. Changing this on an existing cluster forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"port": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(9900),
				Description: "Native protocol port (ACM-UI default 9900 — NOT the upstream ClickHouse default of 9000). " +
					"Override only if you have a specific reason to deviate from ACM's launch defaults; " +
					"this is wired through to ACM at launch and is not echoed back, so the configured value is authoritative. " +
					"Changing this on an existing cluster forces replacement.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"http_port": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(5123),
				Description: "HTTP protocol port (ACM-UI default 5123 — NOT the upstream ClickHouse default of 8123). " +
					"Override only if you have a specific reason to deviate from ACM's launch defaults. " +
					"Changing this on an existing cluster forces replacement.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"ssh_port": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(2222),
				Description: "SSH port for ACM operator access (ACM-UI default 2222). " +
					"Changing this on an existing cluster forces replacement.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"lb_type": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString("ingress"),
				Description: "Load balancer type (default \"ingress\"). Changing this forces replacement (TODO(spike): confirm in-place path).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"ip_whitelist": schema.StringAttribute{
				// Optional-only (NOT Computed): not read back by ACM — see volume_type.
				Optional:    true,
				Description: "IP allow-list (comma-separated CIDRs). TODO(spike): confirm the in-place update endpoint; conservatively RequiresReplace until then.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"mysql_protocol": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Enable the MySQL protocol (default false). Changing this forces replacement.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"mysql_port": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(9004),
				Description: "MySQL protocol port (ACM-UI default 9004 — matches upstream ClickHouse). " +
					"Only relevant when `mysql_protocol = true`. Changing this on an existing cluster forces replacement.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"replicate_schema": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Replicate schema across replicas (default true). Changing this forces replacement.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"timezone": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Cluster timezone. Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"uptime": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Uptime schedule selector. TODO(spike): confirm the in-place update endpoint; conservatively RequiresReplace until then (design §7.2).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},

			// ---- admin credentials ----
			"admin_user": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString("admin"),
				Description: "Cluster admin username. Required alongside admin_password (launching with a password but no user errors). Defaults to \"admin\".",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},

			// ---- secrets ----
			"admin_password": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Admin user password. Write-only at the API (never returned on Read); preserved from prior state. Changing this updates the admin user's password in place via the DB user API.",
			},

			// ---- computed read-back ----
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "Current cluster status (poll value).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"state": schema.StringAttribute{
				Computed:    true,
				Description: "Current cluster state.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"system_version": schema.StringAttribute{
				Computed:    true,
				Description: "The running ClickHouse system version.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"endpoint": schema.StringAttribute{
				Computed:    true,
				Description: "The native protocol endpoint.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"endpoint_http": schema.StringAttribute{
				Computed:    true,
				Description: "The HTTP protocol endpoint.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},

			// ---- opaque nested blocks (TODO(spike) raw passthrough) ----
			"datadog": schema.StringAttribute{
				Optional: true,
				Description: "TODO(spike): Datadog integration settings (datadogSettings). Currently a raw JSON-string passthrough; " +
					"replace with a strongly-typed nested block once the spike captures the shape (design §6). " +
					"Contains the Datadog API key when set, hence Sensitive. " +
					"Conservatively RequiresReplace until the in-place update endpoint is confirmed (design §7.2).",
				Sensitive:  true,
				Validators: []validator.String{jsonStringValidator{}},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"backup_options": schema.StringAttribute{
				Optional: true,
				Description: "TODO(spike): backup schedule configuration (backupOptions). Currently a raw JSON-string passthrough; " +
					"replace with a strongly-typed nested block once the spike captures the shape (design §6). " +
					"Marked Sensitive because the opaque blob may carry credentials (e.g. object-storage keys).",
				Sensitive:  true,
				Validators: []validator.String{jsonStringValidator{}},
			},
			"uptime_settings": schema.StringAttribute{
				Optional: true,
				Description: "TODO(spike): uptime window settings (uptimeSettings). Raw JSON passthrough — " +
					"replace with a strongly-typed nested block once the spike captures the shape (design §6). " +
					"WARNING: any change to this attribute forces cluster replacement (RequiresReplace). " +
					"Remove RequiresReplace only after confirming the ACM in-place update endpoint (design §7.2). " +
					"Marked Sensitive because the opaque blob's shape is unspecified and may carry secrets.",
				Sensitive:  true,
				Validators: []validator.String{jsonStringValidator{}},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"alternate_endpoints": schema.StringAttribute{
				Optional: true,
				Description: "TODO(spike): alternate endpoints (alternateEndpoints). Currently a raw JSON-string passthrough; " +
					"replace with a strongly-typed nested block once the spike captures the shape (design §6). " +
					"Conservatively RequiresReplace until the in-place update endpoint is confirmed (design §7.2). " +
					"Marked Sensitive because the opaque blob's shape is unspecified and may carry credentials.",
				Sensitive:  true,
				Validators: []validator.String{jsonStringValidator{}},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},

			// ---- adoption opt-in (default false) ----
			"adopt_existing": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "If true, Create will adopt an existing cluster of the same name in " +
					"the target environment instead of erroring. Default false — a cluster created " +
					"out-of-band (or by another team using the same ACM token) will NOT be silently " +
					"placed under Terraform management. Set to true only when you intend to take over " +
					"a pre-existing cluster, e.g. when migrating an ACM-UI-created cluster into IaC. " +
					"Adoption still validates that immutable topology fields match the plan.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},

			// ---- timeouts (hand-modeled; see comment on the constants) ----
			"timeouts": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Operation timeouts as Go duration strings (e.g. \"30m\"). Defaults: create 30m, update 20m, delete 20m.",
				Attributes: map[string]schema.Attribute{
					"create": schema.StringAttribute{
						Optional:    true,
						Description: "Create timeout (Go duration string). Default 30m.",
					},
					"update": schema.StringAttribute{
						Optional:    true,
						Description: "Update timeout (Go duration string). Default 20m.",
					},
					"delete": schema.StringAttribute{
						Optional:    true,
						Description: "Delete timeout (Go duration string). Default 20m.",
					},
				},
			},
		},
	}
}

func (r *clusterResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return // provider Configure not yet called
	}
	client, ok := req.ProviderData.(*acm.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *acm.Client, got %T. This is a provider bug.", req.ProviderData),
		)
		return
	}
	r.client = client
}

// ValidateConfig enforces cross-attribute invariants that the per-attribute
// schema cannot. See validateClusterModel for the rules.
func (r *clusterResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg clusterResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(validateClusterModel(cfg)...)
}

// validateClusterModel runs the cross-attribute checks against a resolved
// clusterResourceModel. Factored out from ValidateConfig so the rules are
// unit-testable without needing a framework Config (which is read-only).
//
// Rule 1: node_count must equal shards × replicas when both are set. A stale
// node_count alongside an updated replicas previously caused the plan to
// show `node_count: 2 → 1` next to `replicas: 2 → 3`, and ACM mis-applied
// the SQL (observed live).
//
// Rule 2: `zookeeper` and `keeper_name` are mutually exclusive. ACM treats
// them as a one-of selector for the coordination cluster; setting both leads
// to a mid-apply HTTP error AFTER the cluster may already be partially
// provisioned. Fail at plan time with a clear diagnostic instead.
func validateClusterModel(cfg clusterResourceModel) diag.Diagnostics {
	var diags diag.Diagnostics

	// Rule 1: shards × replicas == node_count.
	if !cfg.NodeCount.IsNull() && !cfg.NodeCount.IsUnknown() &&
		!cfg.Shards.IsNull() && !cfg.Shards.IsUnknown() &&
		!cfg.Replicas.IsNull() && !cfg.Replicas.IsUnknown() {
		nc := cfg.NodeCount.ValueInt64()
		shards := cfg.Shards.ValueInt64()
		replicas := cfg.Replicas.ValueInt64()
		expected := shards * replicas
		if nc != expected {
			diags.AddAttributeError(
				path.Root("node_count"),
				"node_count inconsistent with topology",
				fmt.Sprintf("node_count (%d) must equal shards × replicas (%d × %d = %d). "+
					"Remove node_count from your config to let it be computed by the provider, "+
					"or align it to %d. The provider no longer sends a separate `nodes` field on "+
					"rescale (shards × replicas is authoritative), so a stale node_count has no "+
					"effect on what ACM does — it only confuses the plan output.",
					nc, shards, replicas, expected, expected),
			)
		}
	}

	// Rule 2: zookeeper and keeper_name are mutually exclusive.
	zkSet := !cfg.Zookeeper.IsNull() && !cfg.Zookeeper.IsUnknown() && cfg.Zookeeper.ValueString() != ""
	keeperSet := !cfg.KeeperName.IsNull() && !cfg.KeeperName.IsUnknown() && cfg.KeeperName.ValueString() != ""
	if zkSet && keeperSet {
		diags.AddAttributeError(
			path.Root("zookeeper"),
			"Conflicting coordination cluster configuration",
			"Set either `zookeeper` or `keeper_name`, not both. ACM treats them as a "+
				"one-of selector for the cluster's coordination backend. Setting both "+
				"produces an opaque mid-apply ACM error after the cluster may already "+
				"be partially provisioned. Choose `keeper_name = \"<existing-keeper>\"` "+
				"to attach a managed CH Keeper resource, or `zookeeper = \"launch\"` to "+
				"let ACM auto-create a ZK ensemble.",
		)
	}

	return diags
}

// ImportState takes the ACM cluster id directly (design §5.1):
// terraform import altinity_clickhouse_cluster.x 12345
func (r *clusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id := strings.TrimSpace(req.ID)
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("altinity_clickhouse_cluster import expects the ACM cluster id (an integer), got %q: %s", req.ID, err),
		)
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), resource.ImportStateRequest{ID: id}, resp)
}

// ---- Create ----

func (r *clusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan clusterResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	to, diags := resolveTimeouts(ctx, plan.Timeouts)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Bound the whole create (launch retries + poll) by the create timeout.
	opCtx, cancel := context.WithTimeout(ctx, to.create)
	defer cancel()

	// ACM serializes env operations: a launch can fail with "Another operation
	// is in progress" while a just-created keeper/cluster is still settling.
	// Retry on that transient lock until the env frees up (bounded by opCtx).
	launchReq := launchRequestFromPlan(plan)
	tflog.Info(ctx, "launching ClickHouse cluster", map[string]any{
		"environment": plan.Environment.ValueString(),
		"name":        plan.Name.ValueString(),
		"version":     plan.Version.ValueString(),
		"node_type":   plan.NodeType.ValueString(),
		"keeper_name": plan.KeeperName.ValueString(),
	})
	env := plan.Environment.ValueString()
	name := plan.Name.ValueString()
	adoptExisting := plan.AdoptExisting.ValueBool()
	var cluster acm.Cluster
	var adopted bool
	err := acm.RetryWhileBusy(opCtx, func() error {
		// Idempotent-with-opt-in: if a cluster of this name already exists, only
		// adopt it when adopt_existing=true. Otherwise refuse — silently adopting
		// would place a cluster created out-of-band (or by another team using the
		// same token) under Terraform management, with eventual destroy authority.
		// Adoption also stops the busy-retry from re-POSTing a launch every interval
		// when a prior attempt's launch succeeded server-side without returning.
		if existing, found, ferr := r.client.FindClusterByName(opCtx, env, name); ferr != nil {
			return ferr
		} else if found {
			if !adoptExisting {
				return fmt.Errorf("a cluster named %q already exists in environment %s (id=%d); "+
					"Terraform refuses to adopt it by default. Set adopt_existing = true to take it over, "+
					"or destroy the existing cluster first if it is unmanaged", name, env, existing.ID)
			}
			cluster = existing
			adopted = true
			return nil
		}
		c, lerr := r.client.LaunchCluster(opCtx, env, launchReq)
		if lerr != nil {
			return lerr
		}
		cluster = c
		return nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to launch cluster", err.Error())
		return
	}
	if cluster.ID == 0 {
		resp.Diagnostics.AddError(
			"Cluster launch returned invalid ID",
			"ACM returned a cluster with id=0, which indicates a malformed or unexpected response. "+
				"The cluster may or may not have been created; check the ACM UI before re-applying.",
		)
		return
	}
	if adopted {
		if err := validateAdoptedCluster(plan, cluster); err != nil {
			resp.Diagnostics.AddError("Adopted cluster has mismatched immutable fields", err.Error())
			return
		}
	}
	tflog.Info(ctx, "cluster launched; polling until healthy", map[string]any{"cluster_id": cluster.ID})

	// §7.1: persist the returned id to state BEFORE polling, so a timed-out or
	// cancelled create is still tracked and recoverable on the next apply.
	// resolveUnknownComputed first, so this partial write contains no unknowns
	// (Terraform rejects unknown values if Create returns here on a poll error).
	plan.ID = types.StringValue(strconv.FormatInt(cluster.ID, 10))
	resolveUnknownComputed(&plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := acm.PollUntilHealthy(opCtx, func(c context.Context) (string, error) {
		return r.client.GetClusterStatus(c, cluster.ID)
	}); err != nil {
		resp.Diagnostics.AddError(
			"Cluster did not become healthy",
			fmt.Sprintf("The cluster id %d was launched and is tracked in state, but polling failed: %s. "+
				"Re-apply to resume polling, or destroy to terminate it.", cluster.ID, err),
		)
		return
	}

	// Operation-level idle check (belt-and-suspenders): wait for the
	// /cluster/{id}/status endpoint to report action="Completed". A cluster
	// can flip to status="online" while ACM is still finishing provisioning
	// (e.g. spinning up additional nodes), so the action endpoint is the
	// reliable signal that launch-side work is done.
	if err := acm.PollUntilIdle(opCtx, func(c context.Context) (acm.ClusterAction, error) {
		return r.client.GetClusterAction(c, cluster.ID)
	}); err != nil {
		resp.Diagnostics.AddError(
			"Cluster launch did not complete",
			fmt.Sprintf("The cluster id %d was launched and reached a healthy lifecycle status, "+
				"but the per-cluster operation-status poll failed: %s. Re-apply to resume polling.",
				cluster.ID, err),
		)
		return
	}

	// Post-launch settling delay. Both polls above report a cluster-level
	// "done" signal, but ACM's per-resource state (profiles, settings,
	// schemas) is propagated to ClickHouse asynchronously by the operator
	// AFTER the cluster reports ready/Completed. Without this wait, an
	// immediate downstream apply (e.g. altinity_clickhouse_user) can race
	// the operator: ACM's SQL generator looks up not-yet-pushed state,
	// emits malformed SQL like `GRANT SELECT ON profile.` (empty after the
	// dot), and ClickHouse rejects it with "Code: 62 SYNTAX_ERROR ...
	// Expected end of query". The wait is short — a steady-state settle,
	// not a poll — because downstream Creates also have transient-SQL
	// retry (see acm.RetryOnTransientCreateRace). Tests override to ~0.
	select {
	case <-opCtx.Done():
		return
	case <-time.After(effectivePostCreateSettleDelay(ctx)):
	}

	// Full Read after the cluster is healthy.
	final, err := r.client.GetCluster(ctx, cluster.ID)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read cluster after launch", err.Error())
		return
	}
	applyClusterToModel(&plan, final)
	resolveUnknownComputed(&plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// ---- Read ----

func (r *clusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state clusterResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Invalid cluster id in state", err.Error())
		return
	}

	// Existence check by listing the environment, NOT GET /cluster/{id}: ACM
	// returns 403 (not 404) for a non-existent/inaccessible id (e.g. the id 0
	// left by a failed launch), so a per-id GET cannot distinguish "gone" from
	// "forbidden". Listing the environment treats absence as drift cleanly.
	// Fallback: right after import only the id is known (environment is empty),
	// so resolve via GET by id; that read populates environment for later plans.
	var cluster acm.Cluster
	if env := state.Environment.ValueString(); env != "" {
		var found bool
		cluster, found, err = r.client.FindClusterInEnv(ctx, env, id)
		if err != nil {
			resp.Diagnostics.AddError("Failed to read cluster", err.Error())
			return
		}
		if !found {
			// Drift: cluster not present in its environment (deleted out-of-band
			// or a failed launch left a non-existent id) — remove from state so
			// the next apply re-creates it (§7.1).
			resp.State.RemoveResource(ctx)
			return
		}
	} else {
		cluster, err = r.client.GetCluster(ctx, id)
		if err != nil {
			// Only treat 404 as drift here. A 403 on the per-id GET is more
			// likely a real token problem (insufficient scope, expired) than a
			// gone resource — silently removing from state would destroy work
			// on the next apply. Surface the error so the operator can fix the
			// token instead.
			if acm.IsNotFound(err) {
				resp.State.RemoveResource(ctx)
				return
			}
			resp.Diagnostics.AddError("Failed to read cluster", err.Error())
			return
		}
	}

	// Secrets (admin_password, datadog key) are NOT returned by the API; they
	// are preserved from prior state by applyClusterToModel (it never overwrites
	// the secret attrs), excluding them from drift (§7.1/§9).
	applyClusterToModel(&state, cluster)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// ---- Update (dispatcher, design §7.2) ----

// Update routes the plan/prior-state diff to the correct ACM endpoints in a
// fixed sequential order: upgrade (1) -> rescale (2) -> backup (3). Each
// poll-required step waits for terminal-healthy before the next. After every
// successful sub-mutation we re-Read and write the converged-so-far state, so a
// later failure still leaves state reflecting every step that succeeded
// (best-effort, re-apply-to-converge).
func (r *clusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state clusterResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	to, diags := resolveTimeouts(ctx, plan.Timeouts)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Invalid cluster id in state", err.Error())
		return
	}
	plan.ID = state.ID

	// Compute every sub-domain diff UP FRONT, from the original plan vs prior
	// state, so the routing decision for a later step is not clobbered by the
	// converged read of an earlier one.
	upgradeReq, doUpgrade := upgradeDiff(plan, state)
	rescaleReq, doRescale := rescaleDiff(plan, state)
	backupReq, doBackup := backupDiff(plan, state)

	// reReadAndStore re-Reads the cluster and writes converged-so-far state. It
	// refreshes only the COMPUTED read-back attrs (status/state/endpoints/
	// system_version/id), keeping the user's desired plan config intact for the
	// still-pending steps. Secrets and config-only opaque blocks are likewise
	// preserved from the plan (§7.1/§9).
	reReadAndStore := func() error {
		cluster, err := r.client.GetCluster(ctx, id)
		if err != nil {
			return err
		}
		refreshReadback(&plan, cluster)
		// Null any Computed attr the API does not read back and the user left
		// unset; otherwise it stays unknown in the plan (UseStateForUnknown
		// no-ops on a null prior-state value) and the apply errors with
		// "provider returned invalid result object ... unknown value".
		resolveUnknownComputed(&plan)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return nil
	}

	pollHealthy := func() error {
		pollCtx, cancel := context.WithTimeout(ctx, to.update)
		defer cancel()
		return acm.PollUntilHealthy(pollCtx, func(c context.Context) (string, error) {
			return r.client.GetClusterStatus(c, id)
		})
	}

	// pollIdle waits for the cluster's operation-level status to return to
	// "Completed" (the /cluster/{id}/status endpoint). Run FIRST after each
	// mutating step, then pollHealthy as a sanity check.
	//
	// IMPORTANT: ACM reflects an issued operation in its status endpoints with
	// a ~3-5 second LAG (observed live: cluster.status flips from "ready" →
	// "pending" several seconds after the rescale POST returns 200). Without
	// a warmup, the immediate check inside PollUntilIdle/PollUntilHealthy
	// sees the stale pre-op state (action="Completed", status="ready") and
	// returns success — apply exits while ACM is still scheduling the work.
	// Wait before the first poll to give ACM time to start tracking.
	pollIdle := func() error {
		pollCtx, cancel := context.WithTimeout(ctx, to.update)
		defer cancel()
		select {
		case <-pollCtx.Done():
			return pollCtx.Err()
		case <-time.After(postMutationWarmup):
		}
		return acm.PollUntilIdle(pollCtx, func(c context.Context) (acm.ClusterAction, error) {
			return r.client.GetClusterAction(c, id)
		})
	}

	// (1) version upgrade.
	if doUpgrade {
		if err := r.client.UpgradeCluster(ctx, id, upgradeReq); err != nil {
			resp.Diagnostics.AddError("Failed to upgrade cluster version", err.Error())
			return
		}
		if err := pollIdle(); err != nil {
			resp.Diagnostics.AddError("Cluster upgrade did not complete", err.Error())
			return
		}
		if err := pollHealthy(); err != nil {
			resp.Diagnostics.AddError("Cluster did not become healthy after upgrade", err.Error())
			return
		}
		if err := reReadAndStore(); err != nil {
			resp.Diagnostics.AddError("Failed to re-read cluster after upgrade", err.Error())
			return
		}
	}

	// (2) compute + storage rescale.
	if doRescale {
		if err := r.client.RescaleCluster(ctx, id, rescaleReq); err != nil {
			resp.Diagnostics.AddError("Failed to rescale cluster", err.Error())
			return
		}
		if err := pollIdle(); err != nil {
			resp.Diagnostics.AddError("Cluster rescale did not complete", err.Error())
			return
		}
		if err := pollHealthy(); err != nil {
			resp.Diagnostics.AddError("Cluster did not become healthy after rescale", err.Error())
			return
		}
		if err := reReadAndStore(); err != nil {
			resp.Diagnostics.AddError("Failed to re-read cluster after rescale", err.Error())
			return
		}
	}

	// (3) backup schedule (config-only, no poll).
	if doBackup {
		if err := r.client.ConfigureBackup(ctx, id, backupReq); err != nil {
			resp.Diagnostics.AddError("Failed to configure cluster backup", err.Error())
			return
		}
		if err := reReadAndStore(); err != nil {
			resp.Diagnostics.AddError("Failed to re-read cluster after backup config", err.Error())
			return
		}
	}

	// (4) admin password change — resolve the admin user id via list, then
	// update in place. No poll needed (the DB user update is synchronous).
	//
	// Guards: both plan and prior state must carry a known, non-null value. A
	// null plan means the user removed the attribute from config (write-only
	// semantics — we don't clear at the API). A null state means this is the
	// first apply after import / no prior password was managed: there's no
	// "change" to react to, the operator must run a separate apply once they
	// know what password they want. This avoids the import-then-update-fails
	// scenario where state.AdminUser/AdminPass are both empty.
	//
	// admin_user is RequiresReplace + has a default of "admin", so plan.AdminUser
	// is always populated under Update. Use that (not state) to resolve the
	// target login — imports may not have admin_user in state.
	planPassKnown := !plan.AdminPass.IsNull() && !plan.AdminPass.IsUnknown()
	statePassKnown := !state.AdminPass.IsNull() && !state.AdminPass.IsUnknown()
	if planPassKnown && statePassKnown &&
		plan.AdminPass.ValueString() != state.AdminPass.ValueString() {
		users, lerr := r.client.ListUsers(ctx, id)
		if lerr != nil {
			resp.Diagnostics.AddError("Failed to list users for admin password update", lerr.Error())
			return
		}
		adminLogin := plan.AdminUser.ValueString()
		if adminLogin == "" {
			// Defensive: schema default is "admin", but if somehow null reached
			// this branch (e.g. state-edit), fall back rather than match users
			// with an empty login.
			adminLogin = "admin"
		}
		var adminUser *acm.User
		for i := range users {
			if users[i].Login == adminLogin {
				adminUser = &users[i]
				break
			}
		}
		if adminUser == nil {
			resp.Diagnostics.AddError(
				"Admin user not found",
				fmt.Sprintf("Admin user %q not found in cluster user list; password not updated. "+
					"Ensure admin_user matches the actual admin login.", adminLogin),
			)
			return
		}
		// Admin password rotation goes through the GLOBAL /user/{id} endpoint
		// (UpdateUserGlobal → DbuserEdit), NOT the cluster-scoped DbuserEditSql.
		// DbuserEditSql runs the user-update SQL synchronously and is gated by
		// ACM's "Cluster check" pre-flight, which systematically rejects the
		// admin user even on a fully healthy cluster (live-confirmed). The ACM
		// UI itself uses /user/{id} to rotate the admin password; mirror that.
		//
		// Send the existing user's settings alongside the password so DbuserEdit
		// re-renders the same user shape (just with a new password) rather than
		// resetting networks/databases/profile to defaults.
		updateReq := acm.UserRequest{
			Login:     adminLogin,
			Password:  plan.AdminPass.ValueString(),
			Networks:  splitCSVString(adminUser.Networks),
			Databases: splitCSVString(adminUser.Databases),
			IDProfile: adminUser.IDProfile,
		}
		if adminUser.AccessManagement {
			one := 1
			updateReq.AccessManagement = &one
		}
		if _, err := r.client.UpdateUserGlobal(ctx, adminUser.ID, updateReq); err != nil {
			resp.Diagnostics.AddError("Failed to update admin password", err.Error())
			return
		}
	}

	// Final converged Read (covers the no-op case where nothing dispatched).
	cluster, err := r.client.GetCluster(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read cluster after update", err.Error())
		return
	}
	refreshReadback(&plan, cluster)
	resolveUnknownComputed(&plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// ---- Delete ----

func (r *clusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state clusterResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	to, diags := resolveTimeouts(ctx, state.Timeouts)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Invalid cluster id in state", err.Error())
		return
	}

	if err := r.client.TerminateCluster(ctx, id); err != nil {
		if acm.IsNotFound(err) {
			return // already gone
		}
		resp.Diagnostics.AddError("Failed to terminate cluster", err.Error())
		return
	}

	env := state.Environment.ValueString()
	if env == "" {
		// Environment unknown (import path left it empty). Cannot poll via list;
		// the TerminateCluster call above succeeded so deletion is in progress.
		tflog.Warn(ctx, "altinity_clickhouse_cluster: environment unknown; skipping termination poll", map[string]any{"cluster_id": id})
		return
	}
	pollCtx, cancel := context.WithTimeout(ctx, to.delete)
	defer cancel()
	if err := acm.PollUntilGoneBy(pollCtx, func(c context.Context) (bool, error) {
		_, found, err := r.client.FindClusterInEnv(c, env, id)
		return found, err
	}); err != nil {
		resp.Diagnostics.AddError("Cluster did not terminate", err.Error())
		return
	}
}

// =========================================================================
// tf <-> acm mapping (unit-testable, no framework/client dependencies)
// =========================================================================

// launchRequestFromPlan builds the LaunchRequest from the plan. String-int and
// 0|1-bool coercions follow the ACM convention (clusters.go). Unknown/null
// attributes are left empty so `omitempty` drops them.
// clusterRoleValidator restricts `role` to ACM's accepted codes. The ACM UI
// shows "Development"/"Production" but the API expects "dev"/"prod" (confirmed
// from a real UI launch payload: role="dev"); the literal labels are rejected
// with "Invalid Cluster Role", and omitting role returns HTTP 500.
type clusterRoleValidator struct{}

func (clusterRoleValidator) Description(context.Context) string {
	return `must be "dev" or "prod"`
}

func (v clusterRoleValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (clusterRoleValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	switch req.ConfigValue.ValueString() {
	case "dev", "prod":
	default:
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid cluster role",
			`role must be "dev" (Development) or "prod" (Production); got `+strconv.Quote(req.ConfigValue.ValueString()))
	}
}

// jsonStringValidator rejects non-empty string attribute values that are not
// valid JSON, giving a clear plan-time diagnostic instead of a confusing ACM
// HTTP 400 at apply time.
type jsonStringValidator struct{}

func (jsonStringValidator) Description(context.Context) string {
	return "value must be valid JSON when set"
}

func (v jsonStringValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (jsonStringValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	s := req.ConfigValue.ValueString()
	if s != "" && !json.Valid([]byte(s)) {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid JSON",
			fmt.Sprintf("%s must be valid JSON; got %s", req.Path, strconv.Quote(s)),
		)
	}
}

func launchRequestFromPlan(p clusterResourceModel) acm.LaunchRequest {
	req := acm.LaunchRequest{
		Name:         p.Name.ValueString(),
		Type:         p.Type.ValueString(),
		Role:         p.Role.ValueString(),
		NodeType:     p.NodeType.ValueString(),
		Memory:       p.Memory.ValueString(),
		Version:      p.Version.ValueString(),
		VersionImage: p.VersionImage.ValueString(),
		Size:         p.Size.ValueString(),
		StorageClass: p.StorageClass.ValueString(),
		DataPath:     p.DataPath.ValueString(),
		Zookeeper:    p.Zookeeper.ValueString(),
		KeeperName:   p.KeeperName.ValueString(),
		LBType:       p.LBType.ValueString(),
		Host:         p.Host.ValueString(),
		AdminUser:    p.AdminUser.ValueString(),
		AdminPass:    p.AdminPass.ValueString(),
		IPWhitelist:  p.IPWhitelist.ValueString(),
		Timezone:     p.Timezone.ValueString(),
		Uptime:       p.Uptime.ValueString(),
		Azlist:       stringListToSlice(p.Azlist),
		// JSON booleans; sent unconditionally (the ACM backend wants real bools).
		Secure:          p.Secure.ValueBool(),
		MysqlProtocol:   p.MysqlProtocol.ValueBool(),
		PublicEndpoint:  p.PublicEndpoint.ValueBool(),
		ReplicateSchema: p.ReplicateSchema.ValueBool(),
		ZoneAwareness:   p.ZoneAwareness.ValueBool(),
	}
	if !p.NodeCount.IsNull() && !p.NodeCount.IsUnknown() {
		req.Nodes = strconv.FormatInt(p.NodeCount.ValueInt64(), 10)
	}
	if !p.Shards.IsNull() && !p.Shards.IsUnknown() {
		req.Shards = strconv.FormatInt(p.Shards.ValueInt64(), 10)
	}
	if !p.Replicas.IsNull() && !p.Replicas.IsUnknown() {
		req.Replicas = strconv.FormatInt(p.Replicas.ValueInt64(), 10)
	}
	if !p.Disks.IsNull() && !p.Disks.IsUnknown() {
		req.Disks = strconv.FormatInt(p.Disks.ValueInt64(), 10)
	}
	if !p.IOPS.IsNull() && !p.IOPS.IsUnknown() {
		req.IOPS = strconv.FormatInt(p.IOPS.ValueInt64(), 10)
	}
	if !p.Throughput.IsNull() && !p.Throughput.IsUnknown() {
		req.Throughput = strconv.FormatInt(p.Throughput.ValueInt64(), 10)
	}
	if !p.Port.IsNull() && !p.Port.IsUnknown() {
		req.Port = int(p.Port.ValueInt64())
	}
	if !p.HTTPPort.IsNull() && !p.HTTPPort.IsUnknown() {
		req.HTTPPort = int(p.HTTPPort.ValueInt64())
	}
	if !p.MysqlPort.IsNull() && !p.MysqlPort.IsUnknown() {
		req.MysqlPort = int(p.MysqlPort.ValueInt64())
	}
	if !p.SSHPort.IsNull() && !p.SSHPort.IsUnknown() {
		req.SSHPort = int(p.SSHPort.ValueInt64())
	}
	// Opaque passthroughs — TODO(spike): typed nested blocks.
	req.DatadogSettings = rawJSONOrNil(p.Datadog)
	req.BackupOptions = rawJSONOrNil(p.BackupOptions)
	req.UptimeSettings = rawJSONOrNil(p.UptimeSettings)
	req.AlternateEndpoints = rawJSONOrNil(p.AlternateEndpoints)
	return req
}

// validateAdoptedCluster checks that an existing cluster adopted by name
// (idempotent create) has the same RequiresReplace topology as the plan. A
// mismatch means the existing cluster was launched with different settings;
// proceeding would misrepresent them in state and silently break the next
// plan. The environment id is the highest-stakes check — an org-scoped token
// could find a same-named cluster in the wrong environment.
//
// Field-presence: ACM responses can be sparse mid-provisioning. The presence
// rule is per-field:
//   - environment id (int64): IDEnvironment == 0 means ACM didn't return it.
//   - type, role, keeper_name (string): empty string means ACM didn't return
//     it (these are non-empty when ACM populates them).
//   - shards, replicas (int64): 0 means ACM didn't return it. 0 is logically
//     impossible in ACM (a cluster always has at least one shard/replica), so
//     a real zero is indistinguishable from absence; we conservatively skip
//     rather than false-positive on partially-populated responses.
//
// The plan side is checked for null/unknown so an unset desired value never
// trips the comparison.
func validateAdoptedCluster(plan clusterResourceModel, c acm.Cluster) error {
	var mm []string
	// environment id — highest stakes: a same-named cluster in a different
	// environment than the operator targeted is almost certainly someone else's.
	if !plan.Environment.IsNull() && !plan.Environment.IsUnknown() && c.IDEnvironment != 0 {
		planEnv := plan.Environment.ValueString()
		apiEnv := strconv.FormatInt(c.IDEnvironment, 10)
		if planEnv != apiEnv {
			mm = append(mm, fmt.Sprintf("environment: plan=%q api=%q", planEnv, apiEnv))
		}
	}
	if !plan.Type.IsNull() && !plan.Type.IsUnknown() && c.Type != "" && plan.Type.ValueString() != c.Type {
		mm = append(mm, fmt.Sprintf("type: plan=%q api=%q", plan.Type.ValueString(), c.Type))
	}
	if !plan.Role.IsNull() && !plan.Role.IsUnknown() && c.Role != "" && plan.Role.ValueString() != c.Role {
		mm = append(mm, fmt.Sprintf("role: plan=%q api=%q", plan.Role.ValueString(), c.Role))
	}
	// shards/replicas: 0-shards is logically impossible in ACM, so a real zero
	// from the API can be treated as "not yet populated" without losing real
	// signal.
	if !plan.Shards.IsNull() && !plan.Shards.IsUnknown() && c.Shards != 0 && plan.Shards.ValueInt64() != c.Shards {
		mm = append(mm, fmt.Sprintf("shards: plan=%d api=%d", plan.Shards.ValueInt64(), c.Shards))
	}
	if !plan.Replicas.IsNull() && !plan.Replicas.IsUnknown() && c.Replicas != 0 && plan.Replicas.ValueInt64() != c.Replicas {
		mm = append(mm, fmt.Sprintf("replicas: plan=%d api=%d", plan.Replicas.ValueInt64(), c.Replicas))
	}
	if !plan.KeeperName.IsNull() && !plan.KeeperName.IsUnknown() && c.KeeperName != "" && plan.KeeperName.ValueString() != c.KeeperName {
		mm = append(mm, fmt.Sprintf("keeper_name: plan=%q api=%q", plan.KeeperName.ValueString(), c.KeeperName))
	}
	if len(mm) == 0 {
		return nil
	}
	return fmt.Errorf("adopted cluster differs from plan on RequiresReplace fields (%s); "+
		"destroy the existing cluster or align the config before applying", strings.Join(mm, "; "))
}

// upgradeDiff returns the UpgradeRequest and whether the version sub-domain
// changed between plan and prior state (design §7.2 rank 1).
func upgradeDiff(plan, state clusterResourceModel) (acm.UpgradeRequest, bool) {
	changed := stringChanged(plan.Version, state.Version) || stringChanged(plan.VersionImage, state.VersionImage)
	if !changed {
		return acm.UpgradeRequest{}, false
	}
	return acm.UpgradeRequest{
		Version:      plan.Version.ValueString(),
		VersionImage: plan.VersionImage.ValueString(),
	}, true
}

// rescaleDiff returns the RescaleRequest and whether the topology / compute /
// storage sub-domain changed (design §7.2 rank 2). The fields the ACM
// /cluster/{id}/rescale endpoint accepts per reference.json are: shards,
// replicas, nodeType, size, disks, throughput, azlist (plus the undocumented
// storageClass/iops we have historically sent). All of these go through this
// single dispatcher step — none of them should be RequiresReplace.
//
// node_count is INTENTIONALLY not a rescale trigger: total nodes is derived
// from shards × replicas, and a stale node_count in HCL alongside a real
// shards/replicas change confuses ACM's SQL generation. The cross-attribute
// validator (ValidateConfig) rejects configs where node_count is set but
// doesn't equal shards × replicas.
func rescaleDiff(plan, state clusterResourceModel) (acm.RescaleRequest, bool) {
	changed := int64Changed(plan.Shards, state.Shards) ||
		int64Changed(plan.Replicas, state.Replicas) ||
		stringChanged(plan.NodeType, state.NodeType) ||
		stringChanged(plan.Memory, state.Memory) ||
		stringChanged(plan.Size, state.Size) ||
		stringChanged(plan.StorageClass, state.StorageClass) ||
		int64Changed(plan.IOPS, state.IOPS) ||
		int64Changed(plan.Throughput, state.Throughput) ||
		int64Changed(plan.Disks, state.Disks) ||
		azlistChanged(plan.Azlist, state.Azlist)
	if !changed {
		return acm.RescaleRequest{}, false
	}
	req := acm.RescaleRequest{
		NodeType:     plan.NodeType.ValueString(),
		Size:         plan.Size.ValueString(),
		StorageClass: plan.StorageClass.ValueString(),
		Azlist:       stringListToSlice(plan.Azlist),
	}
	if !plan.Shards.IsNull() && !plan.Shards.IsUnknown() {
		req.Shards = strconv.FormatInt(plan.Shards.ValueInt64(), 10)
	}
	if !plan.Replicas.IsNull() && !plan.Replicas.IsUnknown() {
		req.Replicas = strconv.FormatInt(plan.Replicas.ValueInt64(), 10)
	}
	if !plan.Disks.IsNull() && !plan.Disks.IsUnknown() {
		req.Disks = strconv.FormatInt(plan.Disks.ValueInt64(), 10)
	}
	if !plan.IOPS.IsNull() && !plan.IOPS.IsUnknown() {
		req.IOPS = strconv.FormatInt(plan.IOPS.ValueInt64(), 10)
	}
	if !plan.Throughput.IsNull() && !plan.Throughput.IsUnknown() {
		req.Throughput = strconv.FormatInt(plan.Throughput.ValueInt64(), 10)
	}
	return req, true
}

// sliceToStringList lifts a []string up to a framework List<String>. An empty
// or nil slice maps to a non-null empty list (so post-Read state isn't unknown
// for an Optional+Computed list attr).
func sliceToStringList(s []string) types.List {
	if s == nil {
		s = []string{}
	}
	elems := make([]attr.Value, 0, len(s))
	for _, v := range s {
		elems = append(elems, types.StringValue(v))
	}
	l, _ := types.ListValue(types.StringType, elems)
	return l
}

// stringListToSlice projects a framework List<String> down to a plain
// []string. Null/unknown lists become nil so omitempty drops the field at
// the wire layer. Used for any ListAttribute whose model field is types.List
// (chosen over []string so an unknown value — typically sourced from a data
// source like data.altinity_zones.this.zones — round-trips through the
// framework without a "Received unknown value, however the target type
// cannot handle unknown values" decode error).
func stringListToSlice(l types.List) []string {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	elems := l.Elements()
	out := make([]string, 0, len(elems))
	for _, v := range elems {
		s, ok := v.(types.String)
		if !ok || s.IsNull() || s.IsUnknown() {
			continue
		}
		out = append(out, s.ValueString())
	}
	return out
}

// azlistChanged reports whether the configured AZ list differs from prior
// state in an order-independent way. ACM may sort/canonicalize the list, so
// element-order changes are not real drift. Unknown lists are treated as
// no-change to avoid spurious rescale triggers during multi-step applies.
func azlistChanged(a, b types.List) bool {
	if a.IsUnknown() || b.IsUnknown() {
		return false
	}
	return !stringSlicesEqualUnordered(stringListToSlice(a), stringListToSlice(b))
}

// backupDiff returns the BackupRequest and whether the backup schedule changed
// (design §7.2 rank 3, config-only). TODO(spike): typed backup block.
func backupDiff(plan, state clusterResourceModel) (acm.BackupRequest, bool) {
	if !stringChanged(plan.BackupOptions, state.BackupOptions) {
		return acm.BackupRequest{}, false
	}
	return acm.BackupRequest{Options: rawJSONOrNil(plan.BackupOptions)}, true
}

// applyClusterToModel writes the API-returned cluster onto the model, coercing
// int64/bool back to the framework types. Secret attributes (admin_password,
// datadog) and the timeouts block are NOT touched — they are preserved from the
// passed-in model (§7.1/§9). Config-only opaque blocks not echoed by Read are
// likewise preserved.
func applyClusterToModel(m *clusterResourceModel, c acm.Cluster) {
	m.ID = types.StringValue(strconv.FormatInt(c.ID, 10))
	m.Name = types.StringValue(c.Name)
	// Don't clobber RequiresReplace fields with "" when ACM returns sparse data.
	if c.Type != "" {
		m.Type = types.StringValue(c.Type)
	}
	if c.Role != "" {
		m.Role = types.StringValue(c.Role)
	}
	// node_count: reflect the settled count from the API when non-zero (c.Nodes
	// is 0 mid-provisioning when the node array is still empty). Preserving a
	// zero read would cause a perpetual rescale diff, so we only update when the
	// cluster is healthy and nodes are reporting.
	if c.Nodes > 0 {
		m.NodeCount = types.Int64Value(c.Nodes)
	}
	m.Shards = types.Int64Value(c.Shards)
	m.Replicas = types.Int64Value(c.Replicas)
	m.Secure = types.BoolValue(c.Secure)
	m.ZoneAwareness = types.BoolValue(c.ZoneAwareness)
	m.MysqlProtocol = types.BoolValue(c.MysqlProtocol)
	m.MysqlPort = types.Int64Value(c.MysqlPort)
	m.KeeperName = types.StringValue(c.KeeperName)
	m.Timezone = types.StringValue(c.Timezone)
	m.Uptime = types.StringValue(c.Uptime)
	// version is the user's DESIRED version (it drives the upgrade dispatcher);
	// system_version is the RUNNING version. Only populate version from the API
	// when the user left it unset (Optional+Computed). Otherwise a patch-expanded
	// or mid-upgrade running version (e.g. config "24.8" vs running "24.8.1.2")
	// would clobber desired config, yielding a perpetual diff that re-triggers
	// upgrade on every apply (review #1).
	if m.Version.IsNull() || m.Version.IsUnknown() {
		m.Version = types.StringValue(c.SystemVersion)
	}
	m.SystemVersion = types.StringValue(c.SystemVersion)
	m.Status = types.StringValue(c.Status)
	m.State = types.StringValue(c.State)
	m.Endpoint = types.StringValue(c.Endpoint)
	m.EndpointHTTP = types.StringValue(c.EndpointHTTP)

	// `environment` is set from the parent id on the wire (immutable; keep prior
	// value if the API omits it).
	if c.IDEnvironment != 0 {
		m.Environment = types.StringValue(strconv.FormatInt(c.IDEnvironment, 10))
	}

	// PublicEndpoint and ReplicateSchema are write-only at launch — ACM does
	// not echo them in cluster responses, so c.PublicEndpoint and
	// c.ReplicateSchema are always the zero value here. Overwriting m.* with
	// them would clobber the plan-supplied value with `false` on every Read,
	// producing a perpetual diff. The domain Cluster keeps the fields for the
	// day ACM exposes them via a status endpoint (see domain.go).
}

// versionDowngradeGuard is a plan-time modifier on `version` that rejects an
// in-place downgrade. ACM cluster version changes go through the upgrade
// endpoint and are forward-only; downgrading the binary in place is unsupported
// by ClickHouse and risks corruption. It compares the numeric version code
// (major.minor.patch.<altinity-build>), so a higher-build Antalya
// (*.20002.altinityantalya) -> lower-build Stable (*.10002.altinitystable) at
// the same ClickHouse version is also treated as a downgrade. To move to an
// older version, replace the cluster (or restore from a backup).
type versionDowngradeGuard struct{}

func (versionDowngradeGuard) Description(context.Context) string {
	return "Rejects in-place ClickHouse version downgrades (upgrades are forward-only)."
}

func (g versionDowngradeGuard) MarkdownDescription(ctx context.Context) string {
	return g.Description(ctx)
}

func (versionDowngradeGuard) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	// No prior version (create) or not-yet-resolved values: nothing to compare.
	if req.StateValue.IsNull() || req.StateValue.IsUnknown() {
		return
	}
	if req.PlanValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}
	oldV, newV := req.StateValue.ValueString(), req.PlanValue.ValueString()
	if compareVersionCodes(newV, oldV) < 0 {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Version downgrade not allowed",
			fmt.Sprintf("Cannot change version from %q to %q: ACM cluster version changes are "+
				"forward-only (ClickHouse does not support in-place downgrades). To move to an older "+
				"version, replace the cluster or restore from a backup. Note an Antalya build outranks "+
				"the same-version Stable build, so switching Antalya -> Stable is treated as a downgrade.",
				oldV, newV),
		)
	}
}

// computedStringFields are Optional+Computed string attrs that ACM does not
// echo in Cluster responses. resolveUnknownComputed nulls them out so Create
// doesn't write unknown values to state.
//
// volume_type/data_path/zookeeper/ip_whitelist are Optional-only (NOT
// Computed), so they are never unknown and intentionally excluded here.
var computedStringFields = []func(*clusterResourceModel) *types.String{
	// node_type/memory/size/storage_class: not present in the Cluster response
	// top-level (they live in per-node NodeType objects). Map once a future
	// spike confirms the read path.
	func(m *clusterResourceModel) *types.String { return &m.NodeType },
	func(m *clusterResourceModel) *types.String { return &m.Memory },
	func(m *clusterResourceModel) *types.String { return &m.Size },
	func(m *clusterResourceModel) *types.String { return &m.StorageClass },
	// version_image: optional override; ACM doesn't echo the resolved image
	// tag back in the Cluster response — system_version carries the version.
	func(m *clusterResourceModel) *types.String { return &m.VersionImage },
	// lb_type: sent at launch but not returned in the Cluster top-level; the
	// endpoint URL carries the resolved address instead.
	func(m *clusterResourceModel) *types.String { return &m.LBType },
}

// computedInt64Fields mirrors computedStringFields for Int64.
var computedInt64Fields = []func(*clusterResourceModel) *types.Int64{
	// node_count: ACM doesn't echo back the node count at the Cluster level;
	// the live nodes array (wire.Cluster.Nodes) is a JSON array object, not a
	// scalar. applyClusterToModel reads it when non-zero; null here is safe.
	func(m *clusterResourceModel) *types.Int64 { return &m.NodeCount },
	// iops/throughput/disks: part of per-node storage config, not echoed in
	// the Cluster top-level response.
	func(m *clusterResourceModel) *types.Int64 { return &m.IOPS },
	func(m *clusterResourceModel) *types.Int64 { return &m.Throughput },
	func(m *clusterResourceModel) *types.Int64 { return &m.Disks },
	// port/http_port: sent at launch (defaults 9900/5123) but not echoed back
	// in the Cluster top-level response.
	func(m *clusterResourceModel) *types.Int64 { return &m.Port },
	func(m *clusterResourceModel) *types.Int64 { return &m.HTTPPort },
}

// resolveUnknownComputed nulls any Optional+Computed attribute that the ACM API
// does not read back and that the user left unset. On Create there is no prior
// state, so such attributes are still unknown after launch + read mapping, and
// Terraform rejects unknown values after apply ("Provider returned invalid
// result object"). Read/Update keep prior values via UseStateForUnknown and
// never hit this. TODO(spike): map these from the API once their response
// fields are confirmed, then drop this fallback.
func resolveUnknownComputed(m *clusterResourceModel) {
	for _, f := range computedStringFields {
		if f(m).IsUnknown() {
			*f(m) = types.StringNull()
		}
	}
	for _, f := range computedInt64Fields {
		if f(m).IsUnknown() {
			*f(m) = types.Int64Null()
		}
	}
}

// refreshReadback updates ONLY the computed read-back attributes from the API
// cluster, leaving the desired plan config (compute/storage/version/networking
// and secrets) untouched. Used by the Update dispatcher's converged-state writes
// so a still-pending sub-mutation's desired values are not clobbered by the
// converged read of a completed one (design §7.2).
func refreshReadback(m *clusterResourceModel, c acm.Cluster) {
	m.ID = types.StringValue(strconv.FormatInt(c.ID, 10))
	m.Status = types.StringValue(c.Status)
	m.State = types.StringValue(c.State)
	m.SystemVersion = types.StringValue(c.SystemVersion)
	m.Endpoint = types.StringValue(c.Endpoint)
	m.EndpointHTTP = types.StringValue(c.EndpointHTTP)
}

// =========================================================================
// small value helpers
// =========================================================================

// stringChanged reports whether a known plan string differs from prior state.
// Unknown plan values (computed, not yet resolved) are treated as no-change so
// they do not spuriously trigger a sub-mutation.
func stringChanged(plan, state types.String) bool {
	if plan.IsUnknown() {
		return false
	}
	return !plan.Equal(state)
}

// int64Changed mirrors stringChanged for Int64.
func int64Changed(plan, state types.Int64) bool {
	if plan.IsUnknown() {
		return false
	}
	return !plan.Equal(state)
}

// rawJSONOrNil renders a (raw-passthrough) string attr to json.RawMessage, or
// nil when null/unknown/empty so `omitempty` drops it. TODO(spike): replaced by
// typed nested-block marshaling.
func rawJSONOrNil(s types.String) []byte {
	if s.IsNull() || s.IsUnknown() {
		return nil
	}
	v := s.ValueString()
	if v == "" {
		return nil
	}
	return []byte(v)
}

// resolvedTimeouts holds the effective per-operation deadlines.
type resolvedTimeouts struct {
	create time.Duration
	update time.Duration
	delete time.Duration
}

// resolveTimeouts parses the optional timeouts block, applying defaults for any
// unset value. Invalid duration strings produce a diagnostic.
func resolveTimeouts(ctx context.Context, obj types.Object) (resolvedTimeouts, diag.Diagnostics) {
	out := resolvedTimeouts{
		create: defaultCreateTimeout,
		update: defaultUpdateTimeout,
		delete: defaultDeleteTimeout,
	}
	var diags diag.Diagnostics
	if obj.IsNull() || obj.IsUnknown() {
		return out, diags
	}
	var tm timeoutsModel
	diags.Append(obj.As(ctx, &tm, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return out, diags
	}
	parse := func(s types.String, dst *time.Duration, which string) {
		if s.IsNull() || s.IsUnknown() || s.ValueString() == "" {
			return
		}
		d, err := time.ParseDuration(s.ValueString())
		if err != nil {
			diags.AddError(
				"Invalid timeout duration",
				fmt.Sprintf("timeouts.%s = %q is not a valid Go duration: %s", which, s.ValueString(), err),
			)
			return
		}
		*dst = d
	}
	parse(tm.Create, &out.create, "create")
	parse(tm.Update, &out.update, "update")
	parse(tm.Delete, &out.delete, "delete")
	return out, diags
}
