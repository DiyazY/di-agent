package gossip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPersistentStoreRestoresVerifiedObjects(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPersistent(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := objectBytes(t, `{"capability":"compute"}`)
	hash := Hash(data)
	if added, err := store.Put(hash, data); err != nil || !added {
		t.Fatalf("put: added=%v err=%v", added, err)
	}
	if added, err := store.Put(hash, data); err != nil || added {
		t.Fatalf("duplicate: added=%v err=%v", added, err)
	}
	if err := os.WriteFile(filepath.Join(dir, tempPrefix+"interrupted"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPersistent(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := restarted.Get(hash)
	if !ok || !bytes.Equal(got, data) || len(restarted.Inventory().Hashes) != 1 {
		t.Fatal("restart did not restore the original object")
	}
}

func TestPersistentStoreRejectsCorruptAndExcessObjects(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		dir := t.TempDir()
		data := objectBytes(t, `true`)
		if err := os.WriteFile(filepath.Join(dir, Hash(data)), []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewPersistent(dir); !errors.Is(err, ErrHashMismatch) {
			t.Fatalf("corruption accepted: %v", err)
		}
	})
	t.Run("unexpected entry", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "unrelated"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewPersistent(dir); !errors.Is(err, ErrPersistence) {
			t.Fatalf("unexpected entry accepted: %v", err)
		}
	})
	t.Run("too many objects", func(t *testing.T) {
		dir := t.TempDir()
		for i := range MaxObjects + 1 {
			data := objectBytes(t, fmt.Sprintf("%d", i))
			if err := os.WriteFile(filepath.Join(dir, Hash(data)), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := NewPersistent(dir); !errors.Is(err, ErrFull) {
			t.Fatalf("oversized store accepted: %v", err)
		}
	})
}

func TestPersistentStoreDoesNotAdmitFailedWrites(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "objects")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewPersistent(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"-offline"); err != nil {
		t.Fatal(err)
	}
	data := objectBytes(t, `true`)
	hash := Hash(data)
	if added, err := store.Put(hash, data); added || !errors.Is(err, ErrPersistence) {
		t.Fatalf("failed write acknowledged: added=%v err=%v", added, err)
	}
	if store.Has(hash) || len(store.Inventory().Hashes) != 0 {
		t.Fatal("failed write was admitted")
	}
	if err := os.Rename(dir+"-offline", dir); err != nil {
		t.Fatal(err)
	}
	if added, err := store.Put(hash, data); !added || err != nil {
		t.Fatalf("retry: added=%v err=%v", added, err)
	}
}

func TestExchangePersistsReceivedObjects(t *testing.T) {
	remote := NewMemory()
	data := objectBytes(t, `"remote"`)
	hash := Hash(data)
	if _, err := remote.Put(hash, data); err != nil {
		t.Fatal(err)
	}
	server := objectServer(t, remote)
	dir := t.TempDir()
	store, err := NewPersistent(dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExchange(store, NewClient()).Round(context.Background(), []string{server.URL})
	if err != nil || result.Added != 1 {
		t.Fatalf("exchange: %+v err=%v", result, err)
	}
	restarted, err := NewPersistent(dir)
	if err != nil || !restarted.Has(hash) {
		t.Fatalf("received object not restored: err=%v", err)
	}
}
