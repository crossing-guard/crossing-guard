package guardcli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"crossing-guard/engine"
	"crossing-guard/internal/observation"
)

const (
	maxShellTokens   = 4096
	maxShellSegments = 512
)

type shellWord struct {
	value string
	index int
	safe  bool
}

type shellRedirect struct {
	value     string
	operation string
	index     int
}

type shellSegment struct {
	words     []shellWord
	redirects []shellRedirect
	unsafe    bool
}

type shellLexItem struct {
	kind       byte // w=word, r=redirect, s=separator
	value      string
	safe       bool
	start, end int
}

func isShellTool(tool string) bool {
	switch tool {
	case "Bash", "shell", "local_shell", "exec_command":
		return true
	default:
		return false
	}
}

// shellResourceClaims extracts only literal resources declared by a bounded command
// grammar. It does not execute the command, inspect the filesystem, or claim that an
// operation occurred.
func shellResourceClaims(in hookInput) []observation.ResourceClaim {
	if !isShellTool(engine.BareTool(strings.TrimSpace(in.ToolName))) {
		return nil
	}
	segments, ok := shellSegmentsFromRawInput(in.RawToolInput)
	if !ok {
		return nil
	}
	claims := make([]observation.ResourceClaim, 0)
	for segmentIndex, segment := range segments {
		if len(segment.words) == 0 {
			continue
		}
		command := filepath.Base(segment.words[0].value)
		if stopsShellExtraction(command) {
			break
		}
		if segment.unsafe {
			continue
		}
		claims = append(claims, claimsForShellCommand(in, segmentIndex, command, segment.words[1:])...)
		for _, redirect := range segment.redirects {
			claims = append(claims, declaredClaim(in, "file", redirect.value,
				redirect.operation, fmt.Sprintf("tool_input.command.segment[%d].redirect[%d]", segmentIndex, redirect.index)))
		}
		if len(claims) > 512 {
			return claims[:512]
		}
	}
	return claims
}

func stopsShellExtraction(command string) bool {
	switch command {
	case "cd", "pushd", "popd", ".", "source", "eval", "sh", "bash", "zsh", "dash", "fish", "xargs":
		return true
	default:
		return false
	}
}

func shellSegmentsFromRawInput(raw json.RawMessage) ([]shellSegment, bool) {
	if len(raw) == 0 || string(raw) == "null" || len(raw) > observation.MaxRetainedInput {
		return nil, false
	}
	var wire struct {
		Command json.RawMessage `json:"command"`
	}
	if json.Unmarshal(raw, &wire) != nil || len(wire.Command) == 0 {
		return nil, false
	}
	var command string
	if json.Unmarshal(wire.Command, &command) == nil {
		if len(command) > maxTypedProjectionBytes {
			return nil, false
		}
		prefix, ok := literalHeredocPrefix(command)
		if !ok {
			return nil, false
		}
		items, ok := lexShellCommand(prefix)
		if !ok {
			return nil, false
		}
		return buildShellSegments(items)
	}
	var argv []string
	if json.Unmarshal(wire.Command, &argv) != nil || len(argv) == 0 {
		return nil, false
	}
	total := 0
	segment := shellSegment{words: make([]shellWord, 0, len(argv))}
	for i, arg := range argv {
		total += len(arg)
		if total > maxTypedProjectionBytes || i >= maxShellTokens {
			return nil, false
		}
		segment.words = append(segment.words, shellWord{value: arg, index: i, safe: true})
	}
	return []shellSegment{segment}, true
}

