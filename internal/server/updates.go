package server

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"filmeditor/internal/update"
)

// Updating replaces the running program, so it is allowed only for the person
// sitting at the machine: loopback callers with no proxy headers. Anyone
// reaching the app through a tunnel or the LAN is refused.
func isLocalCaller(r *http.Request) bool {
	if r.Header.Get("CF-Connecting-IP") != "" || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type updateCache struct {
	mu      sync.Mutex
	at      time.Time
	rel     update.Release
	newer   bool
	lastErr error
}

const updateCacheTTL = 10 * time.Minute

func (s *Server) updateInfo(w http.ResponseWriter, r *http.Request) {
	u := s.cfg.Updater
	resp := map[string]any{"version": s.cfg.Version, "enabled": false, "available": false}
	if u == nil || !u.Enabled() || !isLocalCaller(r) {
		writeJSON(w, 200, resp)
		return
	}
	resp["enabled"] = true
	c := &s.updates
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) > updateCacheTTL || r.URL.Query().Get("fresh") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		c.rel, c.newer, c.lastErr = u.Latest(ctx)
		c.at = time.Now()
	}
	if c.lastErr == nil && c.newer {
		resp["available"] = true
		resp["latest"] = c.rel.Tag
		resp["notes"] = c.rel.Notes
	}
	writeJSON(w, 200, resp)
}

func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	u := s.cfg.Updater
	if u == nil || !u.Enabled() || !isLocalCaller(r) {
		fail(w, http.StatusForbidden, "updates can only be applied from the computer running the app")
		return
	}
	c := &s.updates
	c.mu.Lock()
	rel, newer := c.rel, c.newer
	c.mu.Unlock()
	if !newer {
		fail(w, http.StatusConflict, "no update is available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := u.Apply(ctx, rel); err != nil {
		fail(w, http.StatusBadGateway, "update failed: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "version": rel.Tag, "restarting": s.cfg.Restart != nil})
	if s.cfg.Restart != nil {
		go func() {
			time.Sleep(400 * time.Millisecond) // let the response reach the browser
			s.cfg.Restart()
		}()
	}
}
