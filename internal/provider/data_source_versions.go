// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

var (
	_ datasource.DataSource              = (*versionsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*versionsDataSource)(nil)
)

// versionsDataSource lists the ClickHouse versions available in an environment
// (altinity_clickhouse_versions), with optional major/minor filtering and a
// `latest` selector, so cluster `version` is never hard-coded to an unavailable
// value (which ACM accepts at launch but fails to provision).
type versionsDataSource struct {
	client *acm.Client
}

// NewVersionsDataSource is the constructor registered with the provider.
func NewVersionsDataSource() datasource.DataSource {
	return &versionsDataSource{}
}

// Build streams. These are the values of both the `stream` filter input and the
// per-version `stream` output, so input and output share one vocabulary.
const (
	streamAltinityStable  = "altinity-stable"
	streamAltinityAntalya = "altinity-antalya"
	streamUpstream        = "upstream"
)

// validStreams is the allowed set for the `stream` filter (and the full set of
// `stream` output values).
var validStreams = []string{streamAltinityStable, streamAltinityAntalya, streamUpstream}

type versionsDataSourceModel struct {
	Environment types.String       `tfsdk:"environment"`
	Platform    types.String       `tfsdk:"platform"`
	Major       types.Int64        `tfsdk:"major"`
	Minor       types.Int64        `tfsdk:"minor"`
	Stream      types.String       `tfsdk:"stream"`
	Versions    []versionItemModel `tfsdk:"versions"`
	Latest      types.String       `tfsdk:"latest"`
}

type versionItemModel struct {
	Code   types.String `tfsdk:"code"`
	Name   types.String `tfsdk:"name"`
	Repo   types.String `tfsdk:"repo"`
	Major  types.Int64  `tfsdk:"major"`
	Minor  types.Int64  `tfsdk:"minor"`
	IsEOL  types.Bool   `tfsdk:"is_eol"`
	Stream types.String `tfsdk:"stream"`
}

func (d *versionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_clickhouse_versions"
}

func (d *versionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "List the ClickHouse versions available in an environment " +
			"(GET /cloud/{environment}/options?type=versions). Filter by major/minor and/or " +
			"build `stream`, and use `latest` to avoid hard-coding an unavailable version.",
		Attributes: map[string]schema.Attribute{
			"environment": schema.StringAttribute{
				Required:    true,
				Description: "ACM environment id to list versions for.",
			},
			"platform": schema.StringAttribute{
				Optional: true,
				Description: "Platform to query (e.g. kubernetes). Strongly recommended — without it " +
					"ACM returns a stale/older list. Pass the environment's `type`.",
			},
			"major": schema.Int64Attribute{
				Optional:    true,
				Description: "Optional major-version filter (e.g. 25).",
			},
			"minor": schema.Int64Attribute{
				Optional:    true,
				Description: "Optional minor-version filter (e.g. 8). Requires major to be meaningful.",
			},
			"stream": schema.StringAttribute{
				Optional: true,
				Description: "Optional build-stream filter — one of `altinity-stable` " +
					"(Altinity Stable, *.altinitystable), `altinity-antalya` (Project Antalya, " +
					"*.altinityantalya), or `upstream` (upstream ClickHouse). Unset returns all " +
					"streams. Filtering by a single stream keeps `latest` within one build line " +
					"(otherwise an Altinity Antalya build can outrank an Altinity Stable one).",
			},
			"latest": schema.StringAttribute{
				Computed:    true,
				Description: "The highest version `code` among the (filtered) results, by numeric component order.",
			},
			"versions": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The matching versions, in the API's order.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"code":   schema.StringAttribute{Computed: true, Description: "Version code (use as cluster `version`)."},
						"name":   schema.StringAttribute{Computed: true, Description: "Human label (may include LTS/Altinity Stable/[EOL])."},
						"repo":   schema.StringAttribute{Computed: true, Description: "Image repo (altinity/clickhouse-server vs clickhouse/clickhouse-server)."},
						"major":  schema.Int64Attribute{Computed: true},
						"minor":  schema.Int64Attribute{Computed: true},
						"is_eol": schema.BoolAttribute{Computed: true, Description: "True when the name is marked [EOL]."},
						"stream": schema.StringAttribute{Computed: true, Description: "Build stream: altinity-stable | altinity-antalya | upstream."},
					},
				},
			},
		},
	}
}

