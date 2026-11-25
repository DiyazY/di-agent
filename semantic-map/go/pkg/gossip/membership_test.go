package gossip

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
)

type recordingTransport struct {
	memberlist.Transport
	writes atomic.Int32
	dials  atomic.Int32
}

func (t *recordingTransport) WriteTo([]byte, string) (time.Time, error) {
	t.writes.Add(1)
	return time.Now(), nil
}

func (t *recordingTransport) DialTimeout(string, time.Duration) (net.Conn, error) {
	t.dials.Add(1)
	return nil, nil
}

func TestMembershipTransportChecksActualDestinations(t *testing.T) {
	_, network, err := net.ParseCIDR("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordingTransport{}
	transport := &scopedTransport{Transport: recorder, cidrs: []net.IPNet{*network}}
	for _, address := range []string{"192.0.2.1:7946", "localhost:7946", "127.0.0.1:0"} {
		if _, err := transport.WriteTo([]byte("x"), address); err == nil {
			t.Fatalf("UDP destination %s accepted", address)
		}
		if _, err := transport.DialTimeout(address, time.Second); err == nil {
			t.Fatalf("TCP destination %s accepted", address)
		}
	}
	if recorder.writes.Load() != 0 || recorder.dials.Load() != 0 {
		t.Fatal("blocked destination reached the network transport")
	}
	if _, err := transport.WriteTo([]byte("x"), "127.0.0.1:7946"); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.DialTimeout("127.0.0.1:7946", time.Second); err != nil {
		t.Fatal(err)
	}
	if recorder.writes.Load() != 1 || recorder.dials.Load() != 1 {
		t.Fatal("allowed destination was not forwarded")
	}
}

func membershipPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestMembershipConfigEnforcesScope(t *testing.T) {
	valid := MembershipConfig{Enabled: true, Site: "lab-a", BindAddr: "127.0.0.1",
		BindPort: 7946, Seeds: []string{"127.0.0.1:7947"},
		AllowedCIDRs: []string{"127.0.0.0/8"}, MaxActivePeers: 4}
	if err := valid.Validate("127.0.0.1:8080"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*MembershipConfig){
		"missing site":    func(c *MembershipConfig) { c.Site = "" },
		"wildcard bind":   func(c *MembershipConfig) { c.BindAddr = "0.0.0.0" },
		"no cidrs":        func(c *MembershipConfig) { c.AllowedCIDRs = nil },
		"outside bind":    func(c *MembershipConfig) { c.AllowedCIDRs = []string{"10.0.0.0/8"} },
		"outside seed":    func(c *MembershipConfig) { c.Seeds = []string{"192.0.2.1:7946"} },
		"hostname seed":   func(c *MembershipConfig) { c.Seeds = []string{"localhost:7946"} },
		"zero active":     func(c *MembershipConfig) { c.MaxActivePeers = 0 },
		"excessive peers": func(c *MembershipConfig) { c.MaxActivePeers = 33 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if err := cfg.Validate("127.0.0.1:8080"); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	if err := valid.Validate("127.0.0.2:8080"); err == nil {
		t.Fatal("HTTP and membership bind addresses may not disagree")
	}
}

func TestMembershipDiscoversOnlySameSiteAndBoundsActivePeers(t *testing.T) {
	var requests atomic.Int32
	var endpoints []string
	var servers []*httptest.Server
	for range 3 {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Di-Gossip-Site") != "lab" {
				http.Error(w, "wrong site", http.StatusForbidden)
				return
			}

			requests.Add(1)
			_ = json.NewEncoder(w).Encode(Inventory{Hashes: []string{}, Bytes: 0})
		}))
		servers = append(servers, server)
		endpoints = append(endpoints, server.URL)
		t.Cleanup(server.Close)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var memberships []*Membership
	var done []<-chan struct{}
	t.Cleanup(func() {
		cancel()
		for _, ch := range done {
			<-ch
		}
		for _, member := range memberships {
			if err := member.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	var seed string
	for i := range 3 {
		config := MembershipConfig{Enabled: true, Site: "lab", BindAddr: "127.0.0.1",
			BindPort: membershipPort(t), AllowedCIDRs: []string{"127.0.0.0/8"}, MaxActivePeers: 1}
		if i > 0 {
			config.Seeds = []string{seed}
		} else {
			seed = net.JoinHostPort(config.BindAddr, fmt.Sprint(config.BindPort))
		}
		member, err := NewMembership(config, fmt.Sprintf("node-%d", i), servers[i].Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		memberships = append(memberships, member)
		done = append(done, member.Run(ctx))
	}
	wait := time.NewTimer(10 * time.Second)
	defer wait.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		memberships[0].delegate.mu.RLock()
		count := len(memberships[0].delegate.candidates)
		memberships[0].delegate.mu.RUnlock()
		if count == 2 {
			break
		}
		select {
		case <-wait.C:
			t.Fatalf("seed discovered %d candidates, want 2", count)
		case <-tick.C:
		}
	}
	first, _ := memberships[0].SelectActive()
	second, _ := memberships[0].SelectActive()
	if len(first) != 1 || len(second) != 1 || first[0] == second[0] ||
		!slices.Contains(endpoints[1:], first[0]) || !slices.Contains(endpoints[1:], second[0]) {
		t.Fatalf("active selection did not rotate within its limit: %v %v", first, second)
	}
	client := NewMembershipClient(memberships[0])
	transport := client.http.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("membership client must not use a proxy")
	}
	for _, address := range []string{"localhost:80", "192.0.2.1:80"} {
		if conn, err := transport.DialContext(ctx, "tcp", address); err == nil {
			conn.Close()
			t.Fatalf("membership HTTP dial accepted %s", address)
		}
	}
	if _, err := client.Inventory(ctx, second[0]); err != nil || requests.Load() != 1 {
		t.Fatalf("active peer request failed: %v", err)
	}
	if _, err := client.Inventory(ctx, first[0]); err == nil || requests.Load() != 1 {
		t.Fatalf("inactive peer was contacted: %v", err)
	}
	if memberships[0].AllowInbound("192.0.2.1:1234", "lab") ||
		memberships[0].AllowInbound("127.0.0.1:1234", "other") ||
		!memberships[0].AllowInbound("127.0.0.1:1234", "lab") {
		t.Fatal("inbound site or CIDR scope not enforced")
	}

	other := MembershipConfig{Enabled: true, Site: "other", BindAddr: "127.0.0.1",
		BindPort: membershipPort(t), AllowedCIDRs: []string{"127.0.0.0/8"}, MaxActivePeers: 1}
	foreign, err := NewMembership(other, "foreign", servers[0].Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	memberships = append(memberships, foreign)
	if _, err := foreign.list.Join([]string{seed}); err == nil {
		t.Fatal("different site joined membership")
	}
}

func TestMembershipRetriesSeedAndBroadcastsLeave(t *testing.T) {
	seedPort := membershipPort(t)
	seed := net.JoinHostPort("127.0.0.1", fmt.Sprint(seedPort))
	config := MembershipConfig{Enabled: true, Site: "lab", BindAddr: "127.0.0.1",
		BindPort: membershipPort(t), Seeds: []string{seed},
		AllowedCIDRs: []string{"127.0.0.0/8"}, MaxActivePeers: 1}
	client, err := NewMembership(config, "joining", "127.0.0.1:8081")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := client.Run(ctx)
	closed := false
	t.Cleanup(func() {
		cancel()
		<-done
		if !closed {
			if err := client.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	// Give the initial join time to fail before the seed starts.
	time.Sleep(200 * time.Millisecond)
	config.BindPort, config.Seeds = seedPort, nil
	server, err := NewMembership(config, "seed", "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	waitFor := func(condition func() bool, failure string) {
		t.Helper()
		timeout := time.NewTimer(13 * time.Second)
		defer timeout.Stop()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for !condition() {
			select {
			case <-timeout.C:
				t.Fatal(failure)
			case <-ticker.C:
			}
		}
	}
	waitFor(func() bool {
		active, _ := server.SelectActive()
		return len(active) == 1
	}, "seed retry did not discover the newly available node")
	cancel()
	<-done
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	waitFor(func() bool { return len(server.Active()) == 0 }, "graceful leave did not remove the active member")
}
