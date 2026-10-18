//go:build linux || darwin

package facts

import "syscall"

// diskTotal is the size of the filesystem holding path, in bytes.
func diskTotal(path string) uint64 {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0
	}
	return uint64(s.Blocks) * uint64(s.Bsize)
}
