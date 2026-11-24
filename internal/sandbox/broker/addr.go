package broker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// ErrAddress is a destination whose every address was refused.
var ErrAddress = errors.New("no allowed address for this host")

// Ranges refused besides what netip's predicates cover. Stricter than
// core.EgressPolicy, which keeps the LAN reachable for webhooks: a sandbox
// reaches only the public internet and the addresses its rules pin.
var refusedPrefixes = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",
		"100.64.0.0/10",
		"192.0.0.0/24",
		"192.0.2.0/24",
		"198.18.0.0/15",
		"198.51.100.0/24",
		"203.0.113.0/24",
		"240.0.0.0/4",
		"2001:db8::/32",
		"fec0::/10",
		"100::/64",
	} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return ps
}()

// IPv6 forms that carry an IPv4 address a gateway may forward to.
var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour  = netip.MustParsePrefix("2002::/16")
	teredo     = netip.MustParsePrefix("2001::/32")
)

// publicAddr says whether an address is on the public internet.
func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range refusedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	if ip.Is6() {
		b := ip.As16()
		switch {
		case nat64.Contains(ip):
			return publicAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
		case nat64Local.Contains(ip):
			return publicAddr(netip.AddrFrom4([4]byte{b[6], b[7], b[9], b[10]}))
		case sixToFour.Contains(ip):
			return publicAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
		case teredo.Contains(ip):
			return publicAddr(netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]}))
		}
	}
	return true
}

// addrAllowed says whether rule's host may be reached at ip: an address
// the rule pins, or a public address that isn't this machine's.
func (b *Broker) addrAllowed(r *Rule, ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range r.Pin {
		if a, err := netip.ParseAddr(p); err == nil && a.Unmap() == ip {
			return true
		}
	}
	if b.local[ip] {
		return false
	}
	if b.testAddr != nil {
		return b.testAddr(ip)
	}
	return publicAddr(ip)
}

// dial connects to rule's host, trying each resolved address the rule
// allows. The address is checked after DNS, so a name that rebinds to a
// refused address is still refused.
func (b *Broker) dial(ctx context.Context, r *Rule) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	addrs, err := b.resolve(ctx, r.Host)
	if err != nil {
		return nil, err
	}
	last := ErrAddress
	d := net.Dialer{}
	for _, a := range addrs {
		if !b.addrAllowed(r, a) {
			continue
		}
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(a.Unmap().String(), strconv.Itoa(r.port())))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, fmt.Errorf("%s: %w", r.Host, last)
}

func defaultResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// localAddrs are this machine's own addresses, refused even when public.
func localAddrs() map[netip.Addr]bool {
	m := map[netip.Addr]bool{}
	ifaddrs, err := net.InterfaceAddrs()
	if err != nil {
		return m
	}
	for _, a := range ifaddrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			m[p.Addr().Unmap()] = true
		}
	}
	return m
}
