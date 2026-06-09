// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

// parseACMID converts any ACM int64-string identifier (cluster_id,
// profile_id, user_id, setting_id, …) into the int64 the ACM wire layer
// wants. Leading/trailing whitespace is trimmed for robustness against
// variable-interpolation artifacts. The `field` arg seeds the error message
// so a bad id surfaces with the offending HCL attribute name rather than a
// generic "not an integer".
func parseACMID(field, s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a valid integer: %w", field, s, err)
	}
	return id, nil
}

// splitCompositeID splits a "<head>:<tail>" composite Terraform ID. When
// lastColon is true, splits on the LAST colon (the tail may contain ':' in
// principle — though noColonValidator prevents it at plan time for the
// satellite resources). When false, splits on the FIRST colon (the keeper
// case: env id is numeric, so the first colon is unambiguous).
func splitCompositeID(id string, lastColon bool) (head, tail string, err error) {
	var i int
	if lastColon {
		i = strings.LastIndex(id, ":")
	} else {
		i = strings.Index(id, ":")
	}
	if i <= 0 || i == len(id)-1 {
		return "", "", fmt.Errorf("expected composite ID in the form \"<head>:<tail>\", got %q", id)
	}
	return id[:i], id[i+1:], nil
}

// dataSourceErrorDetail builds an actionable error detail from an ACM client
// error: classifies common cases (auth/not-found) and appends a remediation
// hint that helps the operator fix the underlying problem. op is the symbolic
// operation name (e.g. "ListEnvironments") — reserved for future use in the
// hint text; currently the hint is generic.
func dataSourceErrorDetail(op string, err error) string {
	_ = op // reserved for per-operation hints once they materialize.
	base := err.Error()
	switch {
	case acm.IsUnauthorized(err):
		return base + "\n\nHint: verify the api_token (its scope or expiration) — ACM returned an authentication or authorization error."
	case acm.IsNotFound(err):
		return base + "\n\nHint: verify the resource id exists in ACM (e.g. via the altinity_environment data source for environments)."
	default:
		return base
	}
}

// noColonValidator rejects values that contain ":", which is the reserved
// separator in composite Terraform IDs ("<cluster_id>:<name>"). A colon in
// any name component breaks the split on import and can never be recovered
// without manual state surgery.
type noColonValidator struct{}

func (noColonValidator) Description(context.Context) string {
	return `must not contain ":"`
}

func (v noColonValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (noColonValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if strings.Contains(req.ConfigValue.ValueString(), ":") {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid name",
			fmt.Sprintf(`%s must not contain ":"; that character is reserved as the composite Terraform ID separator and breaks import`, req.Path),
		)
	}
}

// pathSafeNameValidator rejects characters that would alter URL routing if
// substituted into an HTTP path segment. resolvePath URL-escapes path arguments
// so the substitution is byte-safe regardless, but a plan-time guard catches
// the misconfiguration before any request is sent and keeps the error message
// close to the offending attribute. Rejects: ":" (composite-ID separator),
// "/" (path separator — would reshape the endpoint route), control characters
// and whitespace (illegal in path segments).
type pathSafeNameValidator struct{}

func (pathSafeNameValidator) Description(context.Context) string {
	return `must not contain ":", "/", whitespace, or control characters`
}

