//go:build windows

package logs

import "os"

func fileInode(os.FileInfo) uint64 { return 0 }
