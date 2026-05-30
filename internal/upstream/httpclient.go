package upstream

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpClientConfig holds the knobs the env loader (config.go) plumbs in
// to NewHTTPClient. Defaults are conservative; ops can tighten them.
type httpClientConfig struct {
	allowlist          *Allowlist
	privateGuard       *PrivateIPGuard
	allowPlaintext     bool          // permit http:// (default false)
	connectTimeout     time.Duration // per-connection dial
	overallTimeout     time.Duration // request total
	idleConnTimeout    time.Duration
	maxIdleConns       int
	maxConnsPerHost    int
	userAgent          string
}

// NewHTTPClient builds the package-internal *http.Client used by every
// fetch. The transport DialContext is wrapped to refuse private IPs
// (defense in depth: the URL host passed the allowlist gate but DNS
// might still resolve it to a private address). The CheckRedirect hook
// refuses to follow redirects to non-allowlisted hosts (CWE-918).
func newHTTPClient(cfg httpClientConfig) *http.Client {
	dialer := &net.Dialer{
		Timeout:   cfg.connectTimeout,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment, // honor HTTPS_PROXY
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Resolve and pre-check every IP the host resolves to.
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if !cfg.privateGuard.AllowsIP(ip) {
					return nil, &net.OpError{
						Op:  "dial",
						Net: network,
						Err: &privateIPError{ip: ip.String(), host: host},
					}
				}
			}
			// Dial the first allowed IP we got - the resolver already
			// returned them; the guard above rejected anything private.
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
		MaxIdleConns:        cfg.maxIdleConns,
		MaxConnsPerHost:     cfg.maxConnsPerHost,
		IdleConnTimeout:     cfg.idleConnTimeout,
		TLSHandshakeTimeout: cfg.connectTimeout,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{
		Transport: &uaTransport{rt: tr, ua: cfg.userAgent},
		Timeout:   cfg.overallTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			if !cfg.allowPlaintext && req.URL.Scheme != "https" {
				return &redirectBlockedError{
					url:    req.URL.String(),
					reason: "plaintext redirect blocked (set PKGMIRROR_UPSTREAM_ALLOW_PLAINTEXT=true to permit)",
				}
			}
			if !cfg.allowlist.Permits(req.URL) {
				return &redirectBlockedError{
					url:    req.URL.String(),
					reason: "redirect target host not in allowlist",
				}
			}
			return nil
		},
	}
}

// uaTransport sets a fixed User-Agent on every outbound request. We
// identify pkgmirror so upstream operators can rate-limit or contact us.
type uaTransport struct {
	rt http.RoundTripper
	ua string
}

func (u *uaTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if u.ua != "" && r.Header.Get("User-Agent") == "" {
		// Clone to avoid mutating the caller's request.
		r2 := r.Clone(r.Context())
		r2.Header.Set("User-Agent", u.ua)
		return u.rt.RoundTrip(r2)
	}
	return u.rt.RoundTrip(r)
}

// privateIPError surfaces when the dialer blocks a private IP. Caller's
// errors.Is(err, ErrUpstreamForbidden) returns true via the Unwrap
// chain on the wrapping url.Error.
type privateIPError struct {
	ip, host string
}

func (e *privateIPError) Error() string {
	return "host " + e.host + " resolved to private/loopback/link-local IP " + e.ip + "; refusing to dial (CWE-918)"
}

// Is allows errors.Is(err, ErrUpstreamForbidden) to match.
func (e *privateIPError) Is(target error) bool { return target == ErrUpstreamForbidden }

// redirectBlockedError surfaces when CheckRedirect refuses a hop.
type redirectBlockedError struct {
	url, reason string
}

func (e *redirectBlockedError) Error() string {
	return "redirect to " + e.url + " blocked: " + e.reason
}

func (e *redirectBlockedError) Is(target error) bool { return target == ErrUpstreamForbidden }

// checkSchemeAndHost is the pre-request gate. The fetcher calls this
// before doing any I/O so a misconfigured upstream URL fails fast with
// ErrUpstreamForbidden instead of producing a network-level error the
// operator has to interpret.
func checkSchemeAndHost(u *url.URL, al *Allowlist, allowPlaintext bool) error {
	if u == nil {
		return ErrUpstreamForbidden
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(allowPlaintext && scheme == "http") {
		return &redirectBlockedError{
			url:    u.String(),
			reason: "scheme " + scheme + " not permitted (set PKGMIRROR_UPSTREAM_ALLOW_PLAINTEXT=true for http)",
		}
	}
	if !al.Permits(u) {
		return &redirectBlockedError{
			url:    u.String(),
			reason: "host " + u.Hostname() + " not in allowlist",
		}
	}
	return nil
}
