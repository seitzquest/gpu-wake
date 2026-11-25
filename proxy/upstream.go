package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Upstream is one app on the GPU host, served on its own loopback listener.
type Upstream struct {
	cfg      UpstreamConfig
	global   *Config
	target   *url.URL
	proxy    *httputil.ReverseProxy
	client   *http.Client
	machine  *Machine
	limiter  *Limiter
	bridge   *Bridge
	pages    *Pages
	inflight chan struct{}

	mu           sync.Mutex
	healthyUntil time.Time
	lastRequest  time.Time
	requests     int64
	// Paused from the host while another job holds the GPU (gpu-exclusive, gpu-guard):
	// visitors get a "GPU in use" page instead of waiting for an app that was stopped on purpose.
	paused       bool
	pausedReason string
	pausedAt     time.Time
}

// controlPrefix is reserved on every upstream for the waiting page's own requests.
const controlPrefix = "/__gpu_wake/"

// A pause lifts itself after maxPause, in case the host that set it slept or crashed first.
const maxPause = 6 * time.Hour

func newUpstream(c UpstreamConfig, g *Config, m *Machine, lim *Limiter, br *Bridge, pages *Pages) (*Upstream, error) {
	t, err := url.Parse(c.Target)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       20 * time.Second, // idle keep-alives must not keep the host awake
		ExpectContinueTimeout: time.Second,
	}
	u := &Upstream{cfg: c, global: g, target: t, machine: m, limiter: lim, bridge: br, pages: pages,
		inflight: make(chan struct{}, c.MaxInflight),
		client:   &http.Client{Timeout: 4 * time.Second, Transport: transport}}
	rp := httputil.NewSingleHostReverseProxy(t)
	rp.Transport = transport
	rp.FlushInterval = -1 // stream chunked responses and server-sent events immediately
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		u.markUnhealthy()
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Printf("%s: upstream error: %v", c.Name, err)
		http.Error(w, "GPU host unavailable", http.StatusBadGateway)
	}
	u.proxy = rp
	return u, nil
}

func (u *Upstream) title() string {
	if u.cfg.Title != "" {
		return u.cfg.Title
	}
	return u.cfg.Name
}

func (u *Upstream) markUnhealthy() {
	u.mu.Lock()
	u.healthyUntil = time.Time{}
	u.mu.Unlock()
}

// healthy checks the app's health path. Any status below 500, and 503, counts as up: the
// process is serving HTTP, even if the app reports that it is still loading.
func (u *Upstream) healthy(ctx context.Context) bool {
	u.mu.Lock()
	cached := time.Now().Before(u.healthyUntil)
	u.mu.Unlock()
	if cached {
		return true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.target.String()+u.cfg.Health, nil)
	if err != nil {
		return false
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 && resp.StatusCode != http.StatusServiceUnavailable {
		return false
	}
	u.mu.Lock()
	u.healthyUntil = time.Now().Add(time.Duration(u.global.HealthyCacheS) * time.Second)
	u.mu.Unlock()
	return true
}

// waitReady wakes the host if needed and waits until the app answers or the wake times out.
func (u *Upstream) waitReady(ctx context.Context, reason string) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(u.global.WakeTimeoutS)*time.Second)
	defer cancel()
	for {
		if u.healthy(ctx) {
			return true
		}
		if u.machine.State() != stateWaking && !u.machine.Probe(ctx) {
			u.machine.Wake(reason)
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}

func (u *Upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, controlPrefix) {
		u.serveControl(w, r)
		return
	}
	if !u.bridge.Allows(u.cfg.Audience) {
		u.servePage(w, r, "offline.html", "offline", 3600, nil) // kill switch: never touches the host
		return
	}
	if paused, reason := u.pauseState(); paused {
		u.servePage(w, r, "busy.html", "gpu in use", 120, map[string]any{"Reason": reason})
		return
	}
	u.mu.Lock()
	u.lastRequest = time.Now()
	u.requests++
	u.mu.Unlock()

	// Skip the probe while waking or when the host is known to be asleep, so the waiting page
	// comes back at once; otherwise allow a few seconds, as a fresh VPN path can take over 1 s.
	if !u.machine.KnownAsleep() && u.machine.State() != stateWaking {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		ok := u.healthy(ctx)
		cancel()
		if ok {
			u.forward(w, r)
			return
		}
	}

	if wantsHTML(r) {
		// Browsers get the waiting page immediately. With wake_policy "any" it also starts the
		// wake; with "explicit" the page's script asks for it, so bots never wake the host.
		if u.cfg.WakePolicy == "any" {
			u.tryWake(r, "page")
		}
		u.serveWaking(w, r)
		return
	}
	if u.cfg.WakePolicy != "any" && isSafeMethod(r.Method) {
		msg := "gpu host asleep"
		if u.machine.State() == stateAwake {
			msg = "app not ready on the gpu host"
		}
		w.Header().Set("Retry-After", "10")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": msg, "state": u.machine.State(), "wake": "POST " + controlPrefix + "wake",
		})
		return
	}
	if code, msg := u.tryWake(r, "api"); code != 0 {
		w.Header().Set("Retry-After", "60")
		http.Error(w, msg, code)
		return
	}
	// API clients wait for the host; the request body stays unread until then.
	if !u.waitReady(r.Context(), u.cfg.Name+": api "+clientIP(r)) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "GPU host did not wake in time", http.StatusGatewayTimeout)
		return
	}
	u.forward(w, r)
}

