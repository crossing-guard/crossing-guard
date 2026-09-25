#!/usr/bin/env bash
# Ground truth for api-contract-lint.sh. It fails builds, so every counting rule
# gets a fixture. Run after touching the lint.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

fx=$(mktemp -d)
trap 'rm -f "$fx"/go/*.go "$fx"/js/*.js "$fx"/*.md; rmdir "$fx"/go "$fx"/js "$fx" 2>/dev/null' EXIT
mkdir -p "$fx/go" "$fx/js"
fail=0

check() {
	if [ "$2" = "$3" ]; then printf '  ok    %s\n' "$1"
	else printf '  FAIL  %s\n        want: %s\n        got:  %s\n' "$1" "$3" "$2"; fail=1; fi
}

cat > "$fx/go/routes.go" <<'GO'
package fixture

func register() {
	mux.HandleFunc("GET /api/documented", handle)
	mux.HandleFunc("POST /api/v1/aliased", handle)
	mux.HandleFunc("GET /api/params/{id}/detail", handle)
	mux.HandleFunc("DELETE /api/missing", handle)
	mux.HandleFunc("GET /healthz", handle)
}

func typedResponse(w http.ResponseWriter) {
	writeJSON(w, SomeDeclaredType{Field: 1})
}

func untypedResponse(w http.ResponseWriter) {
	writeJSON(w, map[string]any{"note": "no schema here"})
}

func untypedEncoder(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]string{"code": "x"})
}
GO

cat > "$fx/go/routes_test.go" <<'GOT'
package fixture

func testOnly() { writeJSON(w, map[string]any{"ignored": true}) }
GOT

cat > "$fx/reference.md" <<'MD'
GET /api/documented
POST /api/aliased
GET /api/params/{id}/detail
MD

cat > "$fx/js/client.js" <<'JS'
const ROOT = '/api/v1/things';
async function good() { return fetch('/api/things'); }
async function alsoGood() { return fetch(ROOT + '/1'); }
JS

out=$(./scripts/api-contract-lint.sh 999 999 999 "$fx/go" "$fx/reference.md" "$fx/js")
# "|" as the delimiter: the labels contain "/api/".
n() { printf '%s\n' "$out" | sed -n "s|^$1:[[:space:]]*\([0-9][0-9]*\).*|\1|p"; }

check "a declared type is not counted" \
	"$(printf '%s\n' "$out" | grep -c 'typedResponse')" "0"
check "two untyped write sites are counted" "$(n 'untyped JSON write sites')" "2"
check "_test.go files are excluded" \
	"$(printf '%s\n' "$out" | grep -c 'routes_test')" "0"

check "one route is undocumented" "$(n 'undocumented routes')" "1"
check "the undocumented route is the right one" \
	"$(printf '%s\n' "$out" | grep -c 'DELETE /api/missing')" "1"
check "a route registered under v1 counts as documented without it" \
	"$(printf '%s\n' "$out" | grep -c '/api/v1/aliased')" "0"
check "a path parameter matches its documented form" \
	"$(printf '%s\n' "$out" | grep -c '/api/params')" "0"
check "non-/api/ routes are not part of the contract" \
	"$(printf '%s\n' "$out" | grep -c 'healthz')" "0"

check "console calls under /api/ are clean" "$(n 'calls outside /api/')" "0"

# Now break each rule and confirm it is caught.
cat > "$fx/js/offsite.js" <<'JS'
async function bad() { return fetch('https://telemetry.example.com/collect'); }
JS
cat > "$fx/js/badroot.js" <<'JS'
const ROOT = '/internal/secret';
JS
out2=$(./scripts/api-contract-lint.sh 999 999 999 "$fx/go" "$fx/reference.md" "$fx/js")
check "an external fetch is caught" \
	"$(printf '%s\n' "$out2" | sed -n 's|^calls outside /api/:[[:space:]]*\([0-9]*\).*|\1|p')" "2"

./scripts/api-contract-lint.sh 1 999 999 "$fx/go" "$fx/reference.md" "$fx/js" >/dev/null 2>&1
check "untyped above its ceiling fails" "$?" "1"
./scripts/api-contract-lint.sh 999 0 999 "$fx/go" "$fx/reference.md" "$fx/js" >/dev/null 2>&1
check "undocumented above its ceiling fails" "$?" "1"
./scripts/api-contract-lint.sh 999 999 1 "$fx/go" "$fx/reference.md" "$fx/js" >/dev/null 2>&1
check "offsite above its ceiling fails" "$?" "1"
./scripts/api-contract-lint.sh 2 1 2 "$fx/go" "$fx/reference.md" "$fx/js" >/dev/null 2>&1
check "all three at their ceilings passes" "$?" "0"

if [ "$fail" -eq 0 ]; then echo "API-CONTRACT-LINT SELF-TEST PASS"; else echo "API-CONTRACT-LINT SELF-TEST FAIL"; fi
exit "$fail"
