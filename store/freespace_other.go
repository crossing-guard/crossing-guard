//go:build !unix

package store

// platformVolumeFreeBytes cannot answer here, so no room check is made.
func platformVolumeFreeBytes(string) (int64, bool) { return 0, false }
