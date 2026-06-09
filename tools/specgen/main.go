// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

// Command specgen reads the vendored OpenAPI document
// (internal/acm/wire/reference.json) and emits the mechanical, drift-prone
// scaffolding for the ACM REST client:
//
//   - internal/acm/wire/endpoints_gen.go — an endpoint registry mapping each
//     allowlisted operationId to its HTTP method, path template, and ordered
//     path parameters.
//   - internal/acm/wire/models_gen.go — faithful Go structs for the named
//     components.schemas we consume.
//
// It is driven by an explicit ALLOWLIST of the ~15 operationIds the provider
// actually uses, so we never emit code for the ~224-operation surface we
// ignore. The generator fails loudly if an allowlisted operationId no longer
// resolves in the spec (catching upstream drift at regen time), and the
// codegen-freshness guard test asserts `go generate` produces no diff against
// the committed output.
//
// Faithful-wire-typing rules (design §4.1):
//   - string            -> string
//   - boolean           -> wire.Bool     (the spec says boolean but DbuserEdit
//     returns accessManagement as 0/1; Bool decodes both bool and number forms,
//     see bool.go)
//   - integer / number  -> wire.Number   (the spec's scalar types are loose:
//     "string-ints" such as "2", real ints such as 13128, JSON booleans
//     true/false (e.g. environment.autoPush), and null all occur in live
//     payloads; the hand-written wire.Number decodes every form without loss)
//   - object (bare, no sub-schema)      -> json.RawMessage  (opaque; the domain
//     layer hand-models these — TODO(spike))
//   - array of object                   -> json.RawMessage
//   - $ref to another named schema      -> *<GoName> (pointer, avoids cycles)
//   - array of $ref                     -> []<GoName>
//
// Invoked via `go generate ./...` from internal/acm/wire (see generate.go).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// allowedOps is the explicit allowlist of operationIds the provider consumes.
// Keep this in sync with the client method set; the generator verifies every
// entry resolves in the vendored spec.
var allowedOps = []string{
	// Cluster lifecycle.
	"ClusterLaunch",       // POST   /environment/{environment}/clusters/launch
	"ClusterList",         // GET    /environment/{environment}/clusters
	"ClusterShow",         // GET    /cluster/{id}
	"ClusterStatus",       // GET    /cluster/{cluster}/status
	"ClusterRescale",      // PUT    /cluster/{id}/rescale
	"ClusterUpgrade",      // PUT    /cluster/{id}/upgrade
	"ClusterBackupCreate", // POST   /cluster/{id}/backup
	"ClusterRemove",       // DELETE /cluster/{id}/{terminate}
	// Cluster settings.
	"ClusterSettingList",   // GET    /cluster/{cluster}/settings
	"ClusterSettingAdd",    // POST   /cluster/{cluster}/settings
	"ClusterSettingEdit",   // POST   /cluster-setting/{id}
	"ClusterSettingRemove", // DELETE /cluster-setting/{id}
	// Cluster profiles.
	"ProfileList",   // GET    /cluster/{cluster}/profiles
	"ProfileAdd",    // POST   /cluster/{cluster}/profiles
	"ProfileEdit",   // POST   /profile/{id}
	"ProfileRemove", // DELETE /profile/{id}
	// Per-profile settings — REQUIRED for ACM to actually push a profile to
	// ClickHouse (live-confirmed 2026-06-08): a profile with zero settings
	// is metadata-only in ACM and never reaches the cluster's user_directories,
	// so any user referencing it fails with Code 180 THERE_IS_NO_PROFILE.
	"ProfileSettingsList",   // GET    /profile/{profile}/settings
	"ProfileSettingsAdd",    // POST   /profile/{profile}/settings
	"ProfileSettingsEdit",   // POST   /setting/{id}
	"ProfileSettingsRemove", // DELETE /setting/{id}
	// Cluster users. Two edit paths, NOT interchangeable:
	//   DbuserEditSql    POST /cluster/{cluster}/user/{id} — runs SQL on the
	//                    cluster synchronously. ACM's pre-flight ("Cluster
	//                    check") rejects with HTTP 200 {"error":"Cluster
	//                    check has failed"} for the admin user even when the
	//                    cluster is healthy. Use for regular user edits where
	//                    the operator wants the change applied right now.
	//   DbuserEdit       POST /user/{id} — updates the ACM record only;
	//                    ACM's operator autoPushes the change to ClickHouse
	//                    out of band. Use for admin password rotation (the
	//                    ACM UI uses this path).
	// Note: an earlier spike claimed DbuserEdit returns 404 "Cluster not
	// found" live. That was wrong — the ACM UI demonstrably uses this
	// endpoint to rotate the admin password.
	"DbuserList",      // GET    /cluster/{cluster}/users
	"DbuserAdd",       // POST   /cluster/{cluster}/users
	"DbuserEditSql",   // POST   /cluster/{cluster}/user/{id}
	"DbuserRemoveSql", // DELETE /cluster/{cluster}/user/{id}
	"DbuserEdit",      // POST   /user/{id}
	// CH Keeper (coordination cluster).
	"ClickhouseKeeperLaunch", // POST   /environment/{environment}/keepers
	"ClickhouseKeeperList",   // GET    /environment/{environment}/keepers
	"ClickhouseKeeperEdit",   // POST   /environment/{environment}/keeper/{name}
	"ClickhouseKeeperDelete", // DELETE /environment/{environment}/keeper/{name}
	"ClickhouseKeeperStatus", // GET    /environment/{environment}/keeper/{name}/status
	// Environment + node-type + version discovery.
	"EnvironmentList", // GET /environments
	"NodeTypeList",    // GET /environment/{environment}/nodetypes
	"CloudOptions",    // GET /cloud/{environment}/options (type=versions, ...)
	// Environment lifecycle (altinity_environment resource).
	"EnvironmentRequest", // POST   /environments/request
	"EnvironmentShow",    // GET    /environment/{id}
	"EnvironmentEdit",    // POST   /environment/{id}
	"EnvironmentRemove",  // DELETE /environment/{id}
	// Global cloud options (altinity_regions data source). The non-env-scoped
	// variant of CloudOptions: region discovery happens before any environment
	// exists, so it cannot use the {environment}-scoped endpoint.
	"CloudOptionsGlobal", // GET    /cloud/options
}

