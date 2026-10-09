#!/bin/sh
# Fails when a tracked file holds an address in 100.64.0.0/10 other than the
# prefix itself or 100.64.0.1. The pattern takes the whole dotted quad, so
# a longer address with the same leading digits is not read as the first one.
set -u

found=$(git grep -noE '100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.[0-9]+\.[0-9]+(/[0-9]+)?')
bad=$(printf '%s\n' "$found" | grep -vE ':(100\.64\.0\.0/10|100\.64\.0\.1)$')
if [ -n "$bad" ]; then
	echo "address in 100.64.0.0/10 other than the prefix and 100.64.0.1:" >&2
	echo "$bad" >&2
	exit 1
fi
