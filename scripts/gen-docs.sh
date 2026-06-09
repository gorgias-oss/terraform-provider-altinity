#!/usr/bin/env bash
# Generate the Terraform Registry documentation under docs/.
#
# Usage:
#   scripts/gen-docs.sh
#
# Why this script exists instead of a bare `tfplugindocs generate`:
# our `main` package lives in ./cmd/terraform-provider-altinity, not at the
# module root. `tfplugindocs generate` builds the provider from the working
# directory and fails with "no Go files in <root>" on this layout. So we:
#
#   1. build the provider binary ourselves,
#   2. install it into a throwaway filesystem mirror under the `hashicorp`
#      namespace (so Terraform keys the exported schema as
#      `registry.terraform.io/hashicorp/altinity`, which is what tfplugindocs
#      matches against `--provider-name altinity`),
#   3. export the schema with `terraform providers schema -json`, and
#   4. render docs with `tfplugindocs --providers-schema`, which skips the
#      build entirely.
#
# Requires: go, terraform (or tofu). tfplugindocs is run via `go run` if not
# installed locally.
set -euo pipefail

PROVIDER_NAME="altinity"
RENDERED_NAME="Altinity.Cloud"
TFPLUGINDOCS_VERSION="v0.25.0"
# A dummy version for the local mirror; the value is irrelevant to the schema,
# but Terraform rejects 0.0.0 as "no available releases", so use a fake non-zero.
VERSION="9.9.9"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

TF_BIN="$(command -v terraform || command -v tofu || true)"
if [ -z "${TF_BIN}" ]; then
    echo "error: terraform (or tofu) not found on PATH" >&2
    exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

OS_ARCH="$(go env GOOS)_$(go env GOARCH)"
MIRROR="${WORK}/mirror/registry.terraform.io/hashicorp/${PROVIDER_NAME}/${VERSION}/${OS_ARCH}"
mkdir -p "${MIRROR}"

echo "==> building provider"
go build -o "${MIRROR}/terraform-provider-${PROVIDER_NAME}_v${VERSION}" ./cmd/terraform-provider-${PROVIDER_NAME}

echo "==> exporting schema via ${TF_BIN##*/}"
cat > "${WORK}/main.tf" <<EOF
terraform {
  required_providers {
    ${PROVIDER_NAME} = {
      source  = "hashicorp/${PROVIDER_NAME}"
      version = "${VERSION}"
    }
  }
}
EOF

# Point Terraform at our throwaway mirror only (no network, no real registry).
cat > "${WORK}/tf.rc" <<EOF
provider_installation {
  filesystem_mirror {
    path    = "${WORK}/mirror"
    include = ["registry.terraform.io/hashicorp/${PROVIDER_NAME}"]
  }
}
EOF

(
    cd "${WORK}"
    TF_CLI_CONFIG_FILE="${WORK}/tf.rc" "${TF_BIN}" init -input=false >/dev/null
    "${TF_BIN}" providers schema -json > "${WORK}/schema.json"
)

echo "==> rendering docs/ with tfplugindocs"
if command -v tfplugindocs >/dev/null; then
    TFPLUGINDOCS=(tfplugindocs)
else
    TFPLUGINDOCS=(go run "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@${TFPLUGINDOCS_VERSION}")
fi

"${TFPLUGINDOCS[@]}" generate \
    --providers-schema "${WORK}/schema.json" \
    --provider-name "${PROVIDER_NAME}" \
    --rendered-provider-name "${RENDERED_NAME}"

echo "==> docs/ generated"