// allowedSchemas is the set of named components.schemas we emit faithful wire
// structs for. References to schemas outside this set are degraded to
// json.RawMessage so the generated file stays self-contained.
// fieldTypeOverrides forces a wire field's Go type when the OpenAPI spec
// mis-declares it (verified against live responses). The launch/list/get
// responses return Cluster.nodes as an array of node objects, but the spec
// declares it "string" — decode it opaquely. Likewise DbUser.networks and
// DbUser.databases are sent as comma-strings but returned as JSON arrays
// (DbuserAdd response), so decode them opaquely and normalize in the domain.
var fieldTypeOverrides = map[string]map[string]string{
	"Cluster": {"nodes": "json.RawMessage"},
	"DbUser":  {"networks": "json.RawMessage", "databases": "json.RawMessage"},
}

var allowedSchemas = []string{
	"Cluster",
	"DbUser",
	"Profile",
	"ProfileSetting",
	"Node",
	"Environment",
	"NodeType",
	"CHKeeper",
}

// ---- OpenAPI subset we parse ----

type doc struct {
	OpenAPI    string              `json:"openapi"`
	Paths      map[string]pathItem `json:"paths"`
	Components struct {
		Schemas map[string]schema `json:"components_schemas_placeholder"`
	} `json:"-"`
	RawComponents struct {
		Schemas map[string]schema `json:"schemas"`
	} `json:"components"`
}

type pathItem map[string]operation

type operation struct {
	OperationID string      `json:"operationId"`
	Parameters  []parameter `json:"parameters"`
}

type parameter struct {
	Name string `json:"name"`
	In   string `json:"in"`
}

type schema struct {
	Type       string            `json:"type"`
	Ref        string            `json:"$ref"`
	Properties map[string]schema `json:"properties"`
	Items      *schema           `json:"items"`
}

// ---- resolved endpoint ----

type endpoint struct {
	OpID       string
	Method     string // upper-case HTTP method
	PathTmpl   string // e.g. /cluster/{id}/rescale
	PathParams []string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "specgen:", err)
		os.Exit(1)
	}
}

func run() error {
	// The generator runs with its working directory set to internal/acm/wire
	// (the //go:generate directive lives there).
	specPath := "reference.json"
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("read spec %s: %w", specPath, err)
	}

	var d doc
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("parse spec: %w", err)
	}
	schemas := d.RawComponents.Schemas

	// Resolve allowlisted operations.
	eps, err := resolveEndpoints(&d)
	if err != nil {
		return err
	}

	// Emit endpoints_gen.go.
	if err := writeFormatted("endpoints_gen.go", renderEndpoints(eps)); err != nil {
		return err
	}

	// Emit models_gen.go.
	if err := writeFormatted("models_gen.go", renderModels(schemas)); err != nil {
		return err
	}

	return nil
}