// forward proxies the request, capped at max_inflight concurrent requests per upstream.
func (u *Upstream) forward(w http.ResponseWriter, r *http.Request) {
	select {
	case u.inflight <- struct{}{}:
		defer func() { <-u.inflight }()
		u.proxy.ServeHTTP(w, r)
	default:
		w.Header().Set("Retry-After", "5")
		http.Error(w, "busy, try again shortly", http.StatusServiceUnavailable)
	}
}

// tryWake starts or joins a wake. Public upstreams are limited per client IP and by the
// public wake breaker. It returns a non-zero HTTP status when the wake is refused.
func (u *Upstream) tryWake(r *http.Request, kind string) (int, string) {
	switch u.machine.State() {
	case stateWaking:
		return 0, ""
	case stateAwake:
		if u.machine.Probe(r.Context()) {
			return 0, "" // the host is up; only the app is not ready yet
		}
	}
	ip := clientIP(r)
	if u.cfg.Audience == audiencePublic {
		if !u.limiter.Allow(ip) {
			return http.StatusTooManyRequests, "too many wake requests"
		}
		if !u.bridge.AllowPublicWake() {
			return http.StatusServiceUnavailable, "the GPU host is resting; try again later"
		}
	}
	u.machine.Wake(fmt.Sprintf("%s: %s %s (%s)", u.cfg.Name, kind, ip, u.cfg.Audience))
	return 0, ""
}

func (u *Upstream) pauseState() (bool, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.paused && time.Since(u.pausedAt) > maxPause {
		u.paused = false
		log.Printf("%s: pause expired", u.cfg.Name)
	}
	return u.paused, u.pausedReason
}

func (u *Upstream) setPause(paused bool, reason string) {
	u.mu.Lock()
	u.paused, u.pausedReason, u.pausedAt = paused, reason, time.Now()
	u.healthyUntil = time.Time{}
	u.mu.Unlock()
	log.Printf("%s: paused=%v%s", u.cfg.Name, paused, suffix(reason))
}

// serveControl answers the waiting page: state, status and wake.
func (u *Upstream) serveControl(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !u.bridge.Allows(u.cfg.Audience) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "state": "offline"})
		return
	}
	switch strings.TrimPrefix(r.URL.Path, controlPrefix) {
	case "state":
		// For status badges elsewhere: answered from memory, never contacts the host.
		paused, _ := u.pauseState()
		writeJSON(w, http.StatusOK, map[string]any{"state": u.machine.State(), "paused": paused})
	case "status":
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		ready := u.healthy(ctx)
		cancel()
		writeJSON(w, http.StatusOK, map[string]any{"ready": ready, "state": u.machine.State()})
	case "wake":
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if code, msg := u.tryWake(r, "visitor"); code != 0 {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, code, map[string]any{"error": msg, "state": u.machine.State()})
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			writeJSON(w, http.StatusAccepted, map[string]any{"state": u.machine.State()})
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther) // no-JS form fallback
	default:
		http.NotFound(w, r)
	}
}

func (u *Upstream) serveWaking(w http.ResponseWriter, r *http.Request) {
	u.servePage(w, r, "waking.html", "gpu host asleep", 10, map[string]any{
		"Auto": u.cfg.WakePolicy == "explicit", "Timeout": u.global.WakeTimeoutS,
		"State": u.machine.State(),
	})
}

// servePage answers with a 503 page for browsers and JSON for other clients.
func (u *Upstream) servePage(w http.ResponseWriter, r *http.Request, page, apiError string, retryS int, data map[string]any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", fmt.Sprint(retryS))
	if !wantsHTML(r) {
		body := map[string]any{"error": apiError, "state": u.machine.State()}
		if reason, ok := data["Reason"]; ok {
			body["reason"] = reason
		}
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method == http.MethodHead {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["Title"] = u.title()
	if err := u.pages.Render(w, page, data); err != nil {
		log.Printf("%s: render %s: %v", u.cfg.Name, page, err)
	}
}

// clientIP trusts X-Real-IP and X-Forwarded-For: the listeners are loopback-only, so every
// request comes through the front proxy, which must set them.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func wantsHTML(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		strings.Contains(r.Header.Get("Accept"), "text/html")
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// Limiter allows n wake triggers per client IP per hour.
type Limiter struct {
	mu   sync.Mutex
	n    int
	hits map[string][]time.Time
}

func newLimiter(n int) *Limiter { return &Limiter{n: n, hits: map[string][]time.Time{}} }

func (l *Limiter) Allow(ip string) bool {
	if l.n <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-time.Hour)
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.n {
		l.hits[ip] = kept
		return false
	}
	// Bound memory under abuse. Clearing forgets per-IP history; the public wake breaker
	// still caps the total.
	if len(l.hits) >= 10000 {
		l.hits = map[string][]time.Time{}
	}
	l.hits[ip] = append(kept, time.Now())
	return true
}
