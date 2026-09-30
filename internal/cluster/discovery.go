package cluster

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"sync"
	"time"
)

// LAN discovery: running sites broadcast a small UDP beacon; a new site
// with nothing configured listens for it and asks to join.
const (
	DiscoveryPort  = 7420
	beaconInterval = 3 * time.Second
	beaconMaxAge   = 15 * time.Second
)

type Beacon struct {
	V       int    `json:"v"`
	Cluster string `json:"cluster"`
	ID      string `json:"id"`
	URL     string `json:"url"`
	Offset  int64  `json:"offset"`
}

// Broadcast sends b on every IPv4 interface's broadcast address until ctx ends.
func Broadcast(ctx context.Context, b Beacon, log *slog.Logger) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		log.Warn("discovery disabled", "err", err)
		return
	}
	defer conn.Close()
	b.V = 1
	payload, _ := json.Marshal(b)
	t := time.NewTicker(beaconInterval)
	defer t.Stop()
	for {
		for _, addr := range broadcastAddrs() {
			conn.WriteToUDP(payload, &net.UDPAddr{IP: addr, Port: DiscoveryPort})
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func broadcastAddrs() []net.IP {
	var out []net.IP
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil {
				continue
			}
			ip, mask := n.IP.To4(), n.Mask
			if len(mask) == 16 {
				mask = mask[12:]
			}
			b := make(net.IP, 4)
			for i := range 4 {
				b[i] = ip[i] | ^mask[i]
			}
			out = append(out, b)
		}
	}
	return out
}

// Listener remembers recently heard beacons.
type Listener struct {
	mu   sync.Mutex
	seen map[string]seenBeacon
}

type seenBeacon struct {
	Beacon
	at time.Time
}

func Listen(ctx context.Context, log *slog.Logger) *Listener {
	l := &Listener{seen: map[string]seenBeacon{}}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: DiscoveryPort})
	if err != nil {
		log.Warn("LAN discovery unavailable", "err", err)
		return l
	}
	go func() { <-ctx.Done(); conn.Close() }()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			var b Beacon
			if json.Unmarshal(buf[:n], &b) != nil || b.V != 1 || b.URL == "" {
				continue
			}
			l.mu.Lock()
			l.seen[b.URL] = seenBeacon{b, time.Now()}
			l.mu.Unlock()
		}
	}()
	return l
}

// Best returns the recently seen site with the lowest id offset (normally
// the founder), so every new site asks the same place.
func (l *Listener) Best() (Beacon, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var best Beacon
	found := false
	for _, s := range l.seen {
		if time.Since(s.at) > beaconMaxAge {
			continue
		}
		if !found || s.Offset < best.Offset {
			best, found = s.Beacon, true
		}
	}
	return best, found
}
