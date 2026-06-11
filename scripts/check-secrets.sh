#!/usr/bin/env bash
# check-secrets.sh — fail if content contains likely secrets or unredacted
# production data, so they never get committed.
#
# Usage:
#   scripts/check-secrets.sh            # scans STAGED content (pre-commit hook)
#   scripts/check-secrets.sh <files..>  # scans the given working-tree files (CI/manual)
#
# Wired as the pre-commit hook via .githooks/pre-commit (enable with
# `make install-hooks`). Bypass a false positive with `git commit --no-verify`.
#
# Detections (all designed to ignore masked/redacted placeholders):
#   - PEM private keys
#   - JWT / bearer tokens (eyJ….eyJ….sig — e.g. Kubernetes SA tokens)
#   - secret-bearing JSON/HCL fields with a real (non-masked) value
#   - Datadog-style 32-hex API keys
set -uo pipefail

root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$root" || exit 0

# Files to scan: explicit args (working tree) or staged additions/modifications.
files=()
if [ "$#" -gt 0 ]; then
	for f in "$@"; do files+=("$f"); done
	getcontent() { cat -- "$1" 2>/dev/null; }
else
	while IFS= read -r -d '' f; do files+=("$f"); done \
		< <(git diff --cached --name-only --diff-filter=ACM -z)
	getcontent() { git show ":$1" 2>/dev/null; }
fi
[ "${#files[@]}" -eq 0 ] && exit 0

# Don't scan the scanner/hook themselves (they contain the patterns as regexes).
self="scripts/check-secrets.sh"
hook=".githooks/pre-commit"

findings=0
flag() { printf '  %s\n      [%s] %s\n' "$1" "$2" "$3"; findings=$((findings + 1)); }

# A quoted value counts as masked/placeholder (not a real secret) when it is
# empty, all '*', or a known placeholder token.
masked_value='"(\*+|x{3,}|redacted[^"]*|example[^"]*|placeholder|changeme|dummy|test|sample|<[^"]*>)"'

for f in "${files[@]}"; do
	[ -z "$f" ] && continue
	case "$f" in "$self" | "$hook") continue ;; esac
	content="$(getcontent "$f")" || continue
	[ -z "$content" ] && continue
	printf '%s' "$content" | grep -Iq . || continue # skip binary

	# PEM private keys and JWT/bearer tokens are unambiguous — scan every file.
	while IFS= read -r l; do [ -n "$l" ] && flag "$f" "private key" "$l"; done \
		< <(printf '%s\n' "$content" | grep -nE -- '-----BEGIN [A-Z ]*PRIVATE KEY-----' | cut -c1-160)

	while IFS= read -r l; do [ -n "$l" ] && flag "$f" "JWT/bearer token" "$l"; done \
		< <(printf '%s\n' "$content" | grep -nE -- 'eyJ[A-Za-z0-9_=-]{10,}\.eyJ[A-Za-z0-9_=-]{10,}\.[A-Za-z0-9_=-]{8,}' | cut -c1-120)

	# The named-field and 32-hex heuristics are higher-noise: Go source/tests
	# legitimately use short fabricated secrets to exercise redaction logic, and
	# that is not the leak vector. Real captures land in data fixtures
	# (JSON/YAML/tfvars/env), so scope these rules to non-Go files and require a
	# realistic value length (>=16) so short test placeholders don't trip them.
	case "$f" in *.go) continue ;; esac

	while IFS= read -r l; do [ -n "$l" ] && flag "$f" "unmasked secret field" "$l"; done \
		< <(printf '%s\n' "$content" \
			| grep -nEi -- '"(kube_?token|aws_?secret_?key|aws_?private_?key|aws_?key|secret_?key|datadog_?password|client_?key|password|passwd|api[_-]?key|x-auth-token|authorization)"[[:space:]]*:[[:space:]]*"[^"]{16,}"' \
			| grep -viE -- "$masked_value" | cut -c1-160)

	while IFS= read -r l; do [ -n "$l" ] && flag "$f" "datadog-style 32-hex key" "$l"; done \
		< <(printf '%s\n' "$content" | grep -nEi -- '"key"[[:space:]]*:[[:space:]]*"[0-9a-f]{32}"' | cut -c1-160)
done

if [ "$findings" -gt 0 ]; then
	printf '\n\xe2\x9c\x96 check-secrets: %d potential secret(s) / unredacted prod data in scanned content:\n\n' "$findings" >&2
	echo "  Replace with synthetic/REDACTED values before committing." >&2
	echo "  (Genuinely a false positive? bypass with: git commit --no-verify)" >&2
	exit 1
fi
exit 0
