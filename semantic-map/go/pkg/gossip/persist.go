package gossip

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const tempPrefix = ".gossip-tmp-"

// NewPersistent loads a dedicated existing directory into the bounded store.
// The directory must not be shared by multiple running agents.
func NewPersistent(dir string) (*Memory, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: storage directory is empty", ErrPersistence)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrPersistence, dir, err)
	}
	store := NewMemory()
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), tempPrefix) {
			continue
		}
		if err := ValidateHash(entry.Name()); err != nil {
			return nil, fmt.Errorf("%w: unexpected entry %q in %s", ErrPersistence, entry.Name(), dir)
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("%w: inspecting %s: %w", ErrPersistence, entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: %s is not a regular file", ErrPersistence, entry.Name())
		}
		if info.Size() > MaxObjectBytes {
			return nil, fmt.Errorf("%w: %s: %w", ErrPersistence, entry.Name(), ErrTooLarge)
		}
		file, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("%w: opening %s: %w", ErrPersistence, entry.Name(), err)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, MaxObjectBytes+1))
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, fmt.Errorf("%w: reading %s: %w", ErrPersistence, entry.Name(), err)
		}
		if _, err := store.Put(entry.Name(), data); err != nil {
			return nil, fmt.Errorf("%w: loading %s: %w", ErrPersistence, entry.Name(), err)
		}
	}
	if err := syncDirectory(dir); err != nil {
		return nil, fmt.Errorf("%w: syncing %s at startup: %w", ErrPersistence, dir, err)
	}
	store.dir = dir
	return store, nil
}

func persistObject(dir, hash string, data []byte) (err error) {
	file, err := os.CreateTemp(dir, tempPrefix)
	if err != nil {
		return fmt.Errorf("creating temporary object: %w", err)
	}
	temp := file.Name()
	defer func() {
		if removeErr := os.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("removing temporary object: %w", removeErr))
		}
	}()
	if _, err := file.Write(data); err != nil {
		return errors.Join(fmt.Errorf("writing object: %w", err), file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("syncing object: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing object: %w", err)
	}
	if err := os.Rename(temp, filepath.Join(dir, hash)); err != nil {
		return fmt.Errorf("installing object: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("syncing object directory after rename: %w", err)
	}
	return nil
}

func syncDirectory(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