// literalHeredocPrefix retains only syntax before one validated heredoc. The body and
// every later byte stay outside the shell lexer, so source text cannot become resource
// claims. Unsupported or unterminated heredocs fail the whole shell projection closed.
func literalHeredocPrefix(command string) (string, bool) {
	quote := byte(0)
	atWordBoundary := true
	for index := 0; index < len(command); index++ {
		current := command[index]
		if quote != 0 {
			if current == quote {
				quote = 0
			} else if quote == '"' && current == '\\' {
				index++
				if index >= len(command) {
					return "", false
				}
			}
			continue
		}
		if current == '\\' {
			index++
			if index >= len(command) {
				return "", false
			}
			atWordBoundary = false
			continue
		}
		if current == '\'' || current == '"' {
			quote = current
			atWordBoundary = false
			continue
		}
		if current == '#' && atWordBoundary {
			newline := strings.IndexByte(command[index:], '\n')
			if newline < 0 {
				return command, true
			}
			index += newline
			atWordBoundary = true
			continue
		}
		if current == '<' && index+1 < len(command) && command[index+1] == '<' {
			delimiter, bodyStart, valid := literalHeredocDelimiter(command, index+2)
			if !valid || !hasLiteralHeredocTerminator(command, bodyStart, delimiter) {
				return "", false
			}
			return command[:index], true
		}
		atWordBoundary = unicode.IsSpace(rune(current)) || current == ';' || current == '|' || current == '&'
	}
	if quote != 0 {
		return "", false
	}
	return command, true
}

func literalHeredocDelimiter(command string, start int) (string, int, bool) {
	if start >= len(command) || command[start] == '<' || command[start] == '-' {
		return "", 0, false
	}
	for start < len(command) && (command[start] == ' ' || command[start] == '\t') {
		start++
	}
	if start >= len(command) || command[start] == '\n' || command[start] == '\r' {
		return "", 0, false
	}
	end := start
	if command[start] == '\'' || command[start] == '"' {
		quote := command[start]
		start++
		end = start
		for end < len(command) && command[end] != quote {
			if !literalHeredocDelimiterByte(command[end]) {
				return "", 0, false
			}
			end++
		}
		if end >= len(command) || end == start {
			return "", 0, false
		}
		end++
	} else {
		for end < len(command) && literalHeredocDelimiterByte(command[end]) {
			end++
		}
		if end == start {
			return "", 0, false
		}
	}
	delimiterEnd := end
	if command[start-1] == '\'' || command[start-1] == '"' {
		delimiterEnd--
	}
	for end < len(command) && (command[end] == ' ' || command[end] == '\t') {
		end++
	}
	if end < len(command) && command[end] == '\r' {
		end++
	}
	if end >= len(command) || command[end] != '\n' {
		return "", 0, false
	}
	return command[start:delimiterEnd], end + 1, true
}

func literalHeredocDelimiterByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '_'
}