func resolveEndpoints(d *doc) ([]endpoint, error) {
	// Index operationId -> (method, path, params).
	type loc struct {
		method     string
		path       string
		pathParams []string
	}
	index := map[string]loc{}
	for path, item := range d.Paths {
		for method, op := range item {
			if op.OperationID == "" {
				continue
			}
			var params []string
			for _, p := range op.Parameters {
				if p.In == "path" {
					params = append(params, p.Name)
				}
			}
			index[op.OperationID] = loc{
				method:     strings.ToUpper(method),
				path:       path,
				pathParams: params,
			}
		}
	}

	var out []endpoint
	var missing []string
	for _, opID := range allowedOps {
		l, ok := index[opID]
		if !ok {
			missing = append(missing, opID)
			continue
		}
		out = append(out, endpoint{
			OpID:       opID,
			Method:     l.method,
			PathTmpl:   l.path,
			PathParams: l.pathParams,
		})
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("allowlisted operationIds not found in spec: %s", strings.Join(missing, ", "))
	}
	// Stable order = allowlist order (already deterministic).
	return out, nil
}

func renderEndpoints(eps []endpoint) string {
	var b bytes.Buffer
	b.WriteString(genHeader)
	b.WriteString("package wire\n\n")
	b.WriteString("// Endpoint describes a single ACM REST operation resolved from the OpenAPI\n")
	b.WriteString("// spec by operationId.\n")
	b.WriteString("type Endpoint struct {\n")
	b.WriteString("\tOperationID string\n")
	b.WriteString("\tMethod      string\n")
	b.WriteString("\t// PathTemplate uses OpenAPI-style {name} placeholders.\n")
	b.WriteString("\tPathTemplate string\n")
	b.WriteString("\t// PathParams lists the {name} placeholders in template order.\n")
	b.WriteString("\tPathParams []string\n")
	b.WriteString("}\n\n")

	b.WriteString("// Endpoints is the registry of allowlisted ACM operations the provider\n")
	b.WriteString("// consumes. Keyed by operationId. Generated from reference.json.\n")
	b.WriteString("var Endpoints = map[string]Endpoint{\n")
	for _, e := range eps {
		b.WriteString(fmt.Sprintf("\t%q: {\n", e.OpID))
		b.WriteString(fmt.Sprintf("\t\tOperationID:  %q,\n", e.OpID))
		b.WriteString(fmt.Sprintf("\t\tMethod:       %q,\n", e.Method))
		b.WriteString(fmt.Sprintf("\t\tPathTemplate: %q,\n", e.PathTmpl))
		if len(e.PathParams) == 0 {
			b.WriteString("\t\tPathParams:   nil,\n")
		} else {
			b.WriteString("\t\tPathParams:   []string{")
			for i, p := range e.PathParams {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(fmt.Sprintf("%q", p))
			}
			b.WriteString("},\n")
		}
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n\n")

	// Typed operationId constants for compile-time references from the client.
	b.WriteString("// Operation ID constants. Referencing these from the client gives a\n")
	b.WriteString("// compile-time guarantee the operation is in the generated registry.\n")
	b.WriteString("const (\n")
	for _, e := range eps {
		b.WriteString(fmt.Sprintf("\tOp%s = %q\n", e.OpID, e.OpID))
	}
	b.WriteString(")\n")
	return b.String()
}

func renderModels(schemas map[string]schema) string {
	allowed := map[string]bool{}
	for _, s := range allowedSchemas {
		allowed[s] = true
	}

	var b bytes.Buffer
	b.WriteString(genHeader)
	b.WriteString("package wire\n\n")
	b.WriteString("import \"encoding/json\"\n\n")
	b.WriteString("// These are faithful WIRE types: scalar types follow the spec's loose\n")
	b.WriteString("// typing (string-ints and 0|1 ints both arrive as json.Number; bare\n")
	b.WriteString("// objects with no sub-schema are json.RawMessage). The hand-written\n")
	b.WriteString("// domain layer coerces the known scalar fields and (TODO(spike))\n")
	b.WriteString("// strongly types the opaque-object fields.\n\n")

	// Emit in allowlist order for determinism.
	for _, name := range allowedSchemas {
		s, ok := schemas[name]
		if !ok {
			// Should be caught by the freshness guard; emit a marker so the
			// generated file still compiles deterministically.
			b.WriteString(fmt.Sprintf("// MISSING SCHEMA: %s not found in reference.json\n\n", name))
			continue
		}
		b.WriteString(fmt.Sprintf("// %s is the wire representation of components.schemas.%s.\n", name, name))
		b.WriteString(fmt.Sprintf("type %s struct {\n", name))

		// Deterministic field order: sort property names.
		props := make([]string, 0, len(s.Properties))
		for p := range s.Properties {
			props = append(props, p)
		}
		sort.Strings(props)
		for _, p := range props {
			goType := wireGoType(s.Properties[p], allowed)
			if ov, ok := fieldTypeOverrides[name][p]; ok {
				goType = ov
			}
			b.WriteString(fmt.Sprintf("\t%s %s `json:%q`\n", goFieldName(p), goType, p+",omitempty"))
		}
		b.WriteString("}\n\n")
	}
	return b.String()
}

// wireGoType maps an OpenAPI schema to the faithful wire Go type.
func wireGoType(s schema, allowed map[string]bool) string {
	if s.Ref != "" {
		name := refName(s.Ref)
		if allowed[name] {
			return "*" + name
		}
		// Reference to a schema we don't emit; keep opaque.
		return "json.RawMessage"
	}
	switch s.Type {
	case "string":
		return "string"
	case "boolean":
		// Loose scalar: the hand-written wire.Bool decodes JSON booleans,
		// JSON numbers (0/1 — DbuserEdit returns accessManagement as a
		// number even though the spec says boolean), string-bools, and
		// null. See bool.go.
		return "Bool"
	case "integer", "number":
		// Loose scalar: the hand-written wire.Number decodes "2", 13128,
		// true/false (->1/0), and null. See number.go.
		return "Number"
	case "array":
		if s.Items == nil {
			return "json.RawMessage"
		}
		if s.Items.Ref != "" {
			name := refName(s.Items.Ref)
			if allowed[name] {
				return "[]" + name
			}
			return "json.RawMessage"
		}
		switch s.Items.Type {
		case "string":
			return "[]string"
		case "object", "":
			// array of bare objects -> opaque passthrough.
			return "json.RawMessage"
		default:
			return "json.RawMessage"
		}
	case "object", "":
		// Bare object, no declared sub-schema -> opaque. TODO(spike).
		return "json.RawMessage"
	default:
		return "json.RawMessage"
	}
}

func refName(ref string) string {
	i := strings.LastIndex(ref, "/")
	if i < 0 {
		return ref
	}
	return ref[i+1:]
}

// goFieldName converts a JSON property name to an exported Go field name,
// preserving common initialisms where the source already uses them.
func goFieldName(json string) string {
	if json == "" {
		return "Field"
	}
	// The ACM spec uses lowerCamelCase and snake_case (e.g. id_environment).
	parts := strings.FieldsFunc(json, func(r rune) bool { return r == '_' })
	var sb strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		sb.WriteString(strings.ToUpper(part[:1]))
		sb.WriteString(part[1:])
	}
	out := sb.String()
	// Common initialism normalisation for readability.
	for _, init := range []string{"Id", "Http", "Url", "Api", "Ssl", "Dns", "Lb", "Aws", "Ssh", "Tls", "Pvc", "Cidr", "Cpu"} {
		out = fixInitialism(out, init)
	}
	return out
}

