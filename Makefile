.PHONY: build test testacc lint docs install fmt vet tidy generate release-notes release-snapshot install-hooks check-secrets

BINARY := bin/terraform-provider-altinity

# Local filesystem mirror for `terraform`/`tofu` dev_overrides.
HOSTNAME := registry.terraform.io
NAMESPACE := gorgias
NAME := altinity
VERSION := 0.1.0
OS_ARCH := $(shell go env GOOS)_$(shell go env GOARCH)
INSTALL_DIR := $(HOME)/.terraform.d/plugins/$(HOSTNAME)/$(NAMESPACE)/$(NAME)/$(VERSION)/$(OS_ARCH)

build:
	go build -o $(BINARY) ./cmd/terraform-provider-altinity

test:
	go test -race -count=1 ./...

# Acceptance tests are gated on TF_ACC + a live token/env; offline by default.
testacc:
	TF_ACC=1 go test -race -count=1 -timeout 120m ./internal/provider/...

lint: vet
	@command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck not installed, skipping"

vet:
	go vet ./...

# Regenerate wire endpoints/models from the vendored reference.json.
generate:
	go generate ./...

# Generate provider docs into docs/ for the Terraform Registry.
# Delegated to scripts/gen-docs.sh because our main package lives in cmd/
# (not the module root), which a bare `tfplugindocs generate` cannot build.
docs:
	@bash scripts/gen-docs.sh

# Install the provider into the local filesystem mirror for manual testing.
install: build
	mkdir -p $(INSTALL_DIR)
	cp $(BINARY) $(INSTALL_DIR)/terraform-provider-altinity_v$(VERSION)

# Enable the versioned git hooks (pre-commit secret scan). Run once per clone.
install-hooks:
	git config core.hooksPath .githooks
	@echo "git hooks enabled (core.hooksPath=.githooks); pre-commit will scan for secrets."

# Scan all tracked files for secrets / unredacted prod data (CI + manual full sweep).
# The pre-commit hook scans only staged content; this scans the whole tree.
check-secrets:
	@git ls-files -z | xargs -0 bash scripts/check-secrets.sh

fmt:
	gofmt -w .

tidy:
	go mod tidy

# Print the CHANGELOG.md section for VERSION (e.g. `make release-notes VERSION=0.1.0`).
# Used by the release workflow; handy locally before tagging to sanity-check
# that the section is non-empty.
release-notes:
	@if [ -z "$(VERSION)" ]; then echo "usage: make release-notes VERSION=X.Y.Z" >&2; exit 2; fi
	@bash scripts/release-notes.sh $(VERSION)

# Dry-run a cross-platform release build locally (no signing, no publish).
# Verifies the .goreleaser.yml + binary layout without needing tags or secrets.
release-snapshot:
	@command -v goreleaser >/dev/null || { echo "goreleaser not installed: https://goreleaser.com/install/" >&2; exit 1; }
	goreleaser release --snapshot --clean --skip=sign,publish
