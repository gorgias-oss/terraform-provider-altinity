// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// allowlistForGuard mirrors tools/specgen.allowedOps. Kept here so the guard is
// self-contained (the generator's allowlist is unexported in package main).
var allowlistForGuard = []string{
	"ClusterLaunch",
	"ClusterList",
	"ClusterShow",
	"ClusterStatus",
	"ClusterRescale",
	"ClusterUpgrade",
	"ClusterBackupCreate",
	"ClusterRemove",
	"ClusterSettingList",
	"ClusterSettingAdd",
	"ClusterSettingEdit",
	"ClusterSettingRemove",
	"ProfileList",
	"ProfileAdd",
	"ProfileEdit",
	"ProfileRemove",
	"DbuserList",
	"DbuserAdd",
	"DbuserEditSql",
	"DbuserRemoveSql",
	"DbuserEdit",
	"ProfileSettingsList",
	"ProfileSettingsAdd",
	"ProfileSettingsEdit",
	"ProfileSettingsRemove",
	"EnvironmentList",
	"NodeTypeList",
	"ClickhouseKeeperLaunch",
	"ClickhouseKeeperList",
	"ClickhouseKeeperEdit",
	"ClickhouseKeeperDelete",
	"ClickhouseKeeperStatus",
	"CloudOptions",
}

// TestAllowlistedOpsResolveInSpec asserts every allowlisted operationId still
// resolves in the vendored reference.json. This guards the *vendored* spec
// against forgotten allowlist drift (design §8.2).
func TestAllowlistedOpsResolveInSpec(t *testing.T) {
	if len(allowlistForGuard) != len(Endpoints) {
		t.Errorf("allowlistForGuard has %d ops but Endpoints registry has %d; they must stay in sync", len(allowlistForGuard), len(Endpoints))
	}
	raw, err := os.ReadFile("reference.json")
	if err != nil {
		t.Fatalf("read reference.json: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse reference.json: %v", err)
	}

	found := map[string]bool{}
	for _, item := range doc.Paths {
		for _, op := range item {
			if op.OperationID != "" {
				found[op.OperationID] = true
			}
		}
	}

	var missing []string
	for _, op := range allowlistForGuard {
		if !found[op] {
			missing = append(missing, op)
		}
		// Also assert it landed in the generated registry.
		if _, ok := Endpoints[op]; !ok {
			t.Errorf("allowlisted op %q not present in generated Endpoints registry", op)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("allowlisted operationIds missing from vendored spec: %s", strings.Join(missing, ", "))
	}
}

// TestGenerateNoDiff runs `go generate` and asserts the committed generated
// files are unchanged — catching a forgotten regeneration in CI (design §8.2).
func TestGenerateNoDiff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go generate diff check in -short mode")
	}

	files := []string{"endpoints_gen.go", "models_gen.go"}
	before := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		before[f] = b
	}

	// Run the generator (cwd is this package dir = internal/acm/wire).
	cmd := exec.Command("go", "run", "github.com/gorgias-oss/terraform-provider-altinity/tools/specgen")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go generate failed: %v\n%s", err, out)
	}

	// Restore originals after comparison regardless of outcome.
	defer func() {
		for f, b := range before {
			_ = os.WriteFile(filepath.Clean(f), b, 0o644)
		}
	}()

	for _, f := range files {
		after, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read regenerated %s: %v", f, err)
		}
		if string(after) != string(before[f]) {
			t.Errorf("%s is stale: `go generate ./...` produced a diff. Run `make generate` and commit.", f)
		}
	}
}
