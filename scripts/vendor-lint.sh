#!/usr/bin/env bash
# Open/closed gate (ADR 0020): vendor identity must live ONLY in per-vendor files
# or a registry file. Every vendor token in a generic file is a spot you'd have to
# MODIFY to change a vendor. This counts them and fails above a ratcheting ceiling.
#
# Allowed (not counted): files whose basename is <vendor>.go or *_<vendor>.go
# (derived from production runtime registration), and *registry*.go.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

CEILING="${1:-0}" # lower this as each migration step lands; done = 0
total=0
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

# Reuse the browser lint's source inventory, not a second provider list. Keep
# historical vendor aliases covered even when their runtime is absent.
runtime_pattern=$(node --input-type=module -e 'import { RUNTIME_NAMES } from "./scripts/vendor-lint-js.mjs"; console.log(RUNTIME_NAMES.join("|"));') || exit 2
runtime_tokens=$(node --input-type=module -e 'import { TOKEN_RUNTIME_NAMES } from "./scripts/vendor-lint-js.mjs"; console.log(TOKEN_RUNTIME_NAMES.join("|"));') || exit 2
if [ -z "$runtime_pattern" ]; then echo "runtime registrations could not be discovered"; exit 2; fi
token_pattern="($runtime_tokens|claude|codex|opencode|openai)"
adapter_pattern="(^|_)($runtime_pattern|openai)(_|\.go$)"

while IFS= read -r f; do
	base=$(basename "$f")
	case "$base" in *registry*.go) continue ;; esac
	if printf '%s\n' "$base" | grep -qE "$adapter_pattern"; then continue; fi
	# Count EVERY vendor token on a non-comment line — in double-quoted strings,
	# backtick raw-strings (e.g. usage() banners), and identifiers alike. An
	# earlier version scanned only double-quotes and CamelCase idents and MISSED
	# the backtick help banner (caught by the vendor-abstraction review 2026-07-18);
	# counting the bare token case-insensitively closes that blind spot.
	code=$(sed -E 's://.*$::' "$f")
	n=$(printf '%s\n' "$code" | grep -oiE "$token_pattern" | wc -l | tr -d ' ')
	if [ "$n" -gt 0 ]; then
		printf '%5d  %s\n' "$n" "$f" >>"$tmp"
		total=$((total + n))
	fi
done < <(find engine store harvest memory internal cmd ruledoc profiledoc schemas teamwire -name '*.go' ! -name '*_test.go')

sort -rn "$tmp"
rm -f "$tmp"
echo "-----------------------------------------------"
echo "generic vendor tokens: $total   (ceiling: $CEILING)"
if [ "$total" -gt "$CEILING" ]; then
	echo "VENDOR-LINT FAIL — coupling above ceiling (ADR 0020: drive to 0)"
	exit 1
fi
echo "VENDOR-LINT PASS"
