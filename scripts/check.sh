#!/usr/bin/env bash
# Public source quality gate. Missing inputs are failures, never skipped checks.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
for tool in go gofmt node rg perl python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "Missing required tool: $tool" >&2; exit 2; }
done
for path in go.mod go.sum engine store harvest memory infer internal cmd codemap ruledoc profiledoc schemas teamwire analyzers/php/go.mod analyzers/javascript/go.mod internal/daemon/static/js scripts/errcheck-excludes.txt scripts/js-shape-lint.sh scripts/js-shape-lint.test.sh scripts/api-contract-lint.sh scripts/api-contract-lint.test.sh scripts/vendor-lint.sh scripts/vendor-lint-js.mjs scripts/vendor-lint-js.test.mjs scripts/vendor-lint-js.baseline.json scripts/module-graph-gate.py scripts/name-lint.sh; do
  test -e "$path" || { echo "Missing required source or check: $path" >&2; exit 2; }
done
fail=0
run() { "$@" || fail=1; }
echo 'Checking formatting'
formatted=$(gofmt -l engine store harvest memory infer internal cmd codemap ruledoc profiledoc schemas teamwire analyzers) || fail=1
if test -n "$formatted"; then printf '%s\n' "$formatted"; fail=1; fi
run go vet ./...
run go tool errcheck -ignoretests -exclude scripts/errcheck-excludes.txt ./...
run go tool staticcheck ./...
errcheck_bin=$(go tool -n errcheck) || exit 2
staticcheck_bin=$(go tool -n staticcheck) || exit 2
for module in analyzers/php analyzers/javascript; do
  (
    cd "$module" || exit 2
    module_fail=0
    go vet ./... || module_fail=1
    "$errcheck_bin" -ignoretests ./... || module_fail=1
    "$staticcheck_bin" ./... || module_fail=1
    exit "$module_fail"
  ) || fail=1
done
js_files=$(rg --files internal/daemon/static/js -g '*.js') || exit 2
js_tests=$(rg --files internal/daemon/static/js -g '*.test.mjs') || exit 2
while IFS= read -r file; do node --input-type=module --check < "$file" || fail=1; done <<< "$js_files"
while IFS= read -r file; do run node --test "$file"; done <<< "$js_tests"
run bash scripts/js-shape-lint.test.sh
run bash scripts/js-shape-lint.sh 45
run python3 scripts/module-graph-gate.py
run bash scripts/api-contract-lint.test.sh
run bash scripts/api-contract-lint.sh 46 11 0
run bash scripts/vendor-lint.sh 13
run node --test scripts/vendor-lint-js.test.mjs
run node scripts/vendor-lint-js.mjs
run bash scripts/name-lint.sh
stale=$(rg -l 'control-plane' engine store harvest memory infer internal cmd ruledoc profiledoc schemas teamwire -g '*.go' -g '*.html')
stale_status=$?
if test "$stale_status" -gt 1; then fail=1; fi
if test -n "$stale"; then printf 'Stale product naming in:\n%s\n' "$stale"; fail=1; fi
# -timeout is per test binary and detects a hang; the daemon race suite alone runs
# for well over the 10-minute default on a loaded machine.
# Fixtures must not scan the developer's installed vendor sessions.
# Retain the disposable home for failure diagnosis; the OS manages its temp lifetime.
test_home=$(mktemp -d) || exit 2
echo "Isolated test home: $test_home"
test_gocache=$(go env GOCACHE) || exit 2
test_modcache=$(go env GOMODCACHE) || exit 2
test_gopath=$(go env GOPATH) || exit 2
run env HOME="$test_home" GOCACHE="$test_gocache" GOMODCACHE="$test_modcache" GOPATH="$test_gopath" go test -race -timeout 31m ./...
for module in analyzers/php analyzers/javascript; do
  (cd "$module" && env HOME="$test_home" GOCACHE="$test_gocache" GOMODCACHE="$test_modcache" GOPATH="$test_gopath" go test -race -timeout 31m ./...) || fail=1
done
if test "$fail" -eq 0; then echo 'GATE PASS'; else echo 'GATE FAIL'; fi
exit "$fail"