func (d *versionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*acm.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Data Source Configure Type",
			fmt.Sprintf("Expected *acm.Client, got %T. This is a provider bug.", req.ProviderData),
		)
		return
	}
	d.client = client
}

func (d *versionsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg versionsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	majorFilter, hasMajor := int64Filter(cfg.Major)
	minorFilter, hasMinor := int64Filter(cfg.Minor)

	var streamFilter string
	hasStream := !cfg.Stream.IsNull() && !cfg.Stream.IsUnknown()
	if hasStream {
		streamFilter = cfg.Stream.ValueString()
		if !slices.Contains(validStreams, streamFilter) {
			resp.Diagnostics.AddAttributeError(
				path.Root("stream"),
				"Invalid stream filter",
				fmt.Sprintf("stream must be one of %v, got %q.", validStreams, streamFilter),
			)
			return
		}
	}

	vs, err := d.client.ListVersions(ctx, cfg.Environment.ValueString(), cfg.Platform.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to list versions", dataSourceErrorDetail("ListVersions", err))
		return
	}

	cfg.Versions = make([]versionItemModel, 0, len(vs))
	var latest string
	for _, v := range vs {
		major, minor := parseVersionMajorMinor(v.Code)
		stream := versionStream(v.Code, v.Name, v.Repo)
		if hasMajor && int64(major) != majorFilter {
			continue
		}
		if hasMinor && int64(minor) != minorFilter {
			continue
		}
		if hasStream && stream != streamFilter {
			continue
		}
		cfg.Versions = append(cfg.Versions, versionItemModel{
			Code:   types.StringValue(v.Code),
			Name:   types.StringValue(v.Name),
			Repo:   types.StringValue(v.Repo),
			Major:  types.Int64Value(int64(major)),
			Minor:  types.Int64Value(int64(minor)),
			IsEOL:  types.BoolValue(strings.HasPrefix(strings.TrimSpace(v.Name), "[EOL]")),
			Stream: types.StringValue(stream),
		})
		if latest == "" || compareVersionCodes(v.Code, latest) > 0 {
			latest = v.Code
		}
	}
	cfg.Latest = types.StringValue(latest)

	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}

func int64Filter(v types.Int64) (int64, bool) {
	if v.IsNull() || v.IsUnknown() {
		return 0, false
	}
	return v.ValueInt64(), true
}

// versionStream is the single classifier for a ClickHouse version's build
// stream (one of validStreams). It is repo-aware (the repo is authoritative;
// code/name are fallbacks for when platform is unset and repo is absent).
func versionStream(code, name, repo string) string {
	lc, ln, lr := strings.ToLower(code), strings.ToLower(name), strings.ToLower(repo)
	switch {
	case strings.Contains(lc, "antalya") || strings.Contains(ln, "antalya"):
		return streamAltinityAntalya
	case strings.Contains(lr, "altinity") || strings.Contains(lc, "altinity") || strings.Contains(ln, "altinity"):
		return streamAltinityStable
	default:
		return streamUpstream
	}
}

// parseVersionMajorMinor extracts the first two dotted integer components of a
// version code ("25.8.16.10002.altinitystable" -> 25, 8). Missing components
// are -1.
func parseVersionMajorMinor(code string) (major, minor int) {
	major, minor = -1, -1
	parts := strings.Split(code, ".")
	if len(parts) > 0 {
		if n, err := strconv.Atoi(parts[0]); err == nil {
			major = n
		}
	}
	if len(parts) > 1 {
		if n, err := strconv.Atoi(parts[1]); err == nil {
			minor = n
		}
	}
	return major, minor
}

