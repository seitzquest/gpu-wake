package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHost is a GPU host that wakes when it receives a magic packet.
type fakeHost struct {
	awake   atomic.Bool
	packets atomic.Int32
	app     *httptest.Server
	hits    atomic.Int32
}

func newFakeHost(t *testing.T, handler http.HandlerFunc) *fakeHost {
	h := &fakeHost{}
	h.app = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.awake.Load() {
			// A suspended host does not answer; closing the connection is the closest stand-in.
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		h.hits.Add(1)
		if handler != nil {
			handler(w, r)
			return
		}
		_, _ = io.WriteString(w, "hello from the app")
	}))
	t.Cleanup(h.app.Close)
	return h
}

type testProxy struct {
	host   *fakeHost
	cfg    *Config
	m      *Machine
	bridge *Bridge
	ups    map[string]*Upstream
	admin  http.Handler
}

func newTestProxy(t *testing.T, host *fakeHost, ups ...UpstreamConfig) *testProxy {
	t.Helper()
	cfg := defaultConfig()
	cfg.Host = HostConfig{MAC: "00:11:22:33:44:55", IP: "192.0.2.10", Broadcast: "192.0.2.255"}
	cfg.WakeTimeoutS, cfg.ResendIntervalS, cfg.HealthyCacheS = 5, 1, 0
	cfg.StateFile = ""
	for i := range ups {
		ups[i].Listen = "127.0.0.1:0"
		ups[i].Target = host.app.URL
	}
	cfg.Upstreams = ups
	// Distinct listen addresses keep validation happy; they are never bound in tests.
	for i := range cfg.Upstreams {
		cfg.Upstreams[i].Listen = "127.0.0.1:" + string(rune('1'+i)) + "000"
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	m := newMachine(cfg)
	m.ping = func(context.Context) bool { return host.awake.Load() }
	m.send = func(HostConfig) error { host.packets.Add(1); host.awake.Store(true); return nil }
	pages, err := loadPages(cfg.Pages)
	if err != nil {
		t.Fatal(err)
	}
	br := loadBridge("", cfg.PublicWakesPerHour)
	lim := newLimiter(cfg.WakesPerIPHour)
	p := &testProxy{host: host, cfg: cfg, m: m, bridge: br, ups: map[string]*Upstream{}}
	var list []*Upstream
	for _, uc := range cfg.Upstreams {
		u, err := newUpstream(uc, cfg, m, lim, br, pages)
		if err != nil {
			t.Fatal(err)
		}
		p.ups[uc.Name] = u
		list = append(list, u)
	}
	p.admin = (&admin{m: m, ups: list, bridge: br, pages: pages, start: time.Now()}).routes()
	m.Probe(context.Background())
	return p
}

func do(h http.Handler, method, path, accept, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	r.Header.Set("X-Real-IP", ip)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const browser = "text/html,application/xhtml+xml"

func TestAwakeHostIsForwarded(t *testing.T) {
	host := newFakeHost(t, nil)
	host.awake.Store(true)
	p := newTestProxy(t, host, UpstreamConfig{Name: "app"})
	w := do(p.ups["app"], "GET", "/", browser, "203.0.113.1")
	if w.Code != 200 || w.Body.String() != "hello from the app" {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	if host.packets.Load() != 0 {
		t.Fatal("woke a host that was already awake")
	}
}

func TestExplicitPolicyNeverWakesOnGet(t *testing.T) {
	host := newFakeHost(t, nil)
	p := newTestProxy(t, host, UpstreamConfig{Name: "app", Title: "My <App>"})
	u := p.ups["app"]

	// A browser (or a crawler sending Accept: text/html) gets the waiting page, nothing more.
	w := do(u, "GET", "/", browser, "203.0.113.1")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "Waking the GPU") {
		t.Fatalf("got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "My &lt;App&gt;") || strings.Contains(w.Body.String(), "My <App>") {
		t.Fatal("title must be escaped")
	}
	if w.Header().Get("X-Robots-Tag") != "noindex" || w.Header().Get("Retry-After") == "" {
		t.Fatal("waiting page must be noindex with Retry-After")
	}
	// Link previews and API GETs get JSON telling them how to wake, and do not wake.
	w = do(u, "GET", "/api/thing", "application/json", "203.0.113.1")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "POST /__gpu_wake/wake") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if host.packets.Load() != 0 || p.m.State() == stateWaking {
		t.Fatal("a GET woke the host under the explicit policy")
	}

	// The waiting page's script asks explicitly.
	w = do(u, "POST", "/__gpu_wake/wake", "application/json", "203.0.113.1")
	if w.Code != 202 {
		t.Fatalf("wake: %d %s", w.Code, w.Body.String())
	}
	waitFor(t, func() bool { return p.m.State() == stateAwake })
	if host.packets.Load() == 0 {
		t.Fatal("no magic packet sent")
	}
	w = do(u, "GET", "/", browser, "203.0.113.1")
	if w.Code != 200 {
		t.Fatalf("after wake: %d", w.Code)
	}
}

func TestAnyPolicyWakesAndWaits(t *testing.T) {
	host := newFakeHost(t, nil)
	p := newTestProxy(t, host, UpstreamConfig{Name: "api", Audience: "private", WakePolicy: "any"})
	start := time.Now()
	w := do(p.ups["api"], "GET", "/v1/models", "application/json", "100.64.0.2")
	if w.Code != 200 || w.Body.String() != "hello from the app" {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("request waited too long")
	}
}

func TestConcurrentRequestsShareOneWake(t *testing.T) {
	host := newFakeHost(t, nil)
	p := newTestProxy(t, host, UpstreamConfig{Name: "api", Audience: "private", WakePolicy: "any"})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := do(p.ups["api"], "GET", "/", "application/json", "100.64.0.2"); w.Code != 200 {
				t.Errorf("got %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if n := host.packets.Load(); n != 1 {
		t.Fatalf("%d wakes for one burst, want 1", n)
	}
}

func TestPublicWakeLimits(t *testing.T) {
	host := newFakeHost(t, nil)
	p := newTestProxy(t, host, UpstreamConfig{Name: "app"})
	u := p.ups["app"]
	u.limiter.n = 1
	p.bridge.maxPublicWakes = 2
	wake := func(ip string) int {
		host.awake.Store(false)
		p.m.Probe(context.Background())
		return do(u, "POST", "/__gpu_wake/wake", "application/json", ip).Code
	}
	if c := wake("203.0.113.1"); c != 202 {
		t.Fatalf("first wake: %d", c)
	}
	waitFor(t, func() bool { return p.m.State() == stateAwake })
	if c := wake("203.0.113.1"); c != 429 {
		t.Fatalf("second wake from the same IP: %d, want 429", c)
	}
	if c := wake("203.0.113.2"); c != 202 {
		t.Fatalf("wake from another IP: %d", c)
	}
	waitFor(t, func() bool { return p.m.State() == stateAwake })
	if c := wake("203.0.113.3"); c != 503 {
		t.Fatalf("wake past the breaker: %d, want 503", c)
	}
	if !p.bridge.Snapshot()["breaker_tripped"].(bool) {
		t.Fatal("breaker not tripped")
	}
}

func TestKillSwitch(t *testing.T) {
	host := newFakeHost(t, nil)
	host.awake.Store(true)
	p := newTestProxy(t, host, UpstreamConfig{Name: "site"},
		UpstreamConfig{Name: "lan", Audience: "private", WakePolicy: "any"})
	if err := p.bridge.Set("public-off", "maintenance"); err != nil {
		t.Fatal(err)
	}
	if w := do(p.ups["site"], "GET", "/", browser, "203.0.113.1"); w.Code != 503 ||
		!strings.Contains(w.Body.String(), "Offline") {
		t.Fatalf("public during public-off: %d", w.Code)
	}
	if w := do(p.ups["lan"], "GET", "/", "", "100.64.0.2"); w.Code != 200 {
		t.Fatalf("private during public-off: %d", w.Code)
	}
	_ = p.bridge.Set("off", "")
	before := host.hits.Load()
	if w := do(p.ups["lan"], "GET", "/", "", "100.64.0.2"); w.Code != 503 {
		t.Fatalf("private during off: %d", w.Code)
	}
	if host.hits.Load() != before {
		t.Fatal("kill switch off still contacted the host")
	}
}

func TestPauseShowsBusy(t *testing.T) {
	host := newFakeHost(t, nil)
	host.awake.Store(true)
	p := newTestProxy(t, host, UpstreamConfig{Name: "app"})
	form := strings.NewReader("names=app&paused=true&reason=training")
	r := httptest.NewRequest("POST", "/pause", form)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	p.admin.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("pause: %d", w.Code)
	}
	w = do(p.ups["app"], "GET", "/", "application/json", "203.0.113.1")
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 503 || body["error"] != "gpu in use" || body["reason"] != "training" {
		t.Fatalf("got %d %v", w.Code, body)
	}
	if w := do(p.ups["app"], "GET", "/", browser, "203.0.113.1"); !strings.Contains(w.Body.String(), "GPU in use") {
		t.Fatal("no busy page")
	}
}

func TestMaxInflight(t *testing.T) {
	release := make(chan struct{})
	host := newFakeHost(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-release
		}
	})
	host.awake.Store(true)
	p := newTestProxy(t, host, UpstreamConfig{Name: "app", MaxInflight: 1})
	done := make(chan int)
	go func() { done <- do(p.ups["app"], "GET", "/slow", "", "203.0.113.1").Code }()
	waitFor(t, func() bool { return len(p.ups["app"].inflight) == 1 })
	if w := do(p.ups["app"], "GET", "/", "", "203.0.113.2"); w.Code != 503 {
		t.Fatalf("second request: %d, want 503", w.Code)
	}
	close(release)
	if c := <-done; c != 200 {
		t.Fatalf("first request: %d", c)
	}
}

