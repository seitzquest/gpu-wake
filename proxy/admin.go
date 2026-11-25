package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// admin is the private control listener: status page, manual wake, kill switch, pauses and
// GPU release. Expose it only on a private network.
type admin struct {
	m      *Machine
	ups    []*Upstream
	bridge *Bridge
	pages  *Pages
	start  time.Time
}

func (a *admin) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		a.m.Probe(r.Context())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := a.pages.Render(w, "status.html", a.status()); err != nil {
			log.Printf("admin: render status: %v", err)
		}
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		a.m.Probe(r.Context())
		writeJSON(w, http.StatusOK, a.status())
	})
	mux.HandleFunc("POST /wake", func(w http.ResponseWriter, r *http.Request) {
		if !a.bridge.Allows(audiencePrivate) {
			http.Error(w, "bridge is off", http.StatusConflict)
			return
		}
		a.m.Wake("manual " + clientIP(r))
		a.reply(w, r, map[string]any{"state": stateWaking})
	})
	// Kill switch: mode=on|public-off|off and an optional reason.
	mux.HandleFunc("POST /bridge", func(w http.ResponseWriter, r *http.Request) {
		if err := a.bridge.Set(r.FormValue("mode"), r.FormValue("reason")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.reply(w, r, a.bridge.Snapshot())
	})
	mux.HandleFunc("POST /breaker/reset", func(w http.ResponseWriter, r *http.Request) {
		a.bridge.ResetBreaker()
		a.reply(w, r, a.bridge.Snapshot())
	})
	// Pause upstreams while another job holds the GPU: names=a,b paused=true|false reason=...
	mux.HandleFunc("POST /pause", func(w http.ResponseWriter, r *http.Request) {
		paused := r.FormValue("paused") != "false"
		want := map[string]bool{}
		for _, n := range strings.Split(r.FormValue("names"), ",") {
			if n = strings.TrimSpace(n); n != "" {
				want[n] = true
			}
		}
		done := []string{}
		for _, u := range a.ups {
			if want[u.cfg.Name] {
				u.setPause(paused, r.FormValue("reason"))
				done = append(done, u.cfg.Name)
			}
		}
		a.reply(w, r, map[string]any{"paused": paused, "upstreams": done})
	})
	mux.HandleFunc("POST /release", a.releaseGPU)
	return sameOrigin(mux)
}

func (a *admin) status() map[string]any {
	a.m.mu.Lock()
	st := map[string]any{
		"state": a.m.state, "last_seen": nullTime(a.m.lastSeen), "last_wake": a.m.lastWake,
		"host": a.m.cfg.Host.IP, "proxy_started": a.start, "version": version,
	}
	if a.m.state == stateWaking {
		st["waking_for_s"] = time.Since(a.m.wakeStarted).Seconds()
		st["waking_reason"] = a.m.wakeReason
	}
	a.m.mu.Unlock()
	ups := []map[string]any{}
	var last time.Time
	for _, u := range a.ups {
		u.mu.Lock()
		ups = append(ups, map[string]any{
			"name": u.cfg.Name, "title": u.title(), "target": u.cfg.Target, "listen": u.cfg.Listen,
			"wake_policy": u.cfg.WakePolicy, "audience": u.cfg.Audience,
			"last_request": nullTime(u.lastRequest), "requests": u.requests,
			"healthy": time.Now().Before(u.healthyUntil), "inflight": len(u.inflight),
			"paused": u.paused, "paused_reason": u.pausedReason,
			"can_release": u.cfg.ReleasePath != "",
		})
		if u.lastRequest.After(last) {
			last = u.lastRequest
		}
		u.mu.Unlock()
	}
	st["upstreams"], st["last_activity"] = ups, nullTime(last)
	st["bridge"] = a.bridge.Snapshot()
	return st
}

// reply answers form posts from the status page with a redirect, and API clients with JSON.
func (a *admin) reply(w http.ResponseWriter, r *http.Request, v map[string]any) {
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusOK, v)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sameOrigin rejects cross-site POSTs, so a web page open on a device in the private network
// cannot flip the kill switch or wake the host. curl and other non-browser clients send no
// Sec-Fetch-Site.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			site := r.Header.Get("Sec-Fetch-Site")
			if site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); site == "" && o != "" {
				if ou, err := url.Parse(o); err != nil || ou.Host != r.Host {
					http.Error(w, "cross-site request refused", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