// fixInitialism upper-cases a trailing or boundary initialism (e.g. Id -> ID).
func fixInitialism(s, init string) string {
	up := strings.ToUpper(init)
	// Replace when followed by an upper-case letter or end-of-string, so we
	// don't mangle words like "Idle".
	for i := 0; i+len(init) <= len(s); i++ {
		if s[i:i+len(init)] != init {
			continue
		}
		// boundary before: start or previous char is upper/lower boundary
		nextIdx := i + len(init)
		nextOK := nextIdx == len(s) || (s[nextIdx] >= 'A' && s[nextIdx] <= 'Z')
		prevOK := i == 0 || (s[i-1] >= 'a' && s[i-1] <= 'z') || (s[i-1] >= 'A' && s[i-1] <= 'Z')
		if nextOK && prevOK {
			s = s[:i] + up + s[nextIdx:]
			i += len(up) - 1
		}
	}
	return s
}

func writeFormatted(path, src string) error {
	formatted, err := format.Source([]byte(src))
	if err != nil {
		// Write unformatted to aid debugging then fail.
		_ = os.WriteFile(path+".broken", []byte(src), 0o644)
		return fmt.Errorf("gofmt %s: %w", path, err)
	}
	return os.WriteFile(filepath.Clean(path), formatted, 0o644)
}

const genHeader = "// Copyright (c) Gorgias, Inc.\n" +
	"// SPDX-License-Identifier: Apache-2.0\n\n" +
	"// Code generated by tools/specgen; DO NOT EDIT.\n" +
	"// Source: internal/acm/wire/reference.json (vendored OpenAPI spec).\n" +
	"// Regenerate with: go generate ./...\n\n"