func TestResponsesStream(t *testing.T) {
	host := newFakeHost(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: 1\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "data: 2\n\n")
	})
	host.awake.Store(true)
	p := newTestProxy(t, host, UpstreamConfig{Name: "app"})
	srv := httptest.NewServer(p.ups["app"])
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	start := time.Now()
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "data: 1\n\n" || time.Since(start) > 150*time.Millisecond {
		t.Fatalf("first event %q after %v: responses are buffered", buf[:n], time.Since(start))
	}
}

func TestBridgePersistsAndFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.json")
	b := loadBridge(path, 6)
	if err := b.Set("off", "holiday"); err != nil {
		t.Fatal(err)
	}
	if got := loadBridge(path, 6); got.Mode != "off" || got.Reason != "holiday" {
		t.Fatalf("after restart: %s %q", got.Mode, got.Reason)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadBridge(path, 6); got.Mode != "public-off" {
		t.Fatalf("unreadable state: %s, want public-off", got.Mode)
	}
	if loadBridge(filepath.Join(t.TempDir(), "missing.json"), 6).Mode != "on" {
		t.Fatal("first start should be on")
	}
}

func TestMagicPacket(t *testing.T) {
	mac := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}
	pkt := magicPacket(mac)
	if len(pkt) != 102 {
		t.Fatalf("length %d", len(pkt))
	}
	for i := range 6 {
		if pkt[i] != 0xff {
			t.Fatal("missing sync stream")
		}
	}
	for i := 6; i < len(pkt); i += 6 {
		if string(pkt[i:i+6]) != string(mac) {
			t.Fatalf("repetition at %d", i)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
