package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestReleaseGPU(t *testing.T) {
	t.Setenv("GPU_CONTROL_TOKEN", "private-token")
	calls, upstreamStatus := 0, http.StatusOK
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/api/gpu/release" || r.Host != "localhost" {
			t.Errorf("unexpected release request: %s %s host=%s", r.Method, r.URL, r.Host)
		}
		if r.Header.Get("Authorization") != "Bearer private-token" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing internal authorization/JSON headers")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "{}" {
			t.Errorf("unexpected body %q", body)
		}
		w.WriteHeader(upstreamStatus)
		_, _ = w.Write([]byte(`{"detail":"model busy"}`))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	br := &Bridge{Mode: "on"}
	a := &admin{m: &Machine{cfg: &Config{}, state: "asleep"}, bridge: br, ups: []*Upstream{{
		cfg:    UpstreamConfig{Name: "app", ReleasePath: "/api/gpu/release"},
		target: target, client: upstream.Client(),
	}}}
	handler := sameOrigin(a.routes())
	request := func(name, site, accept string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/release", strings.NewReader("name="+url.QueryEscape(name)))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", site)
		r.Header.Set("Accept", accept)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("app", "cross-site", "application/json"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := request("unknown", "same-origin", "application/json"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	br.Mode = "off"
	if w := request("app", "same-origin", "application/json"); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if calls != 0 {
		t.Fatal("refused requests contacted the host")
	}
	br.Mode = "on"
	if w := request("app", "same-origin", "application/json"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if a.m.State() != "asleep" {
		t.Fatal("release triggered a wake")
	}
	upstreamStatus = 409
	if w := request("app", "same-origin", "application/json"); w.Code != 409 || !strings.Contains(w.Body.String(), "model busy") {
		t.Fatal(w.Code, w.Body.String())
	}
	upstreamStatus = 500
	if w := request("app", "same-origin", "application/json"); w.Code != 502 {
		t.Fatal(w.Code)
	}
	upstreamStatus = 200
	if w := request("app", "same-origin", "text/html"); w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatal(w.Code)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/release", nil))
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
}
