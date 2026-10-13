package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// A visitor on the same network as the server (often the same computer) who
// opens the *public* link sends their video out to the internet and back,
// bounded by the home connection's slow upload speed. Detecting that lets the
// page point them at the direct address instead.

const defaultPublicIPURL = "https://api.ipify.org"

type netInfo struct {
	publicIP atomic.Value // string
}

func (n *netInfo) get() string {
	v, _ := n.publicIP.Load().(string)
	return v
}

// watchPublicIP learns the server's public address and refreshes it hourly.
// url "-" disables the lookup (tests, offline use).
func (s *Server) watchPublicIP(ctx context.Context, url string) {
	if url == "-" {
		return
	}
	if url == "" {
		url = defaultPublicIPURL
	}
	lookup := func() {
		c, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(c, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		if ip := net.ParseIP(strings.TrimSpace(string(b))); ip != nil {
			s.net.publicIP.Store(ip.String())
		}
	}
	lookup()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			lookup()
		}
	}
}

// remoteIP is the visitor's address, looking through the tunnel/proxy.
func remoteIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	return clientIP(r)
}

// sameNetwork reports whether the visitor is on the server's own network.
// IPv4: same public address. IPv6: devices behind one router share a /64
// prefix, so compare against the prefixes of this host's own addresses.
func (s *Server) sameNetwork(r *http.Request) bool {
	ip := net.ParseIP(strings.TrimSpace(remoteIP(r)))
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		pub := s.net.get()
		return pub != "" && v4.String() == pub
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	return sharesV6Prefix(ip, addrs)
}

// sharesV6Prefix reports whether ip falls in the same /64 as any global
// unicast IPv6 address in addrs.
func sharesV6Prefix(ip net.IP, addrs []net.Addr) bool {
	ip = ip.To16()
	if ip == nil || ip.To4() != nil || !ip.IsGlobalUnicast() {
		return false
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		own := n.IP.To16()
		if own == nil || n.IP.To4() != nil || !n.IP.IsGlobalUnicast() || n.IP.IsPrivate() {
			continue
		}
		if string(own[:8]) == string(ip[:8]) {
			return true
		}
	}
	return false
}

// localURLs lists the direct addresses a same-network visitor can use.
func localURLs(port string) []string {
	urls := []string{"http://localhost:" + port}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return urls
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil || !ipn.IP.IsPrivate() {
			continue
		}
		urls = append(urls, fmt.Sprintf("http://%s:%s", ipn.IP, port))
	}
	return urls
}
