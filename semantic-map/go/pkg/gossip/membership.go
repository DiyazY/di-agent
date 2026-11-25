package gossip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
)

// MembershipConfig enables discovery within a labelled site and IP scope.
type MembershipConfig struct {
	Enabled        bool     `yaml:"enabled"`
	Site           string   `yaml:"site"`
	BindAddr       string   `yaml:"bind_addr"`
	BindPort       int      `yaml:"bind_port"`
	Seeds          []string `yaml:"seeds"`
	AllowedCIDRs   []string `yaml:"allowed_cidrs"`
	MaxActivePeers int      `yaml:"max_active_peers"`
}

func (c MembershipConfig) validate() ([]net.IPNet, error) {
	if c.Site == "" || len(c.Site) > 63 {
		return nil, errors.New("membership.site must contain 1-63 letters, digits, '.', '_' or '-'")
	}
	for _, ch := range c.Site {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '.' || ch == '_' || ch == '-') {
			return nil, errors.New("membership.site must contain only letters, digits, '.', '_' or '-'")
		}
	}
	ip := net.ParseIP(c.BindAddr)
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return nil, errors.New("membership.bind_addr must be a specific IP address")
	}
	if c.BindPort < 1 || c.BindPort > 65535 {
		return nil, errors.New("membership.bind_port must be between 1 and 65535")
	}
	if c.MaxActivePeers < 1 || c.MaxActivePeers > 32 {
		return nil, errors.New("membership.max_active_peers must be between 1 and 32")
	}
	if len(c.AllowedCIDRs) == 0 {
		return nil, errors.New("membership.allowed_cidrs must not be empty")
	}
	cidrs := make([]net.IPNet, 0, len(c.AllowedCIDRs))
	for _, value := range c.AllowedCIDRs {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, fmt.Errorf("membership.allowed_cidrs: %w", err)
		}
		cidrs = append(cidrs, *network)
	}
	if !inCIDRs(ip, cidrs) {
		return nil, errors.New("membership.bind_addr is outside the allowed CIDRs")
	}
	if len(c.Seeds) > 16 {
		return nil, errors.New("membership.seeds exceeds the limit of 16")
	}
	for _, seed := range c.Seeds {
		if err := allowedAddress(seed, cidrs); err != nil {
			return nil, fmt.Errorf("membership seed %q: %w", seed, err)
		}
	}
	return cidrs, nil
}

