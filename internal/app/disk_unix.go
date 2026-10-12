//go:build unix

package app

import "syscall"

// diskSpace reports free and total bytes on the filesystem holding path. The
// free figure is what is available to an unprivileged process, not the reserved
// total, because that is the number that runs out first.
func diskSpace(path string) (free, total int64) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0
	}
	return int64(fs.Bavail) * int64(fs.Bsize), int64(fs.Blocks) * int64(fs.Bsize)
}
