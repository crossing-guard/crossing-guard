#!/usr/bin/env bash
# Shape gate for the browser code (STYLE.md §7 "rough ceilings", made real).
#
# STYLE.md has said since before this file existed that "a function past ~60
# lines or doing more than one nameable job wants splitting" and that "a 330-line
# switch does not [earn its length]" — and marked the whole rule "guidelines, not
# gates". Nothing enforced it, so `doSend` in views/chat.js reached 232 lines and
# 7 levels of nesting while every commit passed. The Go side has gofmt, vet,
# errcheck and staticcheck; the browser side had `node --check`, a syntax parser,
# which accepts a five-thousand-line function without comment.
#
# This counts functions over the ceilings and fails above a ratcheting budget,
# the same shape as vendor-lint.sh. Splitting a function lowers the number; a new
# oversized function raises it and fails the commit that introduced it.
#
# Measurement is a character-level scan that removes line comments, block
# comments, regular-expression literals, and the CONTENTS of '…', "…" and `…`
# before counting braces, so a brace or a quote inside any of them cannot skew
# the count. Regex-versus-division is decided by the preceding significant
# character, the standard heuristic. Ground truth is pinned in
# js-shape-lint.test.sh; run that after touching the scanner.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

CEILING="${1:-0}"        # lower this as functions are split; done = 0
MAX_SPAN="${2:-60}"      # STYLE.md §7: a function past ~60 lines wants splitting
MAX_DEPTH="${3:-4}"      # nesting INSIDE a function body, its own body being 1
ROOT="${4:-internal/daemon/static/js}"

tmp=$(mktemp)
find "$ROOT" -name '*.js' ! -name '*.test.mjs' -print0 \
	| sort -z \
	| xargs -0 awk -v max_span="$MAX_SPAN" -v max_depth="$MAX_DEPTH" '
	FNR == 1 { state = "code"; depth = 0; open = 0; prev = "" }

	{
		clean = ""
		n = length($0)
		for (i = 1; i <= n; i++) {
			c = substr($0, i, 1)
			d = substr($0, i, 2)
			if (state == "code") {
				if (d == "//") break
				if (d == "/*") { state = "block"; i++; continue }
				if (c == "/" && regex_can_start(prev)) { state = "rx"; continue }
				if (c == "\"") { state = "dq"; continue }
				if (c == "'"'"'") { state = "sq"; continue }
				if (c == "`") { state = "tpl"; continue }
				clean = clean c
				if (c != " " && c != "\t") prev = c
			} else if (state == "block") {
				if (d == "*/") { state = "code"; i++ }
			} else if (state == "dq") {
				if (c == "\\") i++; else if (c == "\"") { state = "code"; prev = "\"" }
			} else if (state == "sq") {
				if (c == "\\") i++; else if (c == "'"'"'") { state = "code"; prev = "'"'"'" }
			} else if (state == "tpl") {
				if (c == "\\") i++; else if (c == "`") { state = "code"; prev = "`" }
			} else if (state == "rx") {
				if (c == "\\") i++
				else if (c == "[") in_class = 1
				else if (c == "]") in_class = 0
				else if (c == "/" && !in_class) { state = "code"; prev = "x" }
			}
		}

		# Does a brace on THIS line open a function body? Control-flow keywords
		# are excluded so `if (x) {` counts as nesting, not as a function.
		header = 0
		if (clean ~ /(^|[^A-Za-z0-9_$])function[ \t]*[A-Za-z0-9_$]*[ \t]*\(/) header = 1
		if (clean ~ /=>[ \t]*\{/) header = 1
		if (clean ~ /(^|[ \t;=(,])(async[ \t]+)?[A-Za-z0-9_$]+[ \t]*\([^()]*\)[ \t]*\{/ \
			&& clean !~ /(^|[^A-Za-z0-9_$])(if|for|while|switch|catch|do|else|try|return)[ \t]*\(/) header = 1

		m = length(clean)
		for (i = 1; i <= m; i++) {
			c = substr(clean, i, 1)
			if (c == "{") {
				depth++
				if (header) {
					open++
					f_line[open] = FNR; f_depth[open] = depth; f_max[open] = 1
					f_name[open] = name_of(clean)
					header = 0
				}
				for (k = 1; k <= open; k++) {
					rel = depth - f_depth[k] + 1
					if (rel > f_max[k]) f_max[k] = rel
				}
			} else if (c == "}") {
				if (open > 0 && depth == f_depth[open]) {
					span = FNR - f_line[open] + 1
					over_span = (span > max_span)
					over_depth = (f_max[open] > max_depth)
					if (over_span || over_depth) {
						why = over_span ? (over_depth ? "span+depth" : "span") : "depth"
						printf "%5d  %2d  %-10s %s:%d  %s\n", \
							span, f_max[open], why, FILENAME, f_line[open], f_name[open]
					}
					open--
				}
				if (depth > 0) depth--
			}
		}
	}

	# A "/" opens a regex when the previous significant character cannot end an
	# expression. After an identifier, number, ")" or "]" it is division.
	function regex_can_start(p) {
		if (p == "") return 1
		return (p ~ /[({\[,;:=!&|?+\-*%<>~^]/)
	}

	function name_of(text) {
		if (match(text, /function[ \t]+[A-Za-z0-9_$]+/))
			return substr(text, RSTART + 9, RLENGTH - 9)
		if (match(text, /[A-Za-z0-9_$]+[ \t]*=[ \t]*(async[ \t]+)?(function|\(|[A-Za-z0-9_$]+[ \t]*=>)/))
			return substr(text, RSTART, RLENGTH)
		if (match(text, /[A-Za-z0-9_$]+[ \t]*:[ \t]*(async[ \t]+)?(function|\()/))
			return substr(text, RSTART, RLENGTH)
		if (match(text, /[A-Za-z0-9_$]+[ \t]*\(/))
			return substr(text, RSTART, RLENGTH) "…) callback"
		return "(anonymous)"
	}
' >"$tmp" 2>/dev/null

sort -rn "$tmp"
total=$(wc -l <"$tmp" | tr -d ' ')
rm -f "$tmp"
echo "-----------------------------------------------"
echo "browser functions over ceilings (span > ${MAX_SPAN} lines, or nesting > ${MAX_DEPTH}): $total   (ceiling: $CEILING)"
if [ "$total" -gt "$CEILING" ]; then
	echo "JS-SHAPE-LINT FAIL — split the function, do not raise the ceiling (STYLE.md §7)"
	exit 1
fi
echo "JS-SHAPE-LINT PASS"
