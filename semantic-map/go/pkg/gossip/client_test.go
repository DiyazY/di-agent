package gossip

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
