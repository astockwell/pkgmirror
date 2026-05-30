package upstream

import (
	"net"
	"net/url"
	"strings"
)

// DefaultUpstream is the compiled-in canonical config for one format.
// Operators override these per-tenant via the tenant_upstreams table
// (set in the web console). The list of allowlisted hostnames here is
// extended by PKGMIRROR_UPSTREAM_ALLOWED_HOSTS at server-launch time
// but never via the web console (load-bearing SSRF mitigation).
type DefaultUpstream struct {
	// URL is the canonical public registry root for this format.
	// Per-format adapters join paths onto this.
	URL string

	// Hosts is the set of hostnames the fetcher will contact for this
	// format. Some formats split index + blob hosts (PyPI -> pypi.org +
	// files.pythonhosted.org); both must be present.
	Hosts []string

	// PullThroughSupported reports whether v1 ships pull-through for
	// this format. Generic and Container are explicitly out-of-scope;
	// adapters for the rest land in PRs F through P (see plan S10).
	PullThroughSupported bool
}

// defaultUpstreams is the per-format canonical map. KEEP IN SYNC WITH
// plans/upstream-pull-through.md S5.4. If you add a new public registry
// here, document why and add a test that exercises the allowlist.
var defaultUpstreams = map[string]DefaultUpstream{
	"pypi": {
		URL:                  "https://pypi.org",
		Hosts:                []string{"pypi.org", "files.pythonhosted.org"},
		PullThroughSupported: true, // PR F (the demo deliverable)
	},
	"npm": {
		URL:                  "https://registry.npmjs.org",
		Hosts:                []string{"registry.npmjs.org"},
		PullThroughSupported: false, // PR L
	},
	"go": {
		URL:                  "https://proxy.golang.org",
		Hosts:                []string{"proxy.golang.org", "sum.golang.org"},
		PullThroughSupported: false, // PR J
	},
	"rubygems": {
		URL:                  "https://rubygems.org",
		Hosts:                []string{"rubygems.org", "index.rubygems.org"},
		PullThroughSupported: false, // PR H
	},
	"maven": {
		URL:                  "https://repo.maven.apache.org/maven2",
		Hosts:                []string{"repo.maven.apache.org", "repo1.maven.org"},
		PullThroughSupported: false, // PR K
	},
	"nuget": {
		URL:                  "https://api.nuget.org/v3/index.json",
		Hosts:                []string{"api.nuget.org"},
		PullThroughSupported: false, // PR M
	},
	"cran": {
		URL:                  "https://cran.r-project.org",
		Hosts:                []string{"cran.r-project.org"},
		PullThroughSupported: false, // PR I
	},
	"alpine": {
		URL:                  "https://dl-cdn.alpinelinux.org/alpine",
		Hosts:                []string{"dl-cdn.alpinelinux.org"},
		PullThroughSupported: false, // PR N
	},
	"debian": {
		URL:                  "https://deb.debian.org/debian",
		Hosts:                []string{"deb.debian.org", "security.debian.org"},
		PullThroughSupported: false, // PR O
	},
	"rpm": {
		URL:                  "https://dl.fedoraproject.org",
		Hosts:                []string{"dl.fedoraproject.org", "mirrors.fedoraproject.org"},
		PullThroughSupported: false, // PR P
	},
	// generic + container: explicitly excluded per plan S9.11/9.12.
}

// LookupDefault returns the compiled-in default for format, plus an
// "exists" flag. Formats not in the map (e.g. generic, container) have
// no pull-through and the fetcher returns ErrUpstreamOff regardless of
// the tenant_upstreams row.
func LookupDefault(format string) (DefaultUpstream, bool) {
	d, ok := defaultUpstreams[strings.ToLower(format)]
	return d, ok
}

// DefaultHosts returns the union of every hostname in the compiled-in
// defaults map. Used by the allowlist constructor as the starting set
// (before env-supplied additions).
func DefaultHosts() []string {
	out := make([]string, 0, 16)
	seen := make(map[string]struct{})
	for _, d := range defaultUpstreams {
		for _, h := range d.Hosts {
			if _, dup := seen[h]; dup {
				continue
			}
			seen[h] = struct{}{}
			out = append(out, h)
		}
	}
	return out
}

// Allowlist is the runtime hostname-allow gate. Compiled-in defaults
// plus PKGMIRROR_UPSTREAM_ALLOWED_HOSTS at construction time. Operators
// CANNOT extend this through any user-facing UI.
type Allowlist struct {
	hosts map[string]struct{}
}

// NewAllowlist builds an Allowlist from the compiled-in default hosts
// plus extra (typically from env). Empty hostnames are dropped;
// case-insensitive matching.
func NewAllowlist(extra []string) *Allowlist {
	a := &Allowlist{hosts: make(map[string]struct{})}
	for _, h := range DefaultHosts() {
		a.add(h)
	}
	for _, h := range extra {
		a.add(h)
	}
	return a
}

func (a *Allowlist) add(h string) {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return
	}
	a.hosts[h] = struct{}{}
}

// Permits reports whether the given URL's host is on the allowlist.
// Strips port; case-insensitive. URL with empty host (e.g. relative
// path) returns false - relative URLs should have been joined with the
// per-tenant base by the time they hit here.
func (a *Allowlist) Permits(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	_, ok := a.hosts[host]
	return ok
}

// Hosts returns the allowlisted hostnames (sorted-ish; for tests + the
// /-/healthz debug surface).
func (a *Allowlist) Hosts() []string {
	out := make([]string, 0, len(a.hosts))
	for h := range a.hosts {
		out = append(out, h)
	}
	return out
}

// privateRanges is the set of CIDRs we refuse to dial. Defense-in-depth
// against SSRF if an admin somehow gets a host onto the allowlist that
// resolves to an internal address. Override with
// PKGMIRROR_UPSTREAM_ALLOW_PRIVATE_IPS=true for intermediate-mirror
// deployments.
var privateRanges = []string{
	"127.0.0.0/8",     // loopback v4
	"10.0.0.0/8",      // RFC1918
	"172.16.0.0/12",   // RFC1918
	"192.168.0.0/16",  // RFC1918
	"169.254.0.0/16",  // link-local v4
	"::1/128",         // loopback v6
	"fc00::/7",        // unique-local v6
	"fe80::/10",       // link-local v6
	"0.0.0.0/8",       // unspecified
	"100.64.0.0/10",   // CGN
}

// PrivateIPGuard checks whether an IP is in a private range. Used by
// the HTTP transport's DialContext to fail SSRF-prone connections
// before any bytes are sent.
type PrivateIPGuard struct {
	nets      []*net.IPNet
	permitted bool // if true, guard is disabled (operator override)
}

// NewPrivateIPGuard parses the compiled-in private ranges. When permit
// is true (PKGMIRROR_UPSTREAM_ALLOW_PRIVATE_IPS=true), the guard
// reports every IP as allowed.
func NewPrivateIPGuard(permit bool) *PrivateIPGuard {
	g := &PrivateIPGuard{permitted: permit}
	for _, cidr := range privateRanges {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue // never happens for the compiled-in list
		}
		g.nets = append(g.nets, ipnet)
	}
	return g
}

// AllowsIP reports whether the IP is permitted as an upstream target.
// When the guard is in permitted=true mode it allows everything;
// otherwise it rejects IPs that fall in any private range.
func (g *PrivateIPGuard) AllowsIP(ip net.IP) bool {
	if g.permitted || ip == nil {
		return g.permitted
	}
	for _, n := range g.nets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}
