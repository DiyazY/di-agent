//go:build unix

package actuators

import "golang.org/x/sys/unix"

// writable is nil when this process may write in dir: on a read-only mount the
// answer is EROFS whatever the modes say.
func writable(dir string) error { return unix.Access(dir, unix.W_OK) }
