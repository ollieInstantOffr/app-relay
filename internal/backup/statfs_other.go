//go:build !unix

package backup

func freeBytes(dir string) (int64, bool) { return 0, false }
