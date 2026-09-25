package changeenv

// Readers for git's -z protocols and the row classification the diff plan
// measured (binary, mode-only, rename-only, gitlink, conflict).

import (
	"bytes"
	"strconv"
	"strings"
)

type numstat struct {
	added, removed int
	binary         bool
}

type rawRecord struct {
	srcMode, dstMode, srcOID, dstOID, status, path, oldPath string
	stat                                                    numstat
}

// parseLiveRaw reads `diff --raw -z --no-abbrev` records: ":srcmode dstmode
// srcsha dstsha status[score]\0path\0[newpath\0]`. Renames and copies carry
// the old path first.
func parseLiveRaw(raw []byte) ([]rawRecord, error) {
	parts := bytes.Split(raw, []byte{0})
	out := []rawRecord{}
	for i := 0; i < len(parts) && len(parts[i]) > 0; i++ {
		meta := string(parts[i])
		fields := strings.Fields(strings.TrimPrefix(meta, ":"))
		if len(fields) != 5 || !strings.HasPrefix(meta, ":") {
			return nil, &LiveDiffError{Code: "git-failed", Message: "malformed raw diff record"}
		}
		record := rawRecord{srcMode: fields[0], dstMode: fields[1], srcOID: fields[2], dstOID: fields[3], status: fields[4][:1]}
		i++
		if i >= len(parts) {
			return nil, &LiveDiffError{Code: "git-failed", Message: "raw diff record missing its path"}
		}
		record.path = string(parts[i])
		if record.status == "R" || record.status == "C" {
			record.oldPath = record.path
			i++
			if i >= len(parts) {
				return nil, &LiveDiffError{Code: "git-failed", Message: "raw rename record missing its destination"}
			}
			record.path = string(parts[i])
		}
		out = append(out, record)
	}
	return out, nil
}

// parseLiveNumstat reads `diff --numstat -z` records; a rename carries an empty
// third field and the two paths as the next records. Binary files report "-"
// counts, which is the binary signal, so a failed Atoi there is expected.
func parseLiveNumstat(raw []byte) map[string]numstat {
	out := map[string]numstat{}
	parts := bytes.Split(raw, []byte{0})
	for i := 0; i < len(parts) && len(parts[i]) > 0; i++ {
		fields := strings.SplitN(string(parts[i]), "\t", 3)
		if len(fields) != 3 {
			continue
		}
		entry := numstat{binary: fields[0] == "-"}
		entry.added, _ = strconv.Atoi(fields[0])   // "-" for binary, by design
		entry.removed, _ = strconv.Atoi(fields[1]) // same
		path := fields[2]
		if path == "" && i+2 < len(parts) {
			path = string(parts[i+2])
			i += 2
		}
		out[path] = entry
	}
	return out
}

// classify names the row's kind from the raw record and its numstat, in the
// order the diff plan measured: conflict, gitlink, binary, type change,
// mode-only, rename-only.
func classify(record rawRecord) string {
	stat := record.stat
	switch {
	case record.status == "U":
		return "conflict"
	case record.srcMode == "160000" || record.dstMode == "160000":
		return "submodule"
	case stat.binary:
		return "binary"
	case record.status == "T":
		return "typechange"
	case record.status != "A" && record.status != "D" && stat.added == 0 && stat.removed == 0 && record.srcMode != record.dstMode:
		return "mode"
	case record.status == "R" && stat.added == 0 && stat.removed == 0:
		return "rename"
	}
	return "text"
}

// parseBranches reads for-each-ref lines; a symbolic ref (origin/HEAD) is an
// alias of a branch already listed and is dropped.
func parseBranches(refs []byte) []LiveDiffBranch {
	out := []LiveDiffBranch{}
	for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		fields := strings.Split(line, "\x00")
		if len(fields) != 4 || fields[0] == "" || fields[3] != "" {
			continue
		}
		out = append(out, LiveDiffBranch{Name: fields[0], Head: fields[1], Current: fields[2] == "*"})
	}
	return out
}

func parseCommits(log []byte) []LiveDiffCommit {
	out := []LiveDiffCommit{}
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		fields := strings.Split(line, "\x00")
		if len(fields) != 5 {
			continue
		}
		when, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			when = 0 // git's %ct is numeric; a malformed line keeps the commit, not the time
		}
		out = append(out, LiveDiffCommit{SHA: fields[0], Short: fields[1], Subject: fields[2], Author: fields[3], When: when})
	}
	return out
}
