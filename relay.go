package main

import (
	"sync/atomic"
	"time"
)

const maxPacket = 1500

type counter struct {
	rxPackets atomic.Int64
	rxBytes   atomic.Int64
	txPackets atomic.Int64
	txBytes   atomic.Int64
	lastSeen  atomic.Int64
}

func (c *counter) recordRx(n int) {
	c.rxPackets.Add(1)
	c.rxBytes.Add(int64(n))
	c.lastSeen.Store(time.Now().UnixNano())
}

func (c *counter) recordTx(n int) {
	c.txPackets.Add(1)
	c.txBytes.Add(int64(n))
}

func (c *counter) snapshot() (rxPkts, rxBytes, txPkts, txBytes int64, lastSeen time.Time) {
	return c.rxPackets.Load(), c.rxBytes.Load(), c.txPackets.Load(), c.txBytes.Load(),
		time.Unix(0, c.lastSeen.Load())
}

type rateLimiter struct {
	interval time.Duration
	last     atomic.Int64
}

func newRateLimiter(interval time.Duration) *rateLimiter {
	return &rateLimiter{interval: interval}
}

func (r *rateLimiter) allow() bool {
	now := time.Now().UnixNano()
	last := r.last.Load()
	if now-last < int64(r.interval) {
		return false
	}
	r.last.Store(now)
	return true
}
