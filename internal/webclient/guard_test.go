package webclient

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"

	"github.com/kawaiipantsu/boop/internal/config"
)

// staticLookup returns a resolver that always answers with the given IPs.
func staticLookup(ips ...string) LookupIPFunc {
	return func(context.Context, string) ([]net.IP, error) {
		out := make([]net.IP, 0, len(ips))
		for _, s := range ips {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
}

func guardConfig() config.NetworkConfig {
	cfg := config.DefaultNetwork()
	cfg.Enabled = true
	return cfg
}

func TestGuardBlockedAddressRanges(t *testing.T) {
	tests := []struct {
		name    string
		ip      string
		blocked bool
		// alsoBlockedWhenPrivateAllowed marks addresses that stay refused
		// even after opting in to private networks.
		alsoBlockedWhenPrivateAllowed bool
	}{
		{name: "public v4", ip: "93.184.216.34"},
		{name: "public v6", ip: "2606:2800:220:1:248:1893:25c8:1946"},
		{name: "loopback v4", ip: "127.0.0.1", blocked: true},
		{name: "loopback v4 high", ip: "127.255.255.254", blocked: true},
		{name: "loopback v6", ip: "::1", blocked: true},
		{name: "link local v4", ip: "169.254.10.10", blocked: true},
		{name: "cloud metadata", ip: "169.254.169.254", blocked: true, alsoBlockedWhenPrivateAllowed: true},
		{name: "ecs metadata", ip: "169.254.170.2", blocked: true, alsoBlockedWhenPrivateAllowed: true},
		{name: "aws imds v6", ip: "fd00:ec2::254", blocked: true, alsoBlockedWhenPrivateAllowed: true},
		{name: "alibaba metadata", ip: "100.100.100.200", blocked: true, alsoBlockedWhenPrivateAllowed: true},
		{name: "oracle metadata", ip: "192.0.0.192", blocked: true, alsoBlockedWhenPrivateAllowed: true},
		{name: "link local v6", ip: "fe80::1", blocked: true},
		{name: "private 10/8", ip: "10.1.2.3", blocked: true},
		{name: "private 172.16/12", ip: "172.16.5.5", blocked: true},
		{name: "private 172.31 edge", ip: "172.31.255.255", blocked: true},
		{name: "public 172.32", ip: "172.32.0.1"},
		{name: "private 192.168/16", ip: "192.168.1.1", blocked: true},
		{name: "unique local v6", ip: "fc00::1", blocked: true},
		{name: "unspecified v4", ip: "0.0.0.0", blocked: true, alsoBlockedWhenPrivateAllowed: true},
		{name: "unspecified v6", ip: "::", blocked: true, alsoBlockedWhenPrivateAllowed: true},
		{name: "multicast v4", ip: "224.0.0.1", blocked: true, alsoBlockedWhenPrivateAllowed: true},
		{name: "multicast v6", ip: "ff02::1", blocked: true, alsoBlockedWhenPrivateAllowed: true},
		{name: "broadcast", ip: "255.255.255.255", blocked: true, alsoBlockedWhenPrivateAllowed: true},
		{name: "cgnat", ip: "100.64.0.1", blocked: true},
		{name: "reserved 240/4", ip: "240.0.0.1", blocked: true},
		{name: "ipv4 mapped loopback", ip: "::ffff:127.0.0.1", blocked: true},
		{name: "ipv4 mapped metadata", ip: "::ffff:169.254.169.254", blocked: true, alsoBlockedWhenPrivateAllowed: true},
		{name: "nat64 embedded private", ip: "64:ff9b::a00:1", blocked: true},
	}

	strict := NewGuard(guardConfig())
	permissive := func() *Guard {
		cfg := guardConfig()
		cfg.AllowPrivateNetworks = true
		return NewGuard(cfg)
	}()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tc.ip)
			}
			err := strict.CheckIP(ip)
			if tc.blocked && err == nil {
				t.Fatalf("CheckIP(%s) = nil, want blocked", tc.ip)
			}
			if !tc.blocked && err != nil {
				t.Fatalf("CheckIP(%s) = %v, want allowed", tc.ip, err)
			}
			if tc.blocked && !errors.Is(err, ErrBlocked) {
				t.Fatalf("CheckIP(%s) kind = %q, want blocked", tc.ip, KindOf(err))
			}

			err = permissive.CheckIP(ip)
			if tc.alsoBlockedWhenPrivateAllowed {
				if err == nil {
					t.Fatalf("CheckIP(%s) with AllowPrivateNetworks = nil, want still blocked", tc.ip)
				}
			} else if err != nil {
				t.Fatalf("CheckIP(%s) with AllowPrivateNetworks = %v, want allowed", tc.ip, err)
			}
		})
	}
}

