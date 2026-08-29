package webclient

import (
	"context"
	"net"
	"net/url"
	"strings"
	"syscall"

	"github.com/boop-dev/boop/internal/config"
)

// LookupIPFunc resolves a hostname to IP addresses. It is an injection point
// for tests; production uses net.DefaultResolver.
type LookupIPFunc func(ctx context.Context, host string) ([]net.IP, error)

// metadataIPs are cloud instance-metadata endpoints. They hand out credentials
// to anything that can reach them, and no legitimate fetch or search needs
// them, so they stay blocked even when AllowPrivateNetworks is on. This is
// deliberately stricter than "link-local is allowed once you opt in", and
// matches the same decision made by the http tool.
var metadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"), // AWS, Azure, GCP, OpenStack, DigitalOcean
	net.ParseIP("169.254.170.2"),   // AWS ECS task metadata
	net.ParseIP("100.100.100.200"), // Alibaba Cloud
	net.ParseIP("192.0.0.192"),     // Oracle Cloud
	net.ParseIP("fd00:ec2::254"),   // AWS IMDS over IPv6
}

// extraBlockedNets are ranges that are never a sensible fetch target even
// though Go's net.IP predicates do not classify them as private.
var extraBlockedNets = []*net.IPNet{
	mustCIDR("100.64.0.0/10"),   // RFC 6598 carrier-grade NAT
	mustCIDR("192.0.0.0/24"),    // RFC 6890 IETF protocol assignments
	mustCIDR("198.18.0.0/15"),   // RFC 2544 benchmarking
	mustCIDR("192.0.2.0/24"),    // TEST-NET-1
	mustCIDR("198.51.100.0/24"), // TEST-NET-2
	mustCIDR("203.0.113.0/24"),  // TEST-NET-3
	mustCIDR("240.0.0.0/4"),     // reserved
	mustCIDR("::/128"),          // unspecified
	mustCIDR("2001:db8::/32"),   // documentation
	mustCIDR("64:ff9b::/96"),    // NAT64: an embedded private v4 would bypass checks
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("webclient: bad CIDR " + s)
	}
	return n
}

// Guard decides whether an outbound request may proceed. It is the security
// boundary of this package: URLs arrive from a model, and a model is an
// untrusted source of URLs.
//
// # What it enforces
//
//   - Scheme allowlist: http and https only.
//   - Domain policy: BlockedDomains always wins; a non-empty AllowedDomains
//     denies everything not listed.
//   - Address policy: loopback, link-local, private, unspecified, multicast and
//     several reserved ranges are refused unless AllowPrivateNetworks is set.
//     Cloud metadata addresses are refused unconditionally.
//
// # Where the defence is incomplete
//
// CheckURL resolves the hostname and inspects the answers, so a public name
// pointing at 127.0.0.1 is caught. CheckAddr runs again from the dialer's
// Control hook on the address actually being connected to, which closes most
// of the gap between resolution and connection, and CheckURL runs again on
// every redirect hop. What remains theoretically possible is a DNS-rebinding
// race in which a resolver answer changes between the Control-hook check and
// the kernel's use of that address; fully closing it needs connection-level
// pinning of the checked IP, which Go's http.Transport does not expose. The
// residual window is small, but it is real and worth stating honestly.
type Guard struct {
	cfg    config.NetworkConfig
	lookup LookupIPFunc
}

// NewGuard returns a Guard that resolves names with the default resolver.
func NewGuard(cfg config.NetworkConfig) *Guard {
	return NewGuardWithLookup(cfg, nil)
}

// NewGuardWithLookup returns a Guard using a caller-supplied resolver. Passing
// nil selects net.DefaultResolver.
func NewGuardWithLookup(cfg config.NetworkConfig, lookup LookupIPFunc) *Guard {
	if lookup == nil {
		lookup = defaultLookupIP
	}
	return &Guard{cfg: cfg, lookup: lookup}
}

