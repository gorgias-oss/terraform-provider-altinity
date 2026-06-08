#!/usr/bin/env bash
# Print the CHANGELOG.md section for a given version to stdout.
#
# Usage:
#   scripts/release-notes.sh 0.1.0
#
# Walks CHANGELOG.md and prints the body of the section whose heading
# matches `## [<version>]` (optionally followed by " — <date>" or " - <date>").
# Stops at the next `## ` heading. Exits 0 even if the section is empty —
# the caller is expected to verify the output is non-empty (the release
# workflow does this and fails the job with a clear error if it isn't).
set -euo pipefail

if [ $# -ne 1 ]; then
    echo "usage: $0 <version>" >&2
    exit 2
fi

VERSION="$1"
CHANGELOG="${CHANGELOG:-CHANGELOG.md}"

if [ ! -f "${CHANGELOG}" ]; then
    echo "${CHANGELOG} not found" >&2
    exit 1
fi

# awk extracts the section body between "## [<version>]" and the next "## ".
# - Match the heading literally (the version is treated as a fixed string,
#   not a regex — `.` in semver pre-release identifiers would otherwise
#   become a regex metachar).
# - Print everything strictly BETWEEN the matched heading and the next "## "
#   heading. Trailing blank lines are stripped.
awk -v version="${VERSION}" '
    BEGIN { in_section = 0 }
    {
        # Heading for the requested version. Accept "## [v]" with optional
        # trailing " — date" or " - date".
        if (!in_section && index($0, "## [" version "]") == 1) {
            in_section = 1
            next
        }
        # Any subsequent "## " heading closes the section.
        if (in_section && index($0, "## ") == 1) {
            in_section = 0
            exit
        }
        if (in_section) {
            print
        }
    }
' "${CHANGELOG}" | awk '
    # Trim leading and trailing blank lines.
    /^$/ { if (in_body) buf = buf $0 "\n"; next }
    {
        if (!in_body) { in_body = 1 }
        print buf $0
        buf = ""
    }
'