func TestGuardCheckURLScheme(t *testing.T) {
	g := NewGuardWithLookup(guardConfig(), staticLookup("93.184.216.34"))
	tests := []struct {
		name string
		url  string
		kind ErrorKind
	}{
		{"http ok", "http://example.com/x", ""},
		{"https ok", "https://example.com/x", ""},
		{"file rejected", "file:///etc/passwd", KindBlocked},
		{"ftp rejected", "ftp://example.com/x", KindBlocked},
		{"gopher rejected", "gopher://example.com/1", KindBlocked},
		{"data rejected", "data:text/html,<h1>hi</h1>", KindBlocked},
		{"javascript rejected", "javascript:alert(1)", KindBlocked},
		{"relative rejected", "/just/a/path", KindMalformed},
		{"no host rejected", "http://", KindMalformed},
		{"embedded credentials rejected", "http://trusted.example@evil.example/", KindBlocked},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := g.CheckRawURL(context.Background(), tc.url)
			if tc.kind == "" {
				if err != nil {
					t.Fatalf("CheckRawURL(%q) = %v, want nil", tc.url, err)
				}
				return
			}
			if got := KindOf(err); got != tc.kind {
				t.Fatalf("CheckRawURL(%q) kind = %q, want %q (err=%v)", tc.url, got, tc.kind, err)
			}
		})
	}
}

func TestGuardResolvesHostname(t *testing.T) {
	tests := []struct {
		name    string
		ips     []string
		blocked bool
	}{
		{"public", []string{"93.184.216.34"}, false},
		{"resolves to loopback", []string{"127.0.0.1"}, true},
		{"resolves to metadata", []string{"169.254.169.254"}, true},
		{"one public one private", []string{"93.184.216.34", "10.0.0.5"}, true},
		{"private first", []string{"192.168.0.1", "93.184.216.34"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGuardWithLookup(guardConfig(), staticLookup(tc.ips...))
			_, err := g.CheckRawURL(context.Background(), "http://foo.example/page")
			if tc.blocked != (err != nil) {
				t.Fatalf("CheckRawURL with %v = %v, blocked want %v", tc.ips, err, tc.blocked)
			}
			if tc.blocked && !errors.Is(err, ErrBlocked) {
				t.Fatalf("kind = %q, want blocked", KindOf(err))
			}
		})
	}
}

func TestGuardResolutionFailure(t *testing.T) {
	g := NewGuardWithLookup(guardConfig(), func(context.Context, string) ([]net.IP, error) {
		return nil, errors.New("no such host")
	})
	_, err := g.CheckRawURL(context.Background(), "http://missing.example/")
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("kind = %q, want transport (err=%v)", KindOf(err), err)
	}

	empty := NewGuardWithLookup(guardConfig(), staticLookup())
	if _, err := empty.CheckRawURL(context.Background(), "http://missing.example/"); err == nil {
		t.Fatal("a host resolving to no addresses must be refused")
	}
}

