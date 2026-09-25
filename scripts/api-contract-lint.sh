#!/usr/bin/env bash
# The daemon API is the PUBLIC contract (ADR 0022): the console is a
# zero-privilege reference client, and anyone may bring their own frontend.
# A contract nobody can read is not a contract, so this counts the three ways
# ours is currently unreadable and fails above ratcheting ceilings.
#
#   untyped     JSON written from an inline map literal instead of a declared
#               Go type. There is nothing to generate a schema from, and the
#               key set is whatever that call site happened to write.
#   undocumented  a registered /api/ route with no entry in the loopback
#               reference. A BYO client cannot discover it.
#   offsite     a console network call not provably under /api/. ADR 0022
#               decision 2: the console may use no capability the public API
#               does not expose. This is green today; nothing kept it green.
#
# Same shape as vendor-lint.sh and js-shape-lint.sh: lower the ceiling as the
# work lands. Raising a ceiling is not a fix.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

CEIL_UNTYPED="${1:-0}"
CEIL_UNDOCUMENTED="${2:-0}"
CEIL_OFFSITE="${3:-0}"
# Overridable so the self-test can point the same code at fixtures.
GO_ROOT="${4:-internal/daemon}"
REFERENCE="${5:-docs/public/reference/loopback-api.md}"
JS_ROOT="${6:-internal/daemon/static/js}"

fail=0
go_files=$(find "$GO_ROOT" -name '*.go' ! -name '*_test.go' | sort)

# ---- 1. untyped JSON responses -------------------------------------------
echo "--- untyped JSON responses (inline map literal, no declared type) ---"
untyped_list=$(grep -nE '(writeJSON\(w,[[:space:]]*map\[string\]|json\.NewEncoder\(w\)\.Encode\(map\[string\])' \
	$go_files | sed -E 's/:[[:space:]]*/: /' | sort)
untyped=$(printf '%s' "$untyped_list" | grep -c . )
printf '%s\n' "$untyped_list" | sed -E 's/^([^:]*):([0-9]*):.*/  \1:\2/' | uniq -c | sort -rn | head -12
echo "  … $untyped write sites total"

# ---- 2. undocumented routes ----------------------------------------------
echo "--- registered routes with no entry in $REFERENCE ---"
undocumented=0
undoc_list=""
while IFS= read -r route; do
	[ -n "$route" ] || continue
	path=${route#* }                       # drop the method
	# A path parameter matches anything; check both the /v1/ and bare spellings.
	# /api/v1/X and /api/X are the same handler (main.go's compat alias), so a
	# route registered under either spelling is documented if EITHER appears.
	probe=$(printf '%s' "$path" | sed -E 's/\{[^}]*\}/[^ ),`]*/g; s#^/api/(v1/)?#/api/(v1/)?#')
	if ! grep -qE "$probe" "$REFERENCE" 2>/dev/null; then
		undoc_list="${undoc_list}  ${route}"$'\n'
		undocumented=$((undocumented + 1))
	fi
done < <(grep -rhoE 'HandleFunc\("(GET|POST|PUT|DELETE|PATCH) /api/[^"]*"' $go_files \
	| sed -E 's/HandleFunc\("//; s/"$//' | sort -u)
printf '%s' "$undoc_list" | head -30
echo "  … $undocumented undocumented routes"

# ---- 3. zero-privilege ----------------------------------------------------
echo "--- console network calls not provably under /api/ ---"
offsite=0
off_list=""
# Literal targets.
while IFS= read -r hit; do
	[ -n "$hit" ] || continue
	target=$(printf '%s' "$hit" | sed -E "s/.*(fetch|EventSource)\(['\"\`]//")
	case "$target" in
		/api/*) ;;
		*) off_list="${off_list}  literal: ${hit}"$'\n'; offsite=$((offsite + 1)) ;;
	esac
done < <(grep -rnoE "(fetch|EventSource)\(['\"\`][^'\"\`]*" "$JS_ROOT" \
	| grep -v '\.test\.' | sort)
# Roots the helpers build paths from.
while IFS= read -r hit; do
	[ -n "$hit" ] || continue
	value=$(printf '%s' "$hit" | sed -E "s/.*=[[:space:]]*['\"\`]//; s/['\"\`].*//")
	case "$value" in
		/api/*) ;;
		*) off_list="${off_list}  root: ${hit}"$'\n'; offsite=$((offsite + 1)) ;;
	esac
done < <(grep -rnE "^const ROOT[[:space:]]*=" "$JS_ROOT" | grep -v '\.test\.' | sort)
printf '%s' "$off_list"
echo "  … $offsite calls outside the public API"

# ---- verdict ---------------------------------------------------------------
echo "-----------------------------------------------"
printf 'untyped JSON write sites: %-4s (ceiling: %s)\n' "$untyped" "$CEIL_UNTYPED"
printf 'undocumented routes:      %-4s (ceiling: %s)\n' "$undocumented" "$CEIL_UNDOCUMENTED"
printf 'calls outside /api/:      %-4s (ceiling: %s)\n' "$offsite" "$CEIL_OFFSITE"
[ "$untyped" -gt "$CEIL_UNTYPED" ] && { echo "FAIL — declare a response type; a map literal has no schema"; fail=1; }
[ "$undocumented" -gt "$CEIL_UNDOCUMENTED" ] && { echo "FAIL — document the route; BYO clients cannot read Go"; fail=1; }
[ "$offsite" -gt "$CEIL_OFFSITE" ] && { echo "FAIL — ADR 0022: the console may use no capability the API does not expose"; fail=1; }
[ "$fail" -eq 0 ] && echo "API-CONTRACT-LINT PASS"
exit "$fail"