// defaultLookupIP resolves through the system resolver.
func defaultLookupIP(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// CheckRawURL parses rawURL and applies CheckURL to it.
func (g *Guard) CheckRawURL(ctx context.Context, rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, wrapError(KindMalformed, "guard", rawURL, err, "invalid URL")
	}
	if err := g.CheckURL(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// CheckURL applies the scheme, domain and address policy to u. It performs DNS
// resolution, so it takes a context and can block.
func (g *Guard) CheckURL(ctx context.Context, u *url.URL) error {
	if u == nil {
		return newError(KindMalformed, "guard", "", "no URL")
	}
	target := u.Redacted()
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "":
		return newError(KindMalformed, "guard", target, "URL has no scheme; use an absolute http:// or https:// URL")
	default:
		return newError(KindBlocked, "guard", target,
			"scheme %q is not allowed; only http and https are fetched", u.Scheme)
	}
	if u.User != nil {
		// http://trusted.example@evil.example/ reads as trusted to a human
		// and resolves to evil.example. Refuse rather than disambiguate.
		return newError(KindBlocked, "guard", target, "URLs with embedded credentials are not fetched")
	}
	host := u.Hostname()
	if host == "" {
		return newError(KindMalformed, "guard", target, "URL has no host")
	}
	if err := g.CheckHostPolicy(host); err != nil {
		return err
	}
	return g.checkHostAddresses(ctx, host, target)
}

// CheckHostPolicy applies only the domain allow/block lists to host. It does no
// DNS resolution, which makes it usable for cheap pre-checks.
func (g *Guard) CheckHostPolicy(host string) error {
	h := normalizeHost(host)
	if h == "" {
		return newError(KindMalformed, "guard", host, "empty host")
	}
	for _, pattern := range g.cfg.BlockedDomains {
		if hostMatches(h, pattern) {
			return newError(KindBlocked, "guard", host,
				"host %q is in network.blocked_domains (%q)", h, pattern)
		}
	}
	if len(g.cfg.AllowedDomains) == 0 {
		return nil
	}
	for _, pattern := range g.cfg.AllowedDomains {
		if hostMatches(h, pattern) {
			return nil
		}
	}
	return newError(KindBlocked, "guard", host,
		"host %q is not in network.allowed_domains", h)
}

// checkHostAddresses resolves host (unless it is already a literal) and refuses
// the request if any answer is in a blocked range. Every answer must pass: a
// name that resolves to one public and one private address is still a way to
// reach the private one.
func (g *Guard) checkHostAddresses(ctx context.Context, host, target string) error {
	if ip := net.ParseIP(host); ip != nil {
		return g.checkIP(ip, target)
	}
	ips, err := g.lookup(ctx, host)
	if err != nil {
		return wrapError(KindTransport, "guard", target, err, "cannot resolve %q", host)
	}
	if len(ips) == 0 {
		return newError(KindTransport, "guard", target, "%q resolved to no addresses", host)
	}
	for _, ip := range ips {
		if err := g.checkIP(ip, target); err != nil {
			return err
		}
	}
	return nil
}

// CheckIP reports whether Boop may connect to ip.
func (g *Guard) CheckIP(ip net.IP) error { return g.checkIP(ip, "") }

func (g *Guard) checkIP(ip net.IP, target string) error {
	if ip == nil {
		return newError(KindMalformed, "guard", target, "invalid IP address")
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, m := range metadataIPs {
		if ip.Equal(m) {
			return newError(KindBlocked, "guard", target,
				"%s is a cloud metadata endpoint and is never fetched", ip)
		}
	}
	// Never useful as an HTTP destination, opt-in or not.
	if ip.IsUnspecified() {
		return newError(KindBlocked, "guard", target, "%s is the unspecified address", ip)
	}
	if ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return newError(KindBlocked, "guard", target, "%s is a multicast address", ip)
	}
	if ip.Equal(net.IPv4bcast) {
		return newError(KindBlocked, "guard", target, "%s is the broadcast address", ip)
	}
	if g.cfg.AllowPrivateNetworks {
		return nil
	}
	switch {
	case ip.IsLoopback():
		return blockedAddr(target, ip, "a loopback address")
	case ip.IsLinkLocalUnicast():
		return blockedAddr(target, ip, "a link-local address")
	case ip.IsPrivate():
		return blockedAddr(target, ip, "a private address")
	}
	for _, n := range extraBlockedNets {
		if n.Contains(ip) {
			return blockedAddr(target, ip, "in the reserved range "+n.String())
		}
	}
	return nil
}

// blockedAddr formats an address refusal with the opt-in hint attached, since
// the fix is a config change the user may not know about.
func blockedAddr(target string, ip net.IP, why string) *Error {
	return newError(KindBlocked, "guard", target,
		"%s is %s; set network.allow_private_networks: true to permit local destinations", ip, why)
}

// CheckAddr validates a resolved "host:port" as handed to the dialer. It is
// wired into net.Dialer.Control so the address actually being connected to is
// checked, not merely the one the URL named.
func (g *Guard) CheckAddr(network, address string) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return newError(KindBlocked, "guard", address, "network %q is not allowed", network)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return wrapError(KindMalformed, "guard", address, err, "cannot parse dial address")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return newError(KindMalformed, "guard", address, "dial address %q is not an IP", host)
	}
	return g.checkIP(ip, address)
}

// control is the net.Dialer.Control hook form of CheckAddr.
func (g *Guard) control(network, address string, _ syscall.RawConn) error {
	return g.CheckAddr(network, address)
}

// normalizeHost lowercases a host and strips the root label's trailing dot and
// any surrounding IPv6 brackets, so "Example.COM." and "example.com" compare
// equal.
func normalizeHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(h, ".")
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	return h
}

// hostMatches reports whether host is covered by a domain pattern. A pattern
// matches the domain itself and any subdomain of it, and "*.example.com" and
// ".example.com" are accepted spellings of the same thing.
//
// Matching is label-aware on purpose: "evil-example.com" must not match
// "example.com", which a naive HasSuffix would allow.
func hostMatches(host, pattern string) bool {
	h := normalizeHost(host)
	p := normalizeHost(pattern)
	p = strings.TrimPrefix(p, "*.")
	p = strings.TrimPrefix(p, ".")
	if p == "" || h == "" {
		return false
	}
	if h == p {
		return true
	}
	return strings.HasSuffix(h, "."+p)
}
