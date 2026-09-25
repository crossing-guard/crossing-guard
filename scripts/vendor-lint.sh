#!/usr/bin/env bash
# Open/closed gate (ADR 0020): vendor identity must live ONLY in per-vendor files
# or a registry file. Every vendor token in a generic file is a spot you'd have to
# MODIFY to change a vendor. This counts them and fails above a ratcheting ceiling.
#
# Allowed (not counted): files whose basename is <vendor>.go or *_<vendor>.go
# (claude/codex/opencode), and *registry*.go. Everything else is generic.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

CEILING="${1:-0}" # lower this as each migration step lands; done = 0
total=0
tmp=$(mktemp)

while IFS= read -r f; do
	base=$(basename "$f")
	case "$base" in
		claude.go|codex.go|opencode.go|*_claude.go|*_codex.go|*_opencode.go|*registry*.go) continue ;;
		# Revision-suffixed vendor files (natural-session plan, Slice A): the
		# adapter rename carries the measured CLI revision in the basename
		# (claude_2_1_212.go, graph_codex_0_149_0.go, …). Still vendor files.
		claude_*.go|codex_*.go|opencode_*.go|*_claude_*.go|*_codex_*.go|*_opencode_*.go) continue ;;
		# OpenAI became a speech backend on 2026-09-01 (internal/transcription):
		# its adapter files carry the vendor name like every other adapter.
		openai.go|*_openai.go|openai_*.go) continue ;;
	esac
	# Count EVERY vendor token on a non-comment line — in double-quoted strings,
	# backtick raw-strings (e.g. usage() banners), and identifiers alike. An
	# earlier version scanned only double-quotes and CamelCase idents and MISSED
	# the backtick help banner (caught by the vendor-abstraction review 2026-07-18);
	# counting the bare token case-insensitively closes that blind spot.
	code=$(sed -E 's://.*$::' "$f")
	n=$(printf '%s\n' "$code" | grep -oiE '(claude|codex|opencode|openai)' | wc -l | tr -d ' ')
	if [ "$n" -gt 0 ]; then
		printf '%5d  %s\n' "$n" "$f" >>"$tmp"
		total=$((total + n))
	fi
done < <(find engine store harvest memory internal cmd -name '*.go' ! -name '*_test.go')

sort -rn "$tmp"
rm -f "$tmp"
echo "-----------------------------------------------"
echo "generic vendor tokens: $total   (ceiling: $CEILING)"
if [ "$total" -gt "$CEILING" ]; then
	echo "VENDOR-LINT FAIL — coupling above ceiling (ADR 0020: drive to 0)"
	exit 1
fi
echo "VENDOR-LINT PASS"
