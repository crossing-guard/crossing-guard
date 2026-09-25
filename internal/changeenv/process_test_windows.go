//go:build windows

package changeenv

func processAlive(_ int) error { return nil }
func processGone(_ error) bool { return false }
