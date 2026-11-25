package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Config is /etc/gpu-wake/proxy.json. See docs/configuration.md.
type Config struct {
	Host               HostConfig       `json:"host"`
	WakeTimeoutS       int              `json:"wake_timeout_s"`
	ResendIntervalS    int              `json:"resend_interval_s"`
	HealthyCacheS      int              `json:"healthy_cache_s"`
	WakesPerIPHour     int              `json:"wakes_per_ip_per_hour"`
	PublicWakesPerHour int              `json:"public_wakes_per_hour"`
	AdminListen        string           `json:"admin_listen"`
	StateFile          string           `json:"state_file"`
	Pages              PagesConfig      `json:"pages"`
	Upstreams          []UpstreamConfig `json:"upstreams"`
}

// HostConfig identifies the GPU machine on the LAN.
type HostConfig struct {
	MAC       string `json:"mac"`
	IP        string `json:"ip"`
	Broadcast string `json:"broadcast"`
	Iface     string `json:"iface"`
}

// PagesConfig controls the waiting, busy, offline and status pages.
type PagesConfig struct {
	SiteName     string `json:"site_name"`     // optional breadcrumb: "<site> / <app>"
	SiteURL      string `json:"site_url"`      // where the site name links to
	StyleURL     string `json:"style_url"`     // optional stylesheet loaded after the built-in theme
	TemplatesDir string `json:"templates_dir"` // optional *.html overriding built-in templates by name
}

type UpstreamConfig struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Listen string `json:"listen"`
	Target string `json:"target"`
	Health string `json:"health"`
	// "explicit" (default): only the waiting page's script or button, or a non-GET request,
	// wakes the host, so crawlers and link previews never do. "any": every request may wake
	// it; allowed only for private upstreams.
	WakePolicy string `json:"wake_policy"`
	// "public" (default) or "private". Public upstreams are subject to the kill switch's
	// public-off mode, the per-IP wake limit and the public wake breaker.
	Audience string `json:"audience"`
	// Concurrent forwarded requests; more get 503 at once instead of piling up on the host.
	MaxInflight int `json:"max_inflight"`
	// Optional app endpoint that releases the app's GPU memory, reachable only through the
	// admin listener.
	ReleasePath string `json:"release_path,omitempty"`
}

const (
	audiencePublic  = "public"
	audiencePrivate = "private"
)

func defaultConfig() *Config {
	return &Config{WakeTimeoutS: 150, ResendIntervalS: 5, HealthyCacheS: 10, WakesPerIPHour: 6,
		PublicWakesPerHour: 6, StateFile: "/var/lib/gpu-wake/bridge.json"}
}

// legacyConfig holds keys from earlier versions, still accepted so existing installs keep working.
type legacyConfig struct {
	PC       *HostConfig `json:"pc"`
	StyleURL string      `json:"style_url"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseConfig(b, path)
}

func parseConfig(b []byte, name string) (*Config, error) {
	var both struct {
		*Config
		legacyConfig
	}
	both.Config = defaultConfig()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&both); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	c := both.Config
	if both.PC != nil {
		if c.Host != (HostConfig{}) {
			return nil, fmt.Errorf("%s: use host, not both host and pc", name)
		}
		c.Host = *both.PC
	}
	if both.StyleURL != "" && c.Pages.StyleURL == "" {
		c.Pages.StyleURL = both.StyleURL
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return c, nil
}

func (c *Config) validate() error {
	if _, err := net.ParseMAC(c.Host.MAC); err != nil {
		return fmt.Errorf("host.mac: %w", err)
	}
	if net.ParseIP(c.Host.IP) == nil {
		return errors.New("host.ip must be an IP address")
	}
	if net.ParseIP(c.Host.Broadcast).To4() == nil {
		return errors.New("host.broadcast must be an IPv4 address, such as 192.168.1.255")
	}
	if c.WakeTimeoutS <= 0 || c.ResendIntervalS <= 0 || c.HealthyCacheS < 0 {
		return errors.New("wake_timeout_s and resend_interval_s must be positive")
	}
	if c.AdminListen != "" && !isLoopback(c.AdminListen) {
		return fmt.Errorf("admin_listen %q must be a loopback address", c.AdminListen)
	}
	for _, s := range []struct{ key, value string }{
		{"pages.site_url", c.Pages.SiteURL}, {"pages.style_url", c.Pages.StyleURL}} {
		if s.value != "" && !isHTTPURL(s.value) {
			return fmt.Errorf("%s must be an http(s) URL", s.key)
		}
	}
	if len(c.Upstreams) == 0 {
		return errors.New("at least one upstream is required")
	}
	names, listens := map[string]bool{}, map[string]bool{}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.Name == "" || u.Listen == "" || u.Target == "" {
			return fmt.Errorf("upstream %d: name, listen and target are required", i)
		}
		if names[u.Name] || listens[u.Listen] {
			return fmt.Errorf("upstream %s: duplicate name or listen address", u.Name)
		}
		names[u.Name], listens[u.Listen] = true, true
		if !isLoopback(u.Listen) {
			return fmt.Errorf("upstream %s: listen %q must be a loopback address", u.Name, u.Listen)
		}
		if !isHTTPURL(u.Target) {
			return fmt.Errorf("upstream %s: target must be an http(s) URL", u.Name)
		}
		if u.Health == "" {
			u.Health = "/"
		}
		if !isPath(u.Health) {
			return fmt.Errorf("upstream %s: health must be an absolute path", u.Name)
		}
		if u.ReleasePath != "" && !isPath(u.ReleasePath) {
			return fmt.Errorf("upstream %s: release_path must be an absolute path without query or fragment", u.Name)
		}
		switch u.Audience {
		case "":
			u.Audience = audiencePublic
		case "tailnet": // earlier name
			u.Audience = audiencePrivate
		case audiencePublic, audiencePrivate:
		default:
			return fmt.Errorf("upstream %s: audience must be public or private", u.Name)
		}
		switch u.WakePolicy {
		case "":
			u.WakePolicy = "explicit"
		case "any", "explicit":
		default:
			return fmt.Errorf("upstream %s: wake_policy must be explicit or any", u.Name)
		}
		if u.WakePolicy == "any" && u.Audience != audiencePrivate {
			return fmt.Errorf("upstream %s: wake_policy any is only allowed for private upstreams", u.Name)
		}
		if u.MaxInflight <= 0 {
			u.MaxInflight = 32
		}
	}
	return nil
}

// Listeners are loopback-only by design: exposure is decided by the front proxy (nginx, Caddy,
// a tunnel, tailscale serve), never by binding this proxy to a public interface.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func isPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.ContainsAny(p, "?#")
}
