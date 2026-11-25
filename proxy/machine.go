package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os/exec"
	"sync"
	"time"
)

// Machine tracks whether the GPU host is asleep, waking or awake, and runs wakes.
//
// State comes from ICMP ping only. Unlike a TCP probe, a ping opens no connection that the
// host's idle check would count, so watching the host never keeps it awake.
type Machine struct {
	cfg *Config
	// Swappable for tests.
	ping func(ctx context.Context) bool
	send func(HostConfig) error

	mu          sync.Mutex
	state       string // unknown | asleep | waking | awake
	wakeDone    chan struct{}
	wakeStarted time.Time
	wakeReason  string
	lastWake    *WakeRecord
	lastSeen    time.Time
}

type WakeRecord struct {
	Reason     string    `json:"reason"`
	Started    time.Time `json:"started"`
	ReachableS float64   `json:"reachable_after_s,omitempty"`
	Outcome    string    `json:"outcome"`
	Packets    int       `json:"packets"`
}

const (
	stateUnknown = "unknown"
	stateAsleep  = "asleep"
	stateWaking  = "waking"
	stateAwake   = "awake"
)

// recentlySeen: a host that answered a ping this recently is not woken again.
const recentlySeen = 5 * time.Second

func newMachine(cfg *Config) *Machine {
	m := &Machine{cfg: cfg, state: stateUnknown, send: sendMagic}
	m.ping = func(ctx context.Context) bool { return icmpPing(ctx, cfg.Host.IP) }
	return m
}

// icmpPing uses the system ping, which works unprivileged on most distributions
// (net.ipv4.ping_group_range).
func icmpPing(ctx context.Context, ip string) bool {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	return exec.CommandContext(ctx, "ping", "-n", "-q", "-c", "1", "-W", "1", ip).Run() == nil
}

// Probe pings the host and records the result, unless a wake is in progress.
func (m *Machine) Probe(ctx context.Context) bool {
	up := m.ping(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if up {
		m.lastSeen = time.Now()
	}
	if m.state != stateWaking {
		if up {
			m.state = stateAwake
		} else {
			m.state = stateAsleep
		}
	}
	return up
}

// Wake starts one wake attempt, or joins the one in progress. The returned channel closes
// when the host answers pings or the attempt times out.
func (m *Machine) Wake(reason string) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == stateWaking && m.wakeDone != nil {
		return m.wakeDone
	}
	done := make(chan struct{})
	// A caller that saw "asleep" just before another wake finished needs no second one.
	if m.state == stateAwake && time.Since(m.lastSeen) < recentlySeen {
		close(done)
		return done
	}
	m.state, m.wakeDone, m.wakeStarted, m.wakeReason = stateWaking, done, time.Now(), reason
	go m.runWake(done, reason)
	return done
}

func (m *Machine) runWake(done chan struct{}, reason string) {
	start := time.Now()
	rec := &WakeRecord{Reason: reason, Started: start, Outcome: "timeout"}
	deadline := start.Add(time.Duration(m.cfg.WakeTimeoutS) * time.Second)
	resend := time.Duration(m.cfg.ResendIntervalS) * time.Second
	var lastSend time.Time
	log.Printf("wake: start (%s)", reason)
	for time.Now().Before(deadline) {
		if time.Since(lastSend) >= resend {
			if err := m.send(m.cfg.Host); err != nil {
				log.Printf("wake: magic packet failed: %v", err)
			} else {
				rec.Packets++
			}
			lastSend = time.Now()
		}
		if m.ping(context.Background()) {
			rec.Outcome, rec.ReachableS = "reachable", time.Since(start).Seconds()
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	m.mu.Lock()
	if rec.Outcome == "reachable" {
		m.state, m.lastSeen = stateAwake, time.Now()
	} else {
		m.state = stateAsleep
	}
	m.lastWake, m.wakeDone = rec, nil
	m.mu.Unlock()
	log.Printf("wake: %s after %.1fs, %d packet(s)", rec.Outcome, time.Since(start).Seconds(), rec.Packets)
	close(done)
}

// KnownAsleep reports that background pings have seen no answer for a minute, so a request
// can get the waiting page at once instead of waiting for a probe to time out.
func (m *Machine) KnownAsleep() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state == stateAsleep && time.Since(m.lastSeen) > time.Minute
}

func (m *Machine) State() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Watch pings the host every interval for the status page and the state endpoint.
func (m *Machine) Watch(ctx context.Context, interval time.Duration) {
	for {
		if m.State() != stateWaking {
			m.Probe(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// magicPacket is 6 bytes of 0xff followed by the MAC address 16 times.
func magicPacket(mac net.HardwareAddr) []byte {
	pkt := bytes.Repeat([]byte{0xff}, 6)
	for range 16 {
		pkt = append(pkt, mac...)
	}
	return pkt
}

// sendMagic broadcasts a Wake-on-LAN magic packet to UDP ports 9 and 7. It leaves from the
// configured LAN interface: Wake-on-LAN needs layer-2 broadcast, so it cannot go over a VPN.
func sendMagic(h HostConfig) error {
	mac, err := net.ParseMAC(h.MAC)
	if err != nil {
		return err
	}
	var laddr *net.UDPAddr
	if h.Iface != "" {
		ifi, err := net.InterfaceByName(h.Iface)
		if err != nil {
			return err
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				laddr = &net.UDPAddr{IP: ipn.IP}
				break
			}
		}
		if laddr == nil {
			return fmt.Errorf("no IPv4 address on %s", h.Iface)
		}
	}
	conn, err := net.ListenUDP("udp4", laddr) // Go sets SO_BROADCAST on UDP sockets
	if err != nil {
		return err
	}
	defer conn.Close()
	pkt := magicPacket(mac)
	var firstErr error
	for _, port := range []string{"9", "7"} {
		ua, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(h.Broadcast, port))
		if err == nil {
			_, err = conn.WriteToUDP(pkt, ua)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
