<!--
Thanks for contributing! A few things to check before submitting:

- Read CONTRIBUTING.md — it covers the dev loop, coding bar, and security policy.
- Run `make test && make lint && make build` locally.
- Add or update tests for any behavior change.
- Add a CHANGELOG.md entry under "Unreleased" if the change is user-visible.

For security-sensitive changes (anything touching auth, secret handling, the
HTTP client, validators, or any destructive code path), flag it explicitly
in the description so we can route it through a security review.
-->

## Summary

<!-- One or two sentences describing what changed and why. -->

## Type of change

- [ ] Bug fix (non-breaking change that fixes an issue)
- [ ] New feature (non-breaking change that adds capability)
- [ ] Breaking change (would force users to update Terraform configs)
- [ ] Documentation only
- [ ] Internal refactor / chore

## Test plan

<!-- How was this verified? Which `go test` runs were added/updated? -->

- [ ] `make test` passes locally
- [ ] `make lint` passes locally
- [ ] New tests added or existing tests updated
- [ ] Tested against a live ACM environment (acceptance) — optional

## Related issue

<!-- Closes #123 / Refs #456 -->

## Checklist

- [ ] CHANGELOG.md updated (if user-visible)
- [ ] No secrets, tokens, or sample credentials in the diff
- [ ] Schema descriptions updated (if attributes changed)
