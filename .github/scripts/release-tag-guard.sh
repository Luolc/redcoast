#!/bin/sh
# Release guard: the tag must be `v` + the version constant in cmd/redcoast/version.go.
# Usage: release-tag-guard.sh <tag> [version.go]
# Exit 0 when they match, 1 when they differ, 4 when no version can be read.
set -u
tag=$1
source=${2:-cmd/redcoast/version.go}
version=$(sed -n 's/^const version = "\([^"]*\)"$/\1/p' "$source" 2>/dev/null | head -n 1)
if [ -z "$version" ]; then
  echo "no version constant in $source" >&2
  exit 4
fi
if [ "$tag" != "v$version" ]; then
  echo "tag $tag is not v$version (from $source)" >&2
  exit 1
fi