func TestGuardDomainPolicy(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		blocked []string
		host    string
		want    bool // true = permitted
	}{
		{"no lists permits all", nil, nil, "example.com", true},
		{"allowlist permits listed", []string{"example.com"}, nil, "example.com", true},
		{"allowlist permits subdomain", []string{"example.com"}, nil, "docs.example.com", true},
		{"allowlist permits deep subdomain", []string{"example.com"}, nil, "a.b.example.com", true},
		{"allowlist denies others", []string{"example.com"}, nil, "other.com", false},
		{"allowlist not fooled by suffix", []string{"example.com"}, nil, "evil-example.com", false},
		{"allowlist not fooled by prefix", []string{"example.com"}, nil, "example.com.evil.net", false},
		{"blocklist denies listed", nil, []string{"tracker.net"}, "tracker.net", false},
		{"blocklist denies subdomain", nil, []string{"tracker.net"}, "ads.tracker.net", false},
		{"blocklist not fooled by suffix", nil, []string{"tracker.net"}, "nottracker.net", true},
		{"blocklist beats allowlist", []string{"example.com"}, []string{"secret.example.com"}, "secret.example.com", false},
		{"blocklist beats allowlist for parent", []string{"example.com"}, []string{"example.com"}, "example.com", false},
		{"wildcard pattern", []string{"*.example.com"}, nil, "docs.example.com", true},
		{"wildcard pattern matches apex", []string{"*.example.com"}, nil, "example.com", true},
		{"dot prefix pattern", []string{".example.com"}, nil, "docs.example.com", true},
		{"case insensitive", []string{"Example.COM"}, nil, "DOCS.example.com", true},
		{"trailing dot normalised", []string{"example.com"}, nil, "example.com.", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := guardConfig()
			cfg.AllowedDomains = tc.allowed
			cfg.BlockedDomains = tc.blocked
			g := NewGuard(cfg)
			err := g.CheckHostPolicy(tc.host)
			if tc.want != (err == nil) {
				t.Fatalf("CheckHostPolicy(%q) = %v, permitted want %v", tc.host, err, tc.want)
			}
			if err != nil && !errors.Is(err, ErrBlocked) {
				t.Fatalf("kind = %q, want blocked", KindOf(err))
			}
		})
	}
}

func TestGuardCheckAddr(t *testing.T) {
	g := NewGuard(guardConfig())
	tests := []struct {
		name    string
		network string
		addr    string
		wantErr bool
	}{
		{"public tcp", "tcp", "93.184.216.34:80", false},
		{"loopback tcp", "tcp", "127.0.0.1:8080", true},
		{"metadata tcp", "tcp4", "169.254.169.254:80", true},
		{"udp refused", "udp", "93.184.216.34:53", true},
		{"unix refused", "unix", "/tmp/x", true},
		{"garbage", "tcp", "not-an-address", true},
		{"hostname not ip", "tcp", "example.com:80", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := g.CheckAddr(tc.network, tc.addr)
			if tc.wantErr != (err != nil) {
				t.Fatalf("CheckAddr(%q, %q) = %v, wantErr %v", tc.network, tc.addr, err, tc.wantErr)
			}
		})
	}
}

func TestHostMatches(t *testing.T) {
	tests := []struct {
		host, pattern string
		want          bool
	}{
		{"example.com", "example.com", true},
		{"sub.example.com", "example.com", true},
		{"evil-example.com", "example.com", false},
		{"example.com.evil.net", "example.com", false},
		{"xexample.com", "example.com", false},
		{"example.com", "sub.example.com", false},
		{"", "example.com", false},
		{"example.com", "", false},
		{"example.com", "*.", false},
	}
	for _, tc := range tests {
		if got := hostMatches(tc.host, tc.pattern); got != tc.want {
			t.Errorf("hostMatches(%q, %q) = %v, want %v", tc.host, tc.pattern, got, tc.want)
		}
	}
}

func TestGuardNilAndEmpty(t *testing.T) {
	g := NewGuard(guardConfig())
	if err := g.CheckURL(context.Background(), nil); err == nil {
		t.Fatal("CheckURL(nil) must fail")
	}
	if err := g.CheckURL(context.Background(), &url.URL{Scheme: "http"}); err == nil {
		t.Fatal("CheckURL with no host must fail")
	}
	if err := g.CheckIP(nil); err == nil {
		t.Fatal("CheckIP(nil) must fail")
	}
}
