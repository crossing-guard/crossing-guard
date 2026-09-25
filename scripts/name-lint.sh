#!/usr/bin/env bash
# One name: crossing-guard. Never `cg`.
#
# The product used to print two names for itself — `Export first: cg export` —
# where `cg` was a symlink nobody was told to create. A user who copied that line
# got "command not found" from the tool that had just told them to run it. The
# rename is only worth doing once, so this keeps it done.
#
# Deliberate exclusions, each a decision and not an oversight:
#   cg:<name>      the console's internal DOM event namespace (cg:info, cg:center).
#                  Invisible to users; renaming is churn.
#   X-CG-Token     a wire header spoken between an installed hook and the daemon.
#   CG_*           environment variables.
#                  Both are CONFIG/WIRE contracts, not prose: renaming them needs a
#                  compat window (accept both, send new), which is its own commit.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

# The scan runs in perl, not grep -P: BSD grep (macOS) has no -P, and with
# stderr swallowed it reports a clean tree instead of an unsupported flag. That
# is a lint that passes because it never ran — caught by planting a violation,
# which is why the gate step below is worth having at all.
# This script is the one file allowed to spell the old name — it has to, to
# explain itself — so it excludes itself rather than being written around.
files=$(find engine store harvest memory infer internal cmd ruledoc schemas scripts docs README.md \
	\( -name '*.go' -o -name '*.md' -o -name '*.sh' \) -type f \
	| grep -v 'internal/daemon/static/' \
	| grep -v 'scripts/name-lint.sh') || exit 2

# close ARGV if eof — without it $. accumulates across files and every reported
# line number after the first file is fiction.
# A line may opt out with a trailing `name-lint: historical` marker. That is for
# code which must RECOGNISE the old name rather than use it — an install made
# before the rename points at the old symlink, and refusing to recognise our own
# past work would ask a long-time user to consent to what they already consented
# to. The marker keeps each exception visible and greppable instead of widening
# the rule until it stops biting.
hits=$(printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 perl -ne '
	print "$ARGV:$.: $_" if /\bcg\b(?!:)/ && !/x-cg-token/i && !/name-lint: historical/;
	close ARGV if eof;
') || exit 2

if [ -n "$hits" ]; then
	echo "FAIL — the product has one name, and it is not 'cg':"
	echo "$hits"
	echo
	echo "fix: spell it crossing-guard. If this is the cg: DOM namespace or the"
	echo "X-CG-Token header, this lint already excludes them — check the spelling."
	exit 1
fi

# Track C closed the previous environment/header compatibility window. Keep this
# in the existing naming owner rather than growing a second contract-lint script.
# The self-check prevents a malformed regex from turning the source scan into a
# permanent false-green.
RETIRED_CONTRACT_RE='\b(?:CP_(?:DETECTORS|POLICY|RULES)|X-CP-Token)\b'
export RETIRED_CONTRACT_RE
if ! printf '%s\n' 'CP_POLICY' | perl -ne '$found ||= /$ENV{RETIRED_CONTRACT_RE}/; END { exit !$found }'; then
	echo "FAIL — retired-contract lint self-check did not detect its planted name"
	exit 1
fi
retired_hits=$(find engine store harvest memory infer internal cmd ruledoc schemas \
	\( -name '*.go' -o -name '*.js' -o -name '*.html' \) -type f \
	| grep -v '_test\.go$' \
	| grep -v 'scripts/name-lint.sh' \
	| xargs perl -ne 'print "$ARGV:$.: $_" if /$ENV{RETIRED_CONTRACT_RE}/; close ARGV if eof;') || exit 2
if [ -n "$retired_hits" ]; then
	echo "FAIL — a retired CP_* environment variable or X-CP-Token wire name resurfaced:"
	echo "$retired_hits"
	exit 1
fi
echo "clean — one product name; retired config and wire names absent from production"
