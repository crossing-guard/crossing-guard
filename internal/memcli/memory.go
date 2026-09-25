package memcli

// memory.go — thin shim over crossing-guard/memory (the extracted engine
// package; code-organization-v1 memory promotion). cpmem's CLI keeps its
// historical function names so main/attach/import stay unchanged; new
// callers (the daemon at M5) import crossing-guard/memory directly.

import (
	"crossing-guard/memory"
)

type Record = memory.Record
type Hit = memory.Hit
type SearchOpts = memory.SearchOpts

var categories = memory.Categories

func memoryDir() string                  { return memory.DefaultDir() }
func loadRecords(dir string) []Record    { return memory.Load(dir) }
func loadAllRecords(dir string) []Record { return memory.LoadAll(dir) }

func readRecord(path string) (Record, error) { return memory.Read(path) }
func writeRecord(dir string, r Record) error { return memory.Write(dir, r) }
func validateRecord(r Record) error          { return memory.Validate(r) }

func searchRecords(dir, query string, opts SearchOpts) []Hit {
	return memory.Search(dir, query, opts)
}
func similarRecords(dir string, cand Record) []Record { return memory.Similar(dir, cand) }

func gitCommit(dir, msg string) { memory.GitCommit(dir, msg) }
func logRecall(dir, op, query string, results []string, nonce ...string) {
	memory.LogRecall(dir, op, query, results, nonce...)
}
func buildIndex(dir string, maxBytes int, project string) string {
	return memory.BuildIndex(dir, maxBytes, project)
}

func isTombstoned(dir, id string) bool          { return memory.IsTombstoned(dir, id) }
func promoteRecord(dir, id string) error        { return memory.Promote(dir, id) }
func rejectRecord(dir, id, reason string) error { return memory.Reject(dir, id, reason) }
func deleteRecord(dir, id string) error         { return memory.Delete(dir, id) }

func shortDate(ts string) string { return memory.ShortDate(ts) }
func splitCSV(s string) []string { return memory.SplitCSV(s) }