// Validate checks discovery configuration before opening network listeners.
func (c MembershipConfig) Validate(httpAddr string) error {
	if !c.Enabled {
		return nil
	}
	if _, err := c.validate(); err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(httpAddr)
	if err != nil {
		return fmt.Errorf("node.addr for membership: %w", err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("node.addr must specify a port between 1 and 65535 for membership")
	}
	if host != "" {
		ip := net.ParseIP(host)
		if ip == nil || (!ip.IsUnspecified() && !ip.Equal(net.ParseIP(c.BindAddr))) {
			return errors.New("node.addr must listen on membership.bind_addr or a wildcard address")
		}
	}
	return nil
}

func inCIDRs(ip net.IP, cidrs []net.IPNet) bool {
	for _, cidr := range cidrs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func allowedAddress(address string, cidrs []net.IPNet) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("expected literal IP:port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || !inCIDRs(ip, cidrs) {
		return fmt.Errorf("IP %q is not in the allowed CIDRs", host)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

type memberMeta struct {
	Site string `json:"site"`
	Port int    `json:"port"`
}

type memberDelegate struct {
	site       string
	meta       []byte
	cidrs      []net.IPNet
	nodeID     string
	mu         sync.RWMutex
	candidates map[string]string
}

func (d *memberDelegate) NodeMeta(limit int) []byte {
	if len(d.meta) > limit {
		return nil
	}
	return slices.Clone(d.meta)
}
func (*memberDelegate) NotifyMsg([]byte)                {}
func (*memberDelegate) GetBroadcasts(int, int) [][]byte { return nil }
func (*memberDelegate) LocalState(bool) []byte          { return nil }
func (*memberDelegate) MergeRemoteState([]byte, bool)   {}
func (d *memberDelegate) NotifyAlive(node *memberlist.Node) error {
	if !inCIDRs(node.Addr, d.cidrs) {
		return errors.New("member address is outside the allowed CIDRs")
	}
	var meta memberMeta
	decoder := json.NewDecoder(strings.NewReader(string(node.Meta)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&meta); err != nil {
		return fmt.Errorf("invalid member metadata: %w", err)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return errors.New("invalid trailing member metadata")
	}
	if meta.Site != d.site || meta.Port < 1 || meta.Port > 65535 {
		return errors.New("member has a different site or invalid HTTP port")
	}
	return nil
}

func (d *memberDelegate) NotifyJoin(node *memberlist.Node) {
	d.NotifyUpdate(node)
}

func (d *memberDelegate) NotifyUpdate(node *memberlist.Node) {
	if node.Name == d.nodeID || d.NotifyAlive(node) != nil {
		return
	}
	var meta memberMeta
	if err := json.Unmarshal(node.Meta, &meta); err != nil {
		log.Printf("gossip membership: invalid metadata for %s: %v", node.Name, err)
		return
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(node.Addr.String(), strconv.Itoa(meta.Port))}
	d.mu.Lock()
	d.candidates[node.Name] = u.String()
	d.mu.Unlock()
}

func (d *memberDelegate) NotifyLeave(node *memberlist.Node) {
	d.mu.Lock()
	delete(d.candidates, node.Name)
	d.mu.Unlock()
}

// AllowInbound checks the immediate HTTP source and its claimed site.
// Neither the address nor the site label authenticates the peer.
func (m *Membership) AllowInbound(remoteAddr, site string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	return err == nil && site == m.delegate.site && inCIDRs(net.ParseIP(host), m.cidrs)
}

// Membership keeps discovered candidates separate from the general peer registry.
type Membership struct {
	list     *memberlist.Memberlist
	delegate *memberDelegate
	cidrs    []net.IPNet
	seeds    []string
	maxPeers int
	activeMu sync.Mutex
	active   []string
	next     int
}

func NewMembership(c MembershipConfig, nodeID, httpAddr string) (*Membership, error) {
	if err := c.Validate(httpAddr); err != nil {
		return nil, err
	}
	if !c.Enabled || strings.TrimSpace(nodeID) == "" {
		return nil, errors.New("membership requires an enabled configuration and node ID")
	}
	cidrs, err := c.validate()
	if err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(httpAddr)
	httpPort, _ := strconv.Atoi(port)
	meta, err := json.Marshal(memberMeta{Site: c.Site, Port: httpPort})
	if err != nil {
		return nil, fmt.Errorf("encoding membership metadata: %w", err)
	}
	delegate := &memberDelegate{site: c.Site, meta: meta, cidrs: cidrs,
		nodeID: nodeID, candidates: make(map[string]string)}
	transport, err := memberlist.NewNetTransport(&memberlist.NetTransportConfig{
		BindAddrs: []string{c.BindAddr}, BindPort: c.BindPort, Logger: log.Default(),
	})
	if err != nil {
		return nil, fmt.Errorf("membership transport: %w", err)
	}
	config := memberlist.DefaultLANConfig()
	config.Name = nodeID
	config.BindAddr, config.AdvertiseAddr = c.BindAddr, c.BindAddr
	config.BindPort, config.AdvertisePort = c.BindPort, c.BindPort
	config.Transport = &scopedTransport{Transport: transport, cidrs: cidrs}
	config.CIDRsAllowed = cidrs
	config.Label = c.Site
	config.Delegate, config.Alive, config.Events = delegate, delegate, delegate
	config.TCPTimeout = 2 * time.Second
	list, err := memberlist.Create(config)
	if err != nil {
		_ = transport.Shutdown()
		return nil, fmt.Errorf("starting membership: %w", err)
	}
	return &Membership{list: list, delegate: delegate, cidrs: cidrs, seeds: slices.Clone(c.Seeds), maxPeers: c.MaxActivePeers}, nil
}

// SelectActive rotates the bounded set and also returns all current candidates.
func (m *Membership) SelectActive() (active, known []string) {
	type candidate struct{ name, url string }
	var candidates []candidate
	m.delegate.mu.RLock()
	for name, address := range m.delegate.candidates {
		candidates = append(candidates, candidate{name, address})
	}
	m.delegate.mu.RUnlock()
	slices.SortFunc(candidates, func(a, b candidate) int { return strings.Compare(a.name, b.name) })
	seen := make(map[string]bool)
	var available []string
	for _, candidate := range candidates {
		if !seen[candidate.url] {
			seen[candidate.url] = true
			available = append(available, candidate.url)
		}
	}
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	m.active = nil
	if len(available) > 0 {
		start := m.next % len(available)
		for i := range min(m.maxPeers, len(available)) {
			m.active = append(m.active, available[(start+i)%len(available)])
		}
		m.next = (start + 1) % len(available)
	}
	return slices.Clone(m.active), available
}

// Active returns the selected set, excluding members that have since left.
func (m *Membership) Active() []string {
	m.delegate.mu.RLock()
	current := make(map[string]bool, len(m.delegate.candidates))
	for _, address := range m.delegate.candidates {
		current[address] = true
	}
	m.delegate.mu.RUnlock()
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	var active []string
	for _, address := range m.active {
		if current[address] {
			active = append(active, address)
		}
	}
	return active
}

// Run retries one seed at a time so temporary outages can recover.
func (m *Membership) Run(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		next := 0
		for len(m.seeds) > 0 && ctx.Err() == nil {
			seed := m.seeds[next%len(m.seeds)]
			next++
			if _, err := m.list.Join([]string{seed}); err != nil && ctx.Err() == nil {
				log.Printf("gossip membership: seed %s: %v", seed, err)
			}
			timer := time.NewTimer(10 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return done
}

func (m *Membership) Close() error {
	return errors.Join(m.list.Leave(time.Second), m.list.Shutdown())
}

type scopedTransport struct {
	memberlist.Transport
	cidrs []net.IPNet
}

func (t *scopedTransport) WriteTo(data []byte, address string) (time.Time, error) {
	if err := allowedAddress(address, t.cidrs); err != nil {
		return time.Time{}, err
	}
	return t.Transport.WriteTo(data, address)
}

func (t *scopedTransport) DialTimeout(address string, timeout time.Duration) (net.Conn, error) {
	if err := allowedAddress(address, t.cidrs); err != nil {
		return nil, err
	}
	return t.Transport.DialTimeout(address, timeout)
}