func hasLiteralHeredocTerminator(command string, bodyStart int, delimiter string) bool {
	for lineStart := bodyStart; lineStart <= len(command); {
		lineEnd := strings.IndexByte(command[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(command)
		} else {
			lineEnd += lineStart
		}
		contentEnd := lineEnd
		if contentEnd > lineStart && command[contentEnd-1] == '\r' {
			contentEnd--
		}
		if command[lineStart:contentEnd] == delimiter {
			return true
		}
		if lineEnd == len(command) {
			return false
		}
		lineStart = lineEnd + 1
	}
	return false
}

func lexShellCommand(command string) ([]shellLexItem, bool) {
	items := make([]shellLexItem, 0)
	var word strings.Builder
	wordStart := -1
	wordSafe := true
	quote := byte(0)
	flush := func(end int) bool {
		if wordStart < 0 {
			return true
		}
		if len(items) >= maxShellTokens {
			return false
		}
		items = append(items, shellLexItem{kind: 'w', value: word.String(), safe: wordSafe, start: wordStart, end: end})
		word.Reset()
		wordStart, wordSafe = -1, true
		return true
	}
	separator := func(start, end int) bool {
		if !flush(start) || len(items) >= maxShellTokens {
			return false
		}
		items = append(items, shellLexItem{kind: 's', start: start, end: end})
		return true
	}
	for i := 0; i < len(command); {
		c := command[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				i++
				continue
			}
			if quote == '"' && c == '\\' {
				if i+1 >= len(command) {
					return nil, false
				}
				word.WriteByte(command[i+1])
				i += 2
				continue
			}
			if quote == '"' && (c == '$' || c == '`') {
				wordSafe = false
			}
			word.WriteByte(c)
			i++
			continue
		}
		if unicode.IsSpace(rune(c)) {
			if !flush(i) {
				return nil, false
			}
			if c == '\n' && !separator(i, i+1) {
				return nil, false
			}
			i++
			continue
		}
		if c == '\'' || c == '"' {
			if wordStart < 0 {
				wordStart = i
			}
			quote = c
			i++
			continue
		}
		if c == '\\' {
			if i+1 >= len(command) {
				return nil, false
			}
			if wordStart < 0 {
				wordStart = i
			}
			word.WriteByte(command[i+1])
			i += 2
			continue
		}
		if c == '#' && wordStart < 0 {
			newline := strings.IndexByte(command[i:], '\n')
			if newline < 0 {
				break
			}
			i += newline
			if !separator(i, i+1) {
				return nil, false
			}
			i++
			continue
		}
		if c == ';' || c == '|' || c == '&' {
			end := i + 1
			if c == '&' {
				if end >= len(command) || command[end] != '&' {
					return nil, false
				}
				end++
			} else if c == '|' && end < len(command) && command[end] == '|' {
				end++
			}
			if !separator(i, end) {
				return nil, false
			}
			i = end
			continue
		}
		if c == '<' || c == '>' {
			if !flush(i) {
				return nil, false
			}
			end := i + 1
			op := string(c)
			if end < len(command) && command[end] == c {
				if c == '<' {
					return nil, false
				}
				op, end = ">>", end+1
			}
			if end < len(command) && (command[end] == '&' || command[end] == '<' || command[end] == '>') {
				return nil, false
			}
			if len(items) >= maxShellTokens {
				return nil, false
			}
			items = append(items, shellLexItem{kind: 'r', value: op, safe: true, start: i, end: end})
			i = end
			continue
		}
		if c == '(' || c == ')' {
			return nil, false
		}
		if wordStart < 0 {
			wordStart = i
		}
		if c == '$' || c == '`' || c == '*' || c == '?' || c == '[' || c == '{' || c == '}' || (c == '~' && word.Len() == 0) {
			wordSafe = false
		}
		word.WriteByte(c)
		i++
	}
	if quote != 0 || !flush(len(command)) {
		return nil, false
	}
	return items, true
}

