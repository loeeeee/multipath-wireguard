package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"
)

type client struct {
	inner  *net.UDPConn
	routes []*net.UDPConn
}

func newClient(listenStr string, routeAddrs []*net.UDPAddr) (*client, error) {
	listenAddr, err := net.ResolveUDPAddr("udp", listenStr)
	if err != nil {
		return nil, fmt.Errorf("resolve listen address %q: %w", listenStr, err)
	}

	inner, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("bind inner socket %s: %w", listenAddr, err)
	}

	var routes []*net.UDPConn
	for _, routeAddr := range routeAddrs {
		conn, err := net.DialUDP("udp", nil, routeAddr)
		if err != nil {
			for _, rc := range routes {
				rc.Close()
			}
			inner.Close()
			return nil, fmt.Errorf("dial route %s: %w", routeAddr, err)
		}
		routes = append(routes, conn)
	}

	return &client{inner: inner, routes: routes}, nil
}

func (c *client) InnerAddr() *net.UDPAddr {
	return c.inner.LocalAddr().(*net.UDPAddr)
}

func (c *client) close() {
	for _, rc := range c.routes {
		rc.Close()
	}
	c.inner.Close()
}

func (c *client) run(ctx context.Context, logInterval time.Duration) {
	var wgPeer atomic.Pointer[net.UDPAddr]

	counters := make([]*counter, len(c.routes))
	for i := range c.routes {
		counters[i] = &counter{}
	}

	for i, rs := range c.routes {
		go clientRouteReader(ctx, rs, c.inner, &wgPeer, counters[i])
	}
	go clientInnerReader(ctx, c.inner, c.routes, &wgPeer, counters)
	go clientCounterLogger(ctx, c.routes, counters, logInterval)

	<-ctx.Done()
	slog.Info("client shutting down")
}

func clientInnerReader(ctx context.Context, inner *net.UDPConn, routes []*net.UDPConn, wgPeer *atomic.Pointer[net.UDPAddr], counters []*counter) {
	buf := make([]byte, maxPacket)
	rlim := newRateLimiter(5 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, ap, err := inner.ReadFromUDPAddrPort(buf)
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
		current := wgPeer.Load()
		if current == nil || current.AddrPort() != ap {
			wgPeer.Store(net.UDPAddrFromAddrPort(ap))
		}

		for i, rs := range routes {
			_, err := rs.Write(buf[:n])
			if err != nil {
				if rlim.allow() {
					slog.Warn("route write error", "route", rs.RemoteAddr(), "err", err)
				}
			} else {
				counters[i].recordTx(n)
			}
		}
	}
}

func clientRouteReader(ctx context.Context, conn *net.UDPConn, inner *net.UDPConn, wgPeer *atomic.Pointer[net.UDPAddr], cnt *counter) {
	buf := make([]byte, maxPacket)
	rlim := newRateLimiter(5 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := conn.Read(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if rlim.allow() {
				slog.Warn("route read error", "route", conn.RemoteAddr(), "err", err)
			}
			continue
		}

		cnt.recordRx(n)

		peer := wgPeer.Load()
		if peer == nil {
			continue
		}

		_, err = inner.WriteToUDP(buf[:n], peer)
		if err != nil {
			if rlim.allow() {
				slog.Warn("inner write error", "err", err)
			}
		}
	}
}

func clientCounterLogger(ctx context.Context, routes []*net.UDPConn, counters []*counter, logInterval time.Duration) {
	ticker := time.NewTicker(logInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i, cnt := range counters {
				rxPkts, rxBytes, txPkts, txBytes, lastSeen := cnt.snapshot()
				slog.Info("route stats",
					"route", routes[i].RemoteAddr(),
					"rx_packets", rxPkts,
					"rx_bytes", rxBytes,
					"tx_packets", txPkts,
					"tx_bytes", txBytes,
					"last_seen", lastSeen,
				)
			}
		}
	}
}
