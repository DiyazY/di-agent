package gossip

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientValidatesBoundedInventories(t *testing.T) {
	hash := Hash(objectBytes(t, `true`))
	for name, body := range map[string][]byte{
		"missing hashes":     []byte(`{"bytes":0}`),
		"null hashes":        []byte(`{"hashes":null,"bytes":0}`),
		"invalid hash":       []byte(`{"hashes":["not-a-hash"],"bytes":10}`),
		"duplicate hashes":   []byte(`{"hashes":["` + hash + `","` + hash + `"],"bytes":10}`),
		"invalid bytes":      []byte(`{"hashes":[],"bytes":-1}`),
		"inconsistent bytes": []byte(`{"hashes":["` + hash + `"],"bytes":0}`),
		"trailing JSON":      []byte(`{"hashes":[],"bytes":0}{}`),
		"oversized response": bytes.Repeat([]byte(" "), MaxInventoryBytes+1),
		"too many hashes":    mustJSON(t, Inventory{Hashes: make([]string, MaxObjects+1), Bytes: 10}),
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			if _, err := NewClient().Inventory(context.Background(), srv.URL); !errors.Is(err, ErrInvalidInventory) {
				t.Fatalf("err=%v, want invalid inventory", err)
			}
		})
	}
	store := NewMemory()
	data := objectBytes(t, `true`)
	if _, err := store.Put(Hash(data), data); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gossip/objects" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(store.Inventory())
	}))
	defer srv.Close()
	got, err := NewClient().Inventory(context.Background(), srv.URL)
	if err != nil || len(got.Hashes) != 1 || got.Hashes[0] != hash || got.Bytes != len(data) {
		t.Fatalf("valid inventory: %+v err=%v", got, err)
	}
}

func TestClientPreservesWireBytesAndRejectsBadTransfers(t *testing.T) {
	valid := objectBytes(t, `{"capability":"compute"}`)
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		want   error
	}{
		{"exact bytes", valid, 200, nil},
		{"tampered", append(bytes.Clone(valid), '\n'), 200, ErrHashMismatch},
		{"oversized", bytes.Repeat([]byte("x"), MaxObjectBytes+1), 200, ErrTooLarge},
		{"missing", nil, 404, nil},
		{"redirect", valid, 302, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				if tc.status == 302 {
					w.Header().Set("Location", "/other")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write(tc.body)
			}))
			defer srv.Close()
			got, err := NewClient().Fetch(context.Background(), srv.URL, Hash(valid))
			if tc.status != 200 {
				if err == nil || hits != 1 {
					t.Fatalf("status %d: err=%v requests=%d", tc.status, err, hits)
				}
			} else if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
			} else if err != nil || !bytes.Equal(got, valid) {
				t.Fatalf("wire bytes changed: err=%v", err)
			}
		})
	}
}
