//go:build windows

package server

// freeBytes is not implemented on Windows; the guard is skipped there.
func freeBytes(dir string) (int64, bool) { return 0, false }
