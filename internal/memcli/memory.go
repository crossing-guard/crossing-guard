package memcli

// memory.go — the remaining thin aliases over crossing-guard/memory. The
// first-class-records conversion (plan §3.3) removed the store-semantics
// shims: writes go through crossing-guard/store's write owner, and only the
// file-format helpers the mirror/migration/import still use remain. Historical
// names are kept where they are a call-site contract.

import (
	"crossing-guard/memory"
)

type Record = memory.Record

var categories = memory.Categories

func memoryDir() string                      { return memory.DefaultDir() }
func readRecord(path string) (Record, error) { return memory.Read(path) }
func writeRecord(dir string, r Record) error { return memory.Write(dir, r) }
func shortDate(ts string) string             { return memory.ShortDate(ts) }
