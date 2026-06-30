package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
)

const maxRouteEntries = 10_000

type routeEntry struct {
	addr    *net.UDPAddr
	counter counter
}

type routeTable struct {
	mu      sync.RWMutex
	entries map[netip.AddrPort]*routeEntry
	scratch []*routeEntry
}

func newRouteTable() *routeTable {
	return &routeTable{
		entries: make(map[netip.AddrPort]*routeEntry),
		scratch: make([]*routeEntry, 0, 16),
	}
}

func (rt *routeTable) upsert(src *net.UDPAddr, n int) {
	ap := src.AddrPort()
	rt.mu.Lock()
	entry, ok := rt.entries[ap]
	if !ok {
		if len(rt.entries) >= maxRouteEntries {
			rt.mu.Unlock()
			return
		}
		entry = &routeEntry{addr: src}
		rt.entries[ap] = entry
	}
	rt.mu.Unlock()
	entry.counter.recordRx(n)
}

func (rt *routeTable) prune(timeout time.Duration) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	now := time.Now()
	pruned := 0
	for key, entry := range rt.entries {
		lastSeen := time.Unix(0, entry.counter.lastSeen.Load())
		if now.Sub(lastSeen) > timeout {
			delete(rt.entries, key)
			pruned++
		}
	}
	return pruned
}

func (rt *routeTable) count() int {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return len(rt.entries)
}

func (rt *routeTable) snapshot() []*routeEntry {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	rt.scratch = rt.scratch[:0]
	for _, entry := range rt.entries {
		rt.scratch = append(rt.scratch, entry)
	}
	return rt.scratch
}

func (rt *routeTable) snapAndReset() []routeSnapEntry {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	entries := make([]routeSnapEntry, 0, len(rt.entries))
	for _, entry := range rt.entries {
		rxPkts, rxBytes, txPkts, txBytes, lastSeen := entry.counter.snapshot()
		entries = append(entries, routeSnapEntry{
			addr:     entry.addr,
			rxPkts:   rxPkts,
			rxBytes:  rxBytes,
			txPkts:   txPkts,
			txBytes:  txBytes,
			lastSeen: lastSeen,
		})
	}
	return entries
}

type routeSnapEntry struct {
	addr            *net.UDPAddr
	rxPkts, rxBytes int64
	txPkts, txBytes int64
	lastSeen        time.Time
}

type server struct {
	outer  *net.UDPConn
	target *net.UDPConn
	table  *routeTable
}

func newServer(listenStr, targetStr string) (*server, error) {
	listenAddr, err := net.ResolveUDPAddr("udp", listenStr)
	if err != nil {
		return nil, fmt.Errorf("resolve listen address %q: %w", listenStr, err)
	}
	targetAddr, err := net.ResolveUDPAddr("udp", targetStr)
	if err != nil {
		return nil, fmt.Errorf("resolve target address %q: %w", targetStr, err)
	}

	outer, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("bind outer socket %s: %w", listenAddr, err)
	}

	target, err := net.DialUDP("udp", nil, targetAddr)
	if err != nil {
		outer.Close()
		return nil, fmt.Errorf("connect to target %s: %w", targetAddr, err)
	}

	return &server{
		outer:  outer,
		target: target,
		table:  newRouteTable(),
	}, nil
}

func (s *server) OuterAddr() *net.UDPAddr {
	return s.outer.LocalAddr().(*net.UDPAddr)
}

func (s *server) RouteCount() int {
	return s.table.count()
}

func (s *server) close() {
	s.target.Close()
	s.outer.Close()
}

func (s *server) run(ctx context.Context, clientTimeout, logInterval time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	fatalErr := make(chan error, 1)

	go serverOuterReader(ctx, s, cancel, fatalErr)
	go serverInnerReader(ctx, s)
	go serverJanitor(ctx, s, clientTimeout)
	go serverCounterLogger(ctx, s, logInterval)

	<-ctx.Done()
	slog.Info("server shutting down")

	select {
	case err := <-fatalErr:
		return err
	default:
		return nil
	}
}

func serverOuterReader(ctx context.Context, s *server, fatalCancel context.CancelFunc, fatalErr chan<- error) {
	buf := make([]byte, maxPacket)
	rlim := newRateLimiter(5 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, ap, err := s.outer.ReadFromUDPAddrPort(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if rlim.allow() {
				slog.Warn("outer read error", "err", err)
			}
			continue
		}

		src := net.UDPAddrFromAddrPort(ap)
		s.table.upsert(src, n)

		_, err = s.target.Write(buf[:n])
		if err != nil {
			slog.Error("target write failed, WireGuard unreachable", "err", err)
			fatalErr <- err
			fatalCancel()
			return
		}
	}
}

func serverInnerReader(ctx context.Context, s *server) {
	buf := make([]byte, maxPacket)
	rlim := newRateLimiter(5 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := s.target.Read(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if rlim.allow() {
				slog.Warn("inner read error", "err", err)
			}
			continue
		}

		entries := s.table.snapshot()
		if len(entries) == 0 {
			continue
		}

		for _, entry := range entries {
			_, err := s.outer.WriteToUDP(buf[:n], entry.addr)
			if err != nil {
				if rlim.allow() {
					slog.Warn("outer write error", "dest", entry.addr, "err", err)
				}
			} else {
				entry.counter.recordTx(n)
			}
		}
	}
}

func serverJanitor(ctx context.Context, s *server, timeout time.Duration) {
	ticker := time.NewTicker(timeout)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruned := s.table.prune(timeout)
			if pruned > 0 {
				slog.Info("pruned idle route entries", "count", pruned)
			}
		}
	}
}

func serverCounterLogger(ctx context.Context, s *server, logInterval time.Duration) {
	ticker := time.NewTicker(logInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, entry := range s.table.snapAndReset() {
				slog.Info("source stats",
					"addr", entry.addr,
					"rx_packets", entry.rxPkts,
					"rx_bytes", entry.rxBytes,
					"tx_packets", entry.txPkts,
					"tx_bytes", entry.txBytes,
					"last_seen", entry.lastSeen,
				)
			}
		}
	}
}
