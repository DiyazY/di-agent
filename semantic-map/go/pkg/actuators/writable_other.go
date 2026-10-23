//go:build !unix

package actuators

// writable cannot be asked where there is no cgroup tree to write: memory.high
// is Linux's, and its Set says what fails.
func writable(string) error { return nil }
