package middleware

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestLoginRateLimiter_AllowsUpToMax exercises the bucket boundary.
// 10 calls all pass; 11th trips the limit.
func TestLoginRateLimiter_AllowsUpToMax(t *testing.T) {
	rl := NewLoginRateLimiter(10, time.Minute)
	for i := 0; i < 10; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatalf("attempt %d: should be allowed", i+1)
		}
	}
	if rl.Allow("1.2.3.4") {
		t.Fatal("11th attempt should be denied")
	}
}

// TestLoginRateLimiter_PerIP confirms buckets are keyed by IP.
func TestLoginRateLimiter_PerIP(t *testing.T) {
	rl := NewLoginRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		_ = rl.Allow("1.2.3.4")
	}
	if rl.Allow("1.2.3.4") {
		t.Fatal("ip1 should be over limit")
	}
	if !rl.Allow("5.6.7.8") {
		t.Fatal("ip2 should still be allowed (separate bucket)")
	}
}

// TestLoginRateLimiter_Forget lets a successful login clear the bucket.
func TestLoginRateLimiter_Forget(t *testing.T) {
	rl := NewLoginRateLimiter(2, time.Minute)
	_ = rl.Allow("1.2.3.4")
	_ = rl.Allow("1.2.3.4")
	if rl.Allow("1.2.3.4") {
		t.Fatal("expected over-limit")
	}
	rl.Forget("1.2.3.4")
	if !rl.Allow("1.2.3.4") {
		t.Fatal("expected fresh allow after Forget")
	}
}

// TestLoginRateLimiter_WindowExpires confirms the bucket auto-resets.
func TestLoginRateLimiter_WindowExpires(t *testing.T) {
	rl := NewLoginRateLimiter(2, 10*time.Millisecond)
	_ = rl.Allow("1.2.3.4")
	_ = rl.Allow("1.2.3.4")
	if rl.Allow("1.2.3.4") {
		t.Fatal("third attempt within window should fail")
	}
	time.Sleep(15 * time.Millisecond)
	if !rl.Allow("1.2.3.4") {
		t.Fatal("after window, bucket should reset")
	}
}

// TestClientIPFor_NoTrustedProxiesIgnoresXFF is the load-bearing
// security property: a spoofed X-Forwarded-For from an untrusted edge
// MUST NOT win over the immediate peer.
func TestClientIPFor_NoTrustedProxiesIgnoresXFF(t *testing.T) {
	r := httptest.NewRequest("POST", "/console/login", nil)
	r.RemoteAddr = "203.0.113.5:51234" // peer is some untrusted internet client
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	got := ClientIPFor(r, nil)
	if got != "203.0.113.5" {
		t.Errorf("with no trusted proxies, expected peer addr, got %q", got)
	}
}

// TestClientIPFor_HonorsXFFFromTrustedProxy is the legitimate case:
// the immediate peer is a known proxy, so its XFF is honored.
func TestClientIPFor_HonorsXFFFromTrustedProxy(t *testing.T) {
	r := httptest.NewRequest("POST", "/console/login", nil)
	r.RemoteAddr = "10.0.0.1:51234"
	r.Header.Set("X-Forwarded-For", "203.0.113.5")
	got := ClientIPFor(r, []string{"10.0.0.0/8"})
	if got != "203.0.113.5" {
		t.Errorf("expected XFF value from trusted proxy, got %q", got)
	}
}

// TestClientIPFor_WalksChainPastTrustedHops handles N proxies in a row.
func TestClientIPFor_WalksChainPastTrustedHops(t *testing.T) {
	r := httptest.NewRequest("POST", "/console/login", nil)
	r.RemoteAddr = "10.0.0.1:51234"
	// L7 LB chain: client -> outer LB (10.0.0.2) -> inner LB (10.0.0.1)
	r.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.2")
	got := ClientIPFor(r, []string{"10.0.0.0/8"})
	if got != "203.0.113.5" {
		t.Errorf("expected real client past trusted hops, got %q", got)
	}
}

// TestClientIPFor_SpoofedXFFFromUntrustedSource keeps the peer IP even
// when the spoofed XFF tries to look like it came from a trusted proxy.
// This is the exact scenario plan §11.2 calls out as a regression risk.
func TestClientIPFor_SpoofedXFFFromUntrustedSource(t *testing.T) {
	r := httptest.NewRequest("POST", "/console/login", nil)
	r.RemoteAddr = "203.0.113.99:51234" // untrusted peer
	r.Header.Set("X-Forwarded-For", "10.0.0.99, 1.2.3.4")
	got := ClientIPFor(r, []string{"10.0.0.0/8"})
	if got != "203.0.113.99" {
		t.Errorf("spoofed XFF must NOT override untrusted peer, got %q", got)
	}
}

// TestClientIPFor_SingleIPTrustedProxy (not CIDR) also works.
func TestClientIPFor_SingleIPTrustedProxy(t *testing.T) {
	r := httptest.NewRequest("POST", "/console/login", nil)
	r.RemoteAddr = "10.0.0.1:51234"
	r.Header.Set("X-Forwarded-For", "203.0.113.5")
	got := ClientIPFor(r, []string{"10.0.0.1"})
	if got != "203.0.113.5" {
		t.Errorf("single-IP trusted proxy entry should honor XFF, got %q", got)
	}
}

// TestClientIPFor_EmptyXFFFallsBackToPeer covers the trusted-peer-no-
// header case.
func TestClientIPFor_EmptyXFFFallsBackToPeer(t *testing.T) {
	r := httptest.NewRequest("POST", "/console/login", nil)
	r.RemoteAddr = "10.0.0.1:51234"
	got := ClientIPFor(r, []string{"10.0.0.0/8"})
	if got != "10.0.0.1" {
		t.Errorf("with no XFF, expected the trusted peer addr, got %q", got)
	}
}
