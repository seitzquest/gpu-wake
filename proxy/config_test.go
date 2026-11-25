package main

import (
	"os"
	"strings"
	"testing"
)

const minimal = `{
  "host": {"mac": "00:11:22:33:44:55", "ip": "192.168.1.50", "broadcast": "192.168.1.255"},
  "upstreams": [{"name": "app", "listen": "127.0.0.1:9101", "target": "http://192.168.1.50:8000"}]
}`

func TestExampleConfigIsValid(t *testing.T) {
	for _, path := range []string{"proxy.json.example", "../examples/proxy.minimal.json"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if _, err := loadConfig(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestDefaults(t *testing.T) {
	c, err := parseConfig([]byte(minimal), "test")
	if err != nil {
		t.Fatal(err)
	}
	u := c.Upstreams[0]
	if u.Audience != "public" || u.WakePolicy != "explicit" || u.Health != "/" || u.MaxInflight != 32 {
		t.Fatalf("defaults: %+v", u)
	}
	if c.WakeTimeoutS != 150 || c.WakesPerIPHour != 6 || c.PublicWakesPerHour != 6 {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestEarlierKeysStillWork(t *testing.T) {
	old := strings.NewReplacer(`"host"`, `"pc"`,
		`"target"`, `"audience": "tailnet", "wake_policy": "any", "target"`).Replace(minimal)
	old = strings.Replace(old, `"upstreams"`, `"style_url": "https://example.org/a.css", "upstreams"`, 1)
	c, err := parseConfig([]byte(old), "test")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host.IP != "192.168.1.50" || c.Upstreams[0].Audience != "private" || c.Pages.StyleURL == "" {
		t.Fatalf("legacy config: %+v", c)
	}
}

func TestRejected(t *testing.T) {
	cases := map[string]string{
		"public any":       strings.Replace(minimal, `"target"`, `"wake_policy": "any", "target"`, 1),
		"non-loopback":     strings.Replace(minimal, "127.0.0.1:9101", "0.0.0.0:9101", 1),
		"unknown key":      strings.Replace(minimal, `"upstreams"`, `"surprise": 1, "upstreams"`, 1),
		"bad mac":          strings.Replace(minimal, "00:11:22:33:44:55", "nope", 1),
		"bad audience":     strings.Replace(minimal, `"target"`, `"audience": "everyone", "target"`, 1),
		"admin public":     strings.Replace(minimal, `"upstreams"`, `"admin_listen": "192.168.1.2:9180", "upstreams"`, 1),
		"release query":    strings.Replace(minimal, `"target"`, `"release_path": "/x?y", "target"`, 1),
		"target not http":  strings.Replace(minimal, "http://192.168.1.50:8000", "192.168.1.50:8000", 1),
		"no upstreams":     `{"host": {"mac": "00:11:22:33:44:55", "ip": "192.168.1.50", "broadcast": "192.168.1.255"}}`,
		"duplicate name":   strings.Replace(minimal, `}]`, `}, {"name": "app", "listen": "127.0.0.1:9102", "target": "http://h:1"}]`, 1),
		"both host and pc": strings.Replace(minimal, `"upstreams"`, `"pc": {"mac": "00:11:22:33:44:55"}, "upstreams"`, 1),
	}
	for name, cfg := range cases {
		if _, err := parseConfig([]byte(cfg), "test"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
