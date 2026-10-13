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

// sameNetwork reports whether the visitor shares the server's public address.
func (s *Server) sameNetwork(r *http.Request) bool {
	pub := s.net.get()
	return pub != "" && remoteIP(r) == pub
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
