//go:build !linux && !darwin

package facts

func diskTotal(string) uint64 { return 0 }