func (v pathSafeNameValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (pathSafeNameValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	s := req.ConfigValue.ValueString()
	for _, r := range s {
		if r == ':' || r == '/' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r < 0x20 || r == 0x7f {
			resp.Diagnostics.AddAttributeError(
				req.Path,
				"Invalid name",
				fmt.Sprintf("%s must not contain %q; the value is used as a URL path segment and as a composite Terraform ID component", req.Path, string(r)),
			)
			return
		}
	}
}

// stringSlicesEqualUnordered reports whether two []string represent the same
// multiset (same elements with the same counts, regardless of order). Used to
// suppress spurious drift on attributes ACM canonicalizes server-side by
// sorting or otherwise reordering — the order is not semantically meaningful
// but the framework's default deep-equal compares positionally.
func stringSlicesEqualUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		seen[v]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// useStateOrNull{String,Int64} are plan modifiers for an Optional+Computed
// attribute that the ACM API does not echo back. Behavior:
//
//   - If the operator set a value in HCL: framework already wrote it to plan
//     before plan modifiers run; PlanValue is not Unknown; we leave it alone.
//   - If state has a value (operator previously set it, or a future API echo
//     populated it): copy state into plan, same as UseStateForUnknown.
//   - If state is null (the API didn't echo and operator hasn't set it):
//     null the plan too — without this, the framework's "Computed" hint keeps
//     PlanValue Unknown and Terraform diffs it as "+ (known after apply)" on
//     every plan, even though nothing is actually changing.
//
// This is the spec-correct shape for attributes like iops/throughput/memory/
// version_image: ACM accepts them on launch/rescale but never reads them back
// in a top-level field, so the only authoritative source is operator config.
//
// SAFETY: use ONLY for attributes the API never echoes back. If the API
// returns the attribute on Read (even sometimes), use the framework's
// `stringplanmodifier.UseStateForUnknown` / `int64planmodifier.UseStateForUnknown`
// instead — those let Read overwrite state from the API. This helper
// short-circuits to null when state is null, which silences a legitimate
// drift signal if the API DOES echo the attribute. Audit each new
// application against the actual Read path.
//
// String and Int64 variants are kept as separate types because
// terraform-plugin-framework's `planmodifier.String` and `planmodifier.Int64`
// are distinct interfaces — Go generics cannot bridge them without a runtime
// type switch that would defeat the whole purpose. The bodies are
// deliberately identical; if you change one, update the other.

const useStateOrNullDesc = "use state value when present; null otherwise (no spurious 'known after apply' for API-not-echoed attrs)"

type useStateOrNullString struct{}

func (useStateOrNullString) Description(context.Context) string         { return useStateOrNullDesc }
func (useStateOrNullString) MarkdownDescription(context.Context) string { return useStateOrNullDesc }
func (useStateOrNullString) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if !req.PlanValue.IsUnknown() {
		return
	}
	if !req.StateValue.IsNull() {
		resp.PlanValue = req.StateValue
		return
	}
	resp.PlanValue = types.StringNull()
}

type useStateOrNullInt64 struct{}

func (useStateOrNullInt64) Description(context.Context) string         { return useStateOrNullDesc }
func (useStateOrNullInt64) MarkdownDescription(context.Context) string { return useStateOrNullDesc }
func (useStateOrNullInt64) PlanModifyInt64(_ context.Context, req planmodifier.Int64Request, resp *planmodifier.Int64Response) {
	if !req.PlanValue.IsUnknown() {
		return
	}
	if !req.StateValue.IsNull() {
		resp.PlanValue = req.StateValue
		return
	}
	resp.PlanValue = types.Int64Null()
}

// reservedProfileNameValidator rejects names that ACM auto-creates and
// auto-maintains at cluster launch. Trying to manage these from Terraform
// produces opaque ACM failures: 404s on adopt-by-name during a propagation
// window, `{"data": false}` on edits as ACM rebases its own settings on top
// of ours, sporadic "Cluster not found" responses. Forcing a non-reserved
// name at plan time saves operators from a multi-hour debugging session.
//
// The reserved set is `default` and `readonly` (case-insensitive — ACM is
// case-sensitive but operators often hit shift accidentally; reject early).
type reservedProfileNameValidator struct{}

func (reservedProfileNameValidator) Description(context.Context) string {
	return `must not be "default" or "readonly" — those are ACM-bootstrap-managed profiles`
}

func (v reservedProfileNameValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (reservedProfileNameValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	switch strings.ToLower(strings.TrimSpace(req.ConfigValue.ValueString())) {
	case "default", "readonly":
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Reserved profile name",
			fmt.Sprintf("%q is auto-created and maintained by ACM at cluster launch. "+
				"Managing it from Terraform produces opaque ACM errors (404s, "+
				"`{\"data\": false}` on edits) as Terraform and ACM compete over the same "+
				"row. Choose a project-specific name such as `analytics_ro_profile` "+
				"instead.", req.ConfigValue.ValueString()),
		)
	}
}
