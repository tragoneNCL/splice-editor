package server

import (
	"net/http"
	"time"
)

// quit lets the person at the machine stop the app from its own window.
// Like updating, it is refused for anyone arriving through a tunnel or LAN.
func (s *Server) quit(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Quit == nil || !isLocalCaller(r) {
		fail(w, http.StatusForbidden, "this can only be done from the computer running the app")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.cfg.Quit()
	}()
}
