package gossip

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func objectBytes(t *testing.T, payload string) []byte {
	t.Helper()
	data, err := Encode("node-a", "announcement", json.RawMessage(payload))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMemoryOwnsBytesAndDeduplicatesConcurrentDelivery(t *testing.T) {
	store := NewMemory()
	data := objectBytes(t, `{"capability":"compute"}`)
	want := bytes.Clone(data)
	hash := Hash(data)
	var wg sync.WaitGroup
	added := make(chan bool, 20)
	for range 20 {
		wg.Go(func() {
			newObject, err := store.Put(hash, data)
			if err != nil {
				t.Error(err)
			}
			added <- newObject
		})
	}
	wg.Wait()
	close(added)
	count := 0
	for newObject := range added {
		if newObject {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("admitted %d copies, want one", count)
	}
	data[0] = '!'
	got, ok := store.Get(hash)
	if !ok || !bytes.Equal(got, want) {
		t.Fatal("caller changed stored bytes")
	}
	got[0] = '!'
	got, _ = store.Get(hash)
	if !bytes.Equal(got, want) {
		t.Fatal("reader changed stored bytes")
	}
	if inventory := store.Inventory(); len(inventory.Hashes) != 1 || inventory.Bytes != len(want) {
		t.Fatalf("duplicate deliveries consumed capacity: %+v", inventory)
	}
}

func TestMemoryRejectsInvalidContentWithoutAdmission(t *testing.T) {
	valid := objectBytes(t, `{"ok":true}`)
	for _, tc := range []struct {
		name string
		data []byte
		hash string
		want error
	}{
		{"bad hash", valid, "../object", ErrInvalidHash},
		{"tampered", append(bytes.Clone(valid), ' '), Hash(valid), ErrHashMismatch},
		{"missing origin", []byte(`{"schema_version":1,"payload_type":"x","payload":true}`), "", ErrInvalidObject},
		{"unknown version", []byte(`{"schema_version":2,"origin_id":"a","payload_type":"x","payload":true}`), "", ErrInvalidObject},
		{"unknown field", []byte(`{"schema_version":1,"origin_id":"a","payload_type":"x","payload":true,"extra":1}`), "", ErrInvalidObject},
		{"null payload", []byte(`{"schema_version":1,"origin_id":"a","payload_type":"x","payload":null}`), "", ErrInvalidObject},
		{"trailing JSON", append(bytes.Clone(valid), []byte(`{}`)...), "", ErrInvalidObject},
		{"too large", bytes.Repeat([]byte("x"), MaxObjectBytes+1), "", ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemory()
			hash := tc.hash
			if hash == "" {
				hash = Hash(tc.data)
			}
			if _, err := store.Put(hash, tc.data); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got := store.Inventory(); len(got.Hashes) != 0 || got.Bytes != 0 {
				t.Fatalf("invalid object admitted: %+v", got)
			}
		})
	}
}

func TestMemoryEnforcesExactObjectAndTotalByteLimits(t *testing.T) {
	store := NewMemory()
	empty := objectBytes(t, `""`)
	payload := `"` + strings.Repeat("x", MaxObjectBytes-len(empty)) + `"`
	exact := objectBytes(t, payload)
	if len(exact) != MaxObjectBytes {
		t.Fatalf("fixture size = %d", len(exact))
	}
	for i := range MaxStoreBytes / MaxObjectBytes {
		data := bytes.Clone(exact)
		// Change two payload bytes so every object remains the same size but distinct.
		start := bytes.Index(data, []byte(`"payload":"`)) + len(`"payload":"`)
		copy(data[start:], fmt.Sprintf("%02d", i))
		if _, err := store.Put(Hash(data), data); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.Inventory().Bytes; got != MaxStoreBytes {
		t.Fatalf("stored %d bytes, want %d", got, MaxStoreBytes)
	}
	small := objectBytes(t, `true`)
	if _, err := store.Put(Hash(small), small); !errors.Is(err, ErrFull) {
		t.Fatalf("over byte limit: %v", err)
	}
	hash := store.Inventory().Hashes[0]
	data, _ := store.Get(hash)
	if added, err := store.Put(hash, data); err != nil || added {
		t.Fatalf("duplicate refused at capacity: added=%v err=%v", added, err)
	}
	if _, err := Encode("node-a", "announcement", json.RawMessage(`"`+strings.Repeat("x", MaxObjectBytes-len(empty)+1)+`"`)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over object limit: %v", err)
	}
}

func TestMemoryEnforcesObjectCountWithoutEviction(t *testing.T) {
	store := NewMemory()
	for i := range MaxObjects {
		data := objectBytes(t, fmt.Sprintf("%d", i))
		if _, err := store.Put(Hash(data), data); err != nil {
			t.Fatal(err)
		}
	}
	before := store.Inventory()
	data := objectBytes(t, `"extra"`)
	if _, err := store.Put(Hash(data), data); !errors.Is(err, ErrFull) {
		t.Fatalf("over object limit: %v", err)
	}
	if after := store.Inventory(); !bytes.Equal(mustJSON(t, before), mustJSON(t, after)) {
		t.Fatal("full store changed or evicted content")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
