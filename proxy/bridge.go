package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Bridge is the kill switch between the proxy and the GPU host, plus a breaker that stops
// public traffic from waking the host too often.
//
//	on          everything is forwarded
//	public-off  public upstreams get an "offline" page without touching the host; private works
//	off         nothing is forwarded and nothing wakes the host
//
// The mode is persisted, so a reboot of the proxy machine keeps the bridge off.
type Bridge struct {
	path           string
	maxPublicWakes int

	mu          sync.Mutex
	Mode        string    `json:"mode"`
	Reason      string    `json:"reason"`
	Changed     time.Time `json:"changed"`
	publicWakes []time.Time
	trippedAt   time.Time
}

var bridgeModes = map[string]bool{"on": true, "public-off": true, "off": true}

func loadBridge(path string, maxPublicWakes int) *Bridge {
	b := &Bridge{path: path, maxPublicWakes: maxPublicWakes, Mode: "on"}
	if data, err := os.ReadFile(path); err == nil {
		var saved Bridge
		if json.Unmarshal(data, &saved) == nil && bridgeModes[saved.Mode] {
			b.Mode, b.Reason, b.Changed = saved.Mode, saved.Reason, saved.Changed
		} else {
			// Unreadable state fails closed for public traffic.
			b.Mode, b.Reason = "public-off", "unreadable state file"
		}
	}
	log.Printf("bridge: %s%s", b.Mode, suffix(b.Reason))
	return b
}

func suffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

func (b *Bridge) Set(mode, reason string) error {
	if !bridgeModes[mode] {
		return fmt.Errorf("mode must be on, public-off or off")
	}
	b.mu.Lock()
	b.Mode, b.Reason, b.Changed = mode, reason, time.Now()
	if mode == "on" {
		b.publicWakes, b.trippedAt = nil, time.Time{}
	}
	data, _ := json.Marshal(b)
	b.mu.Unlock()
	log.Printf("bridge: set %s%s", mode, suffix(reason))
	if b.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}

// Allows reports whether requests from this audience may reach the host at all.
func (b *Bridge) Allows(audience string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.Mode {
	case "on":
		return true
	case "public-off":
		return audience == audiencePrivate
	default:
		return false
	}
}

// AllowPublicWake counts a wake started by public traffic. Beyond maxPublicWakes in an
// hour the breaker trips: public visitors can still use an awake host but cannot wake it.
func (b *Bridge) AllowPublicWake() bool {
	if b.maxPublicWakes <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cut := time.Now().Add(-time.Hour)
	kept := b.publicWakes[:0]
	for _, t := range b.publicWakes {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	b.publicWakes = kept
	if len(kept) >= b.maxPublicWakes {
		if b.trippedAt.IsZero() {
			b.trippedAt = time.Now()
			log.Printf("bridge: breaker tripped, %d public wakes in the last hour", len(kept))
		}
		return false
	}
	if !b.trippedAt.IsZero() {
		log.Printf("bridge: breaker reset (window cleared)")
	}
	b.trippedAt = time.Time{}
	b.publicWakes = append(b.publicWakes, time.Now())
	return true
}

func (b *Bridge) ResetBreaker() {
	b.mu.Lock()
	b.publicWakes, b.trippedAt = nil, time.Time{}
	b.mu.Unlock()
	log.Printf("bridge: breaker reset by admin")
}

func (b *Bridge) Snapshot() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{
		"mode": b.Mode, "reason": b.Reason, "changed": nullTime(b.Changed),
		"public_wakes_last_hour": len(b.publicWakes), "public_wakes_per_hour": b.maxPublicWakes,
		"breaker_tripped": !b.trippedAt.IsZero(), "breaker_tripped_at": nullTime(b.trippedAt),
	}
}
