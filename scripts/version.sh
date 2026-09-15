#!/bin/sh
# Prints Relay's semantic version (MAJOR.MINOR.PATCH) for a git revision
# (default HEAD), derived from history so it never needs editing:
#
#   start  the newest reachable vX.Y.Z tag, or 0.1.0 at the first commit
#   major  a commit with "BREAKING CHANGE" in its body, a "type!:" subject
#          (e.g. "feat!: drop v1 API") or "[major]" in the subject
#   minor  a subject starting with "feat:" / "feat(scope):" or containing "[minor]"
#   patch  every other commit
#
# Merge commits are ignored. Tag a commit (git tag v1.0.0) to jump to a version.
#
# Usage: sh scripts/version.sh [revision]
set -eu

rev=${1:-HEAD}
tag=$(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*' "$rev" 2>/dev/null || true)
if [ -n "$tag" ]; then
	base=${tag#v}
	range="$tag..$rev"
else
	base=0.1.0
	root=$(git rev-list --max-parents=0 "$rev" | tail -n 1)
	range="$root..$rev"
fi

git log --reverse --no-merges --format='%s%x1f%b%x1e' "$range" | awk -v base="$base" '
BEGIN {
	RS = "\036"; FS = "\037"
	split(base, v, "."); major = v[1] + 0; minor = v[2] + 0; patch = v[3] + 0
}
{
	subject = $1; body = $2
	sub(/^\n+/, "", subject)
	if (subject == "" && body == "") next
	if (body ~ /BREAKING CHANGE/ || subject ~ /^[A-Za-z]+(\([^)]*\))?!:/ || subject ~ /\[major\]/) {
		major++; minor = 0; patch = 0
	} else if (subject ~ /^feat(\([^)]*\))?:/ || subject ~ /\[minor\]/) {
		minor++; patch = 0
	} else {
		patch++
	}
}
END { printf "%d.%d.%d\n", major, minor, patch }'
