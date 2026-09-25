#!/usr/bin/env bash
# Ground truth for js-shape-lint.sh. The lint fails builds, so a false positive
# is expensive: every measurement rule gets a fixture here. Run after touching
# the scanner.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

fixtures=$(mktemp -d)
trap 'rm -f "$fixtures"/*.js; rmdir "$fixtures" 2>/dev/null' EXIT
fail=0

check() {
	if [ "$2" = "$3" ]; then
		printf '  ok    %s\n' "$1"
	else
		printf '  FAIL  %s\n        want: %s\n        got:  %s\n' "$1" "$3" "$2"
		fail=1
	fi
}

cat > "$fixtures/fixture.js" <<'FIXTURE'
function tiny(a) {
  return a + 1;
}

function longOne() {
  const a = 1;
  const b = 2;
  const c = 3;
  const d = 4;
  return a + b + c + d;
}

function deep(a, b) {
  if (a) {
    if (b) {
      return 1;
    }
  }
  return 0;
}

function braceInString() {
  const s = "} } } {";
  const t = '} {';
  const u = `} ${x} {`;
  // } { a comment brace
  /* } { a block brace */
  return s + t + u;
}

function regexWithQuoteAndBrace(x) {
  return x.replace(/"/g, '&quot;').replace(/[{]/g, '').replace(/}/g, '');
}

function division(a, b) {
  const r = a / b / 2;
  return r;
}
FIXTURE

out=$(./scripts/js-shape-lint.sh 999 5 2 "$fixtures")
total=$(printf '%s\n' "$out" | sed -n 's/.*): \([0-9][0-9]*\) .*/\1/p')

# A three-line function is not a finding.
check "tiny is not reported" \
	"$(printf '%s\n' "$out" | grep -c 'tiny')" "0"

# The bug this scanner shipped with: /"/g flipped it into string state and
# corrupted every brace count in the file after it.
check "regex containing a quote and a brace is not reported" \
	"$(printf '%s\n' "$out" | grep -c 'regexWithQuoteAndBrace')" "0"

# The same "/" character as division must not open a regex.
check "division is not reported" \
	"$(printf '%s\n' "$out" | grep -c 'division')" "0"

check "an over-long function is reported once" \
	"$(printf '%s\n' "$out" | grep -c 'longOne')" "1"

check "an over-long function is flagged span-only" \
	"$(printf '%s\n' "$out" | awk '/longOne/ {print $3}')" "span"

check "nesting is measured relative to the function body" \
	"$(printf '%s\n' "$out" | awk '/deep/ {print $2}')" "3"

check "braces in strings, templates and comments add no depth" \
	"$(printf '%s\n' "$out" | awk '/braceInString/ {print $2}')" "1"

check "braces in strings do not move the function end" \
	"$(printf '%s\n' "$out" | awk '/braceInString/ {print $1}')" "8"

check "exactly three fixtures breach the ceilings" "$total" "3"

check "the reported rows match the reported total" \
	"$(printf '%s\n' "$out" | grep -cE '^ *[0-9]+ ')" "3"

# The ceiling is enforced, not advisory.
./scripts/js-shape-lint.sh 2 5 2 "$fixtures" >/dev/null 2>&1
check "three findings against a ceiling of two fails" "$?" "1"
./scripts/js-shape-lint.sh 3 5 2 "$fixtures" >/dev/null 2>&1
check "three findings against a ceiling of three passes" "$?" "0"

if [ "$fail" -eq 0 ]; then
	echo "JS-SHAPE-LINT SELF-TEST PASS"
else
	echo "JS-SHAPE-LINT SELF-TEST FAIL"
fi
exit "$fail"