func buildShellSegments(items []shellLexItem) ([]shellSegment, bool) {
	segments := make([]shellSegment, 0)
	current := make([]shellLexItem, 0)
	finish := func() bool {
		if len(current) == 0 {
			return true
		}
		if len(segments) >= maxShellSegments {
			return false
		}
		segment := shellSegment{}
		sawRedirect := false
		for i := 0; i < len(current); i++ {
			item := current[i]
			if item.kind == 'r' {
				sawRedirect = true
				if i+1 >= len(current) || current[i+1].kind != 'w' || !current[i+1].safe {
					segment.unsafe = true
					break
				}
				if len(segment.words) > 0 && item.start == current[i-1].end && allDigits(current[i-1].value) {
					segment.words = segment.words[:len(segment.words)-1]
				}
				op := "write"
				if item.value == "<" {
					op = "read"
				}
				segment.redirects = append(segment.redirects, shellRedirect{value: current[i+1].value, operation: op, index: len(segment.redirects)})
				i++
				continue
			}
			if sawRedirect {
				// Supporting only trailing redirects keeps claim ordinals in literal
				// source order without pretending to model shell argv rewriting.
				segment.unsafe = true
			}
			if !item.safe {
				segment.unsafe = true
			}
			segment.words = append(segment.words, shellWord{value: item.value, safe: item.safe})
		}
		for i := range segment.words {
			segment.words[i].index = i
		}
		segments = append(segments, segment)
		current = current[:0]
		return true
	}
	for _, item := range items {
		if item.kind == 's' {
			if !finish() {
				return nil, false
			}
			continue
		}
		current = append(current, item)
	}
	if !finish() {
		return nil, false
	}
	return segments, true
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func claimsForShellCommand(in hookInput, segment int, command string, args []shellWord) []observation.ResourceClaim {
	claim := func(word shellWord, kind, operation string) observation.ResourceClaim {
		return declaredClaim(in, kind, word.value, operation,
			fmt.Sprintf("tool_input.command.segment[%d].argv[%d]", segment, word.index))
	}
	files := func(words []shellWord, operation string) []observation.ResourceClaim {
		out := make([]observation.ResourceClaim, 0, len(words))
		for _, word := range words {
			if word.value != "-" {
				out = append(out, claim(word, "file", operation))
			}
		}
		return out
	}
	switch command {
	case "cat":
		operands, ok := literalOperands(args, shortFlags("AbEnstTuv"), nil)
		if ok {
			return files(operands, "read")
		}
	case "head", "tail":
		operands, ok := literalOperands(args, shortFlags("qvzfF"), optionValues("n", "c", "-lines", "-bytes"))
		if ok {
			return files(operands, "read")
		}
	case "wc":
		operands, ok := literalOperands(args, shortFlags("cmlwL"), optionValues("-files0-from"))
		if ok {
			return files(operands, "read")
		}
	case "stat":
		operands, ok := literalOperands(args, shortFlags("Lflnqsx"), optionValues("c", "F", "t", "-format", "-printf"))
		if ok {
			return files(operands, "read")
		}
	case "file":
		operands, ok := literalOperands(args, shortFlags("bhiLNnprsSvz0"), optionValues("e", "f", "m", "P", "-exclude", "-files-from", "-magic-file", "-parameter"))
		if ok {
			return files(operands, "read")
		}
	case "shasum":
		operands, ok := literalOperands(args, shortFlags("bcptU0"), optionValues("a"))
		if ok {
			return files(operands, "read")
		}
	case "md5":
		operands, ok := literalOperands(args, shortFlags("pqrstx"), optionValues("s"))
		if ok {
			return files(operands, "read")
		}
	case "sed":
		return sedClaims(in, segment, args)
	case "rg", "grep":
		return searchClaims(in, segment, command, args)
	case "find":
		var roots []observation.ResourceClaim
		for _, word := range args {
			if strings.HasPrefix(word.value, "-") || word.value == "!" || word.value == "(" {
				break
			}
			roots = append(roots, claim(word, "path", "search"))
		}
		return roots
	case "rm", "touch", "rmdir":
		operands, ok := literalOperands(args, shortFlags("dfiPrRvW"), nil)
		if ok {
			return files(operands, "write")
		}
	case "mkdir":
		operands, ok := literalOperands(args, shortFlags("pv"), optionValues("m", "-mode"))
		if ok {
			out := make([]observation.ResourceClaim, 0, len(operands))
			for _, word := range operands {
				out = append(out, claim(word, "directory", "write"))
			}
			return out
		}
	case "tee":
		operands, ok := literalOperands(args, shortFlags("ai"), nil)
		if ok {
			return files(operands, "write")
		}
	case "chmod", "chown":
		operands, ok := literalOperands(args, shortFlags("fhRv"), optionValues("-reference"))
		if ok && len(operands) > 1 {
			return files(operands[1:], "write")
		}
	case "cp", "mv":
		// Target-directory modes reverse ordinary final-operand semantics. Refuse them
		// until a dedicated adapter owns those modes.
		operands, ok := literalOperands(args, shortFlags("afHilnPpRrsvx"), nil)
		if ok && len(operands) >= 2 {
			out := files(operands[:len(operands)-1], "read")
			return append(out, claim(operands[len(operands)-1], "path", "write"))
		}
	case "sqlite3":
		operands, ok := exactOptionOperands(args,
			stringSet("-batch", "-bail", "-column", "-csv", "-header", "-html", "-interactive", "-json", "-line", "-list", "-nofollow", "-readonly", "-safe", "-version"),
			stringSet("-cmd", "-init", "-lookaside", "-maxsize", "-mmap", "-newline", "-nullvalue", "-separator", "-vfs"))
		if ok && len(operands) > 0 {
			return []observation.ResourceClaim{claim(operands[0], "file", "unknown")}
		}
	case "go":
		if len(args) > 0 && args[0].value == "test" {
			operands, ok := exactOptionOperands(args[1:],
				stringSet("-a", "-cover", "-failfast", "-json", "-race", "-short", "-v", "-work", "-x"),
				stringSet("-C", "-asmflags", "-bench", "-benchtime", "-count", "-covermode", "-coverpkg", "-cpu", "-fuzz", "-fuzztime", "-gcflags", "-ldflags", "-list", "-mod", "-modfile", "-overlay", "-p", "-parallel", "-run", "-shuffle", "-tags", "-timeout", "-toolexec", "-vet"))
			if ok {
				out := make([]observation.ResourceClaim, 0, len(operands))
				for _, word := range operands {
					out = append(out, claim(word, "package-pattern", "execute"))
				}
				return out
			}
		}
	case "git":
		for i, word := range args {
			if word.value == "--" {
				out := make([]observation.ResourceClaim, 0, len(args)-i-1)
				for _, path := range args[i+1:] {
					out = append(out, claim(path, "path", "unknown"))
				}
				return out
			}
		}
	}
	return nil
}

func shortFlags(chars string) map[byte]bool {
	out := make(map[byte]bool, len(chars))
	for i := range chars {
		out[chars[i]] = true
	}
	return out
}

func optionValues(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

func stringSet(values ...string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

func exactOptionOperands(args []shellWord, noValue, values map[string]bool) ([]shellWord, bool) {
	operands := make([]shellWord, 0)
	options := true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !arg.safe {
			return nil, false
		}
		if options && arg.value == "--" {
			options = false
			continue
		}
		if options && strings.HasPrefix(arg.value, "-") && arg.value != "-" {
			name, _, hasEquals := strings.Cut(arg.value, "=")
			if noValue[name] && !hasEquals {
				continue
			}
			if values[name] {
				if !hasEquals {
					i++
					if i >= len(args) {
						return nil, false
					}
				}
				continue
			}
			return nil, false
		}
		operands = append(operands, arg)
	}
	return operands, true
}

// literalOperands accepts only explicitly known options. A name beginning with one
// dash represents a long option without its leading "--"; a one-byte name represents
// a short option. Unknown option syntax refuses the adapter for the whole segment.
func literalOperands(args []shellWord, noValueShort map[byte]bool, values map[string]bool) ([]shellWord, bool) {
	operands := make([]shellWord, 0)
	options := true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !arg.safe {
			return nil, false
		}
		if options && arg.value == "--" {
			options = false
			continue
		}
		if options && strings.HasPrefix(arg.value, "--") {
			nameValue := strings.TrimPrefix(arg.value, "--")
			name, _, hasEquals := strings.Cut(nameValue, "=")
			if values["-"+name] {
				if !hasEquals {
					i++
					if i >= len(args) {
						return nil, false
					}
				}
				continue
			}
			return nil, false
		}
		if options && len(arg.value) > 1 && arg.value[0] == '-' && arg.value != "-" {
			body := arg.value[1:]
			if values[body] {
				i++
				if i >= len(args) {
					return nil, false
				}
				continue
			}
			for j := 0; j < len(body); j++ {
				if values[string(body[j])] {
					if j+1 == len(body) {
						i++
						if i >= len(args) {
							return nil, false
						}
					}
					break
				}
				if !noValueShort[body[j]] {
					return nil, false
				}
			}
			continue
		}
		operands = append(operands, arg)
	}
	return operands, true
}

func sedClaims(in hookInput, segment int, args []shellWord) []observation.ResourceClaim {
	inPlace, hasProgram := false, false
	files := make([]shellWord, 0)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !arg.safe {
			return nil
		}
		if arg.value == "--" {
			files = append(files, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg.value, "-i") {
			inPlace = true
			if arg.value == "-i" && i+1 < len(args) && args[i+1].value == "" {
				i++
			}
			continue
		}
		if arg.value == "-n" {
			continue
		}
		if arg.value == "-e" || arg.value == "--expression" {
			i++
			if i >= len(args) {
				return nil
			}
			hasProgram = true
			continue
		}
		if strings.HasPrefix(arg.value, "-e") && len(arg.value) > 2 {
			hasProgram = true
			continue
		}
		if arg.value == "-f" || arg.value == "--file" {
			i++
			if i >= len(args) {
				return nil
			}
			hasProgram = true
			continue
		}
		if strings.HasPrefix(arg.value, "-") {
			return nil
		}
		if !hasProgram {
			hasProgram = true
			continue
		}
		files = append(files, arg)
	}
	operation := "read"
	if inPlace {
		operation = "write"
	}
	out := make([]observation.ResourceClaim, 0, len(files))
	for _, word := range files {
		out = append(out, declaredClaim(in, "file", word.value, operation,
			fmt.Sprintf("tool_input.command.segment[%d].argv[%d]", segment, word.index)))
	}
	return out
}

func searchClaims(in hookInput, segment int, command string, args []shellWord) []observation.ResourceClaim {
	patternProvided, filesMode := false, false
	roots := make([]shellWord, 0)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !arg.safe {
			return nil
		}
		if arg.value == "--" {
			remaining := args[i+1:]
			if !patternProvided && !filesMode && len(remaining) > 0 {
				remaining = remaining[1:]
			}
			roots = append(roots, remaining...)
			break
		}
		if strings.HasPrefix(arg.value, "--") {
			name, _, equals := strings.Cut(strings.TrimPrefix(arg.value, "--"), "=")
			switch name {
			case "glob", "iglob", "type", "type-not", "file", "regexp", "encoding", "max-depth", "max-count", "context", "after-context", "before-context":
				if !equals {
					i++
					if i >= len(args) {
						return nil
					}
				}
				if name == "regexp" {
					patternProvided = true
				}
				continue
			case "files":
				filesMode = true
				continue
			case "hidden", "json", "line-number", "no-heading", "with-filename", "files-with-matches", "files-without-match", "fixed-strings", "ignore-case", "smart-case", "word-regexp", "invert-match", "quiet":
				continue
			default:
				return nil
			}
		}
		if strings.HasPrefix(arg.value, "-") && arg.value != "-" {
			body := strings.TrimPrefix(arg.value, "-")
			if len(body) >= 1 && strings.ContainsRune("geftTABCm", rune(body[0])) {
				if len(body) == 1 {
					i++
					if i >= len(args) {
						return nil
					}
				}
				if body[0] == 'e' {
					patternProvided = true
				}
				continue
			}
			for _, flag := range body {
				if !strings.ContainsRune("nliSuvqHhFwi", flag) {
					return nil
				}
			}
			continue
		}
		if !patternProvided && !filesMode {
			patternProvided = true
			continue
		}
		roots = append(roots, arg)
	}
	_ = command
	out := make([]observation.ResourceClaim, 0, len(roots))
	for _, word := range roots {
		out = append(out, declaredClaim(in, "path", word.value, "search",
			fmt.Sprintf("tool_input.command.segment[%d].argv[%d]", segment, word.index)))
	}
	return out
}
