package main

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestNewServerSuccess(t *testing.T) {
	s, err := newServer("127.0.0.1:0", "127.0.0.1:1")
	if err != nil {
		t.Fatalf("newServer failed: %v", err)
	}
	defer s.close()
	if s.outer == nil || s.target == nil || s.table == nil {
		t.Fatal("server fields should not be nil")
	}
}

func TestNewServerInvalidListen(t *testing.T) {
	_, err := newServer("not-a-valid-address", "127.0.0.1:1")
	if err == nil {
		t.Fatal("expected error for invalid listen address")
	}
}

func TestNewServerInvalidTarget(t *testing.T) {
	_, err := newServer("127.0.0.1:0", "not-a-valid-address")
	if err == nil {
		t.Fatal("expected error for invalid target address")
	}
}

func TestServerOuterAddr(t *testing.T) {
	s, err := newServer("127.0.0.1:0", "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	addr := s.OuterAddr()
	if addr.Port == 0 {
		t.Fatal("OuterAddr should have a valid port")
	}
	if !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("unexpected IP: %v", addr.IP)
	}
}

func TestServerClose(t *testing.T) {
	s, err := newServer("127.0.0.1:0", "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	s.close()
	s.close()
}

func TestServerRunAndCancel(t *testing.T) {
	echoConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echoConn.Close()
	go runEcho(echoConn, nil)

	s, err := newServer("127.0.0.1:0", echoConn.LocalAddr().(*net.UDPAddr).String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.run(ctx, 30*time.Second, 30*time.Second)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server run did not return after cancel")
	}
}

func TestServerRoundTrip(t *testing.T) {
	// Stand up echo as target, server, then send through outer
	echoConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echoConn.Close()
	go runEcho(echoConn, nil)
	echoAddr := echoConn.LocalAddr().(*net.UDPAddr)

	s, err := newServer("127.0.0.1:0", echoAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.run(ctx, 30*time.Second, 30*time.Second)
	time.Sleep(100 * time.Millisecond)

	// Simulate a tunnel client connecting to the server outer addr
	clientConn, err := net.DialUDP("udp", nil, s.OuterAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	payload := []byte("hello-server")
	_, err = clientConn.Write(payload)
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, maxPacket)
	clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("got %q, want %q", buf[:n], payload)
	}

	if s.RouteCount() != 1 {
		t.Fatalf("route count: got %d, want 1", s.RouteCount())
	}
}

func TestServerRouteLearning(t *testing.T) {
	// Verify the server learns routes from multiple sources
	echoConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echoConn.Close()
	go runEcho(echoConn, nil)

	s, err := newServer("127.0.0.1:0", echoConn.LocalAddr().(*net.UDPAddr).String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.run(ctx, 30*time.Second, 30*time.Second)
	time.Sleep(100 * time.Millisecond)

	outerAddr := s.OuterAddr()

	// Two independent tunnel clients
	c1, err := net.DialUDP("udp", nil, outerAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := net.DialUDP("udp", nil, outerAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	c1.Write([]byte("from-c1"))
	c2.Write([]byte("from-c2"))

	buf := make([]byte, maxPacket)
	replies := make(map[string]bool)
	c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	c2.SetReadDeadline(time.Now().Add(2 * time.Second))

	for i := 0; i < 2; i++ {
		n1, err1 := c1.Read(buf)
		if err1 == nil {
			replies[string(buf[:n1])] = true
		}
		n2, err2 := c2.Read(buf)
		if err2 == nil {
			replies[string(buf[:n2])] = true
		}
	}

	if len(replies) < 2 {
		t.Fatalf("got %d unique replies, want 2", len(replies))
	}

	if s.RouteCount() != 2 {
		t.Fatalf("route count: got %d, want 2", s.RouteCount())
	}
}

func TestServerFanOutToAllRoutes(t *testing.T) {
	// Verify the server fans out replies to all learned routes
	echoConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echoConn.Close()
	go runEcho(echoConn, nil)

	s, err := newServer("127.0.0.1:0", echoConn.LocalAddr().(*net.UDPAddr).String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.run(ctx, 30*time.Second, 30*time.Second)
	time.Sleep(100 * time.Millisecond)

	outerAddr := s.OuterAddr()

	c1, _ := net.DialUDP("udp", nil, outerAddr)
	defer c1.Close()
	c2, _ := net.DialUDP("udp", nil, outerAddr)
	defer c2.Close()

	// Both clients send to learn their routes
	c1.Write([]byte("learn-c1"))
	c2.Write([]byte("learn-c2"))
	time.Sleep(300 * time.Millisecond)

	// Drain ALL stale replies from each client independently
	drainBuf := make([]byte, maxPacket)
	c1.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		if _, err := c1.Read(drainBuf); err != nil {
			break
		}
	}
	c2.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		if _, err := c2.Read(drainBuf); err != nil {
			break
		}
	}

	// Now c1 sends a fresh packet; both c1 and c2 should receive it
	c1.Write([]byte("fanout-test"))

	c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	c1buf := make([]byte, maxPacket)
	c2buf := make([]byte, maxPacket)
	n1, err1 := c1.Read(c1buf)
	n2, err2 := c2.Read(c2buf)

	if err1 != nil {
		t.Fatalf("c1 should receive reply: %v", err1)
	}
	if string(c1buf[:n1]) != "fanout-test" {
		t.Fatalf("c1 got %q, want %q", c1buf[:n1], "fanout-test")
	}
	if err2 != nil {
		t.Fatalf("c2 should receive reply (fan-out): %v", err2)
	}
	if string(c2buf[:n2]) != "fanout-test" {
		t.Fatalf("c2 got %q, want %q", c2buf[:n2], "fanout-test")
	}
}

func TestServerEmptyRouteTableDrop(t *testing.T) {
	// When the route table is empty, the inner reader should drop packets
	// silently. Write directly to s.target to trigger echo -> s.target reply
	// without any route being learned first.
	echoConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echoConn.Close()
	echoAddr := echoConn.LocalAddr().(*net.UDPAddr)
	go runEcho(echoConn, nil)

	s, err := newServer("127.0.0.1:0", echoAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.run(ctx, 30*time.Second, 30*time.Second)
	time.Sleep(100 * time.Millisecond)

	// Write directly to s.target. The echo will reply back to s.target's
	// local address, which the server inner reader picks up. The route
	// table is empty at this point, so the inner reader must drop cleanly.
	_, err = s.target.Write([]byte("orphan-reply"))
	if err != nil {
		t.Fatal(err)
	}

	// Wait for echo round-trip and inner reader processing
	time.Sleep(200 * time.Millisecond)
	if s.RouteCount() != 0 {
		t.Fatalf("route count: got %d, want 0", s.RouteCount())
	}
	// No crash, no error, route table still empty
}

func TestRouteTableMaxEntries(t *testing.T) {
	rt := newRouteTable()

	for i := 0; i < maxRouteEntries+100; i++ {
		addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: i + 1}
		rt.upsert(addr, 10)
	}

	if rt.count() != maxRouteEntries {
		t.Fatalf("count: got %d, want %d", rt.count(), maxRouteEntries)
	}
}

func TestRouteTableSnapAndReset(t *testing.T) {
	rt := newRouteTable()
	addr1 := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1111}
	addr2 := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}

	rt.upsert(addr1, 100)
	rt.upsert(addr1, 50)
	rt.upsert(addr2, 200)

	entries := rt.snapAndReset()
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	for _, e := range entries {
		switch e.addr.Port {
		case 1111:
			if e.rxPkts != 2 || e.rxBytes != 150 {
				t.Fatalf("entry 1111: rxPkts=%d rxBytes=%d, want 2/150", e.rxPkts, e.rxBytes)
			}
		case 2222:
			if e.rxPkts != 1 || e.rxBytes != 200 {
				t.Fatalf("entry 2222: rxPkts=%d rxBytes=%d, want 1/200", e.rxPkts, e.rxBytes)
			}
		}
	}
}

func TestRouteTableEmptySnapshot(t *testing.T) {
	rt := newRouteTable()

	snap := rt.snapshot()
	if len(snap) != 0 {
		t.Fatalf("empty snapshot: got %d entries, want 0", len(snap))
	}

	snapReset := rt.snapAndReset()
	if len(snapReset) != 0 {
		t.Fatalf("empty snapAndReset: got %d entries, want 0", len(snapReset))
	}
}

func TestRouteTableSnapshotReuse(t *testing.T) {
	rt := newRouteTable()
	addr1 := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1111}
	rt.upsert(addr1, 100)

	entries1 := rt.snapshot()
	if len(entries1) != 1 {
		t.Fatal("first snapshot should have 1 entry")
	}

	entries2 := rt.snapshot()
	if len(entries2) != 1 {
		t.Fatal("second snapshot should have 1 entry")
	}

	if entries1[0] != entries2[0] {
		t.Fatal("snapshot should return same entry pointer across calls")
	}
}

func TestRouteTableConcurrentOps(t *testing.T) {
	rt := newRouteTable()
	addr1 := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1111}
	addr2 := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}

	rt.upsert(addr1, 10)
	rt.upsert(addr2, 20)

	done := make(chan struct{}, 4)

	go func() {
		for i := 0; i < 500; i++ {
			rt.upsert(addr1, 5)
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 500; i++ {
			_ = rt.snapshot()
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 500; i++ {
			rt.prune(1 * time.Hour)
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 500; i++ {
			_ = rt.snapAndReset()
		}
		done <- struct{}{}
	}()

	for range 4 {
		<-done
	}
	// No deadlock, no panic, no data race
}

func TestRouteTableConcurrentUpsertMany(t *testing.T) {
	rt := newRouteTable()
	done := make(chan struct{}, 50)

	for id := 0; id < 50; id++ {
		go func(base int) {
			for j := 0; j < 200; j++ {
				addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, byte(base)), Port: j + 1}
				rt.upsert(addr, 10)
			}
			done <- struct{}{}
		}(id)
	}

	for range 50 {
		<-done
	}

	count := rt.count()
	if count == 0 || count > maxRouteEntries {
		t.Fatalf("unexpected count: %d", count)
	}
}

func TestRouteTableConcurrentPruneAndUpsert(t *testing.T) {
	rt := newRouteTable()
	done := make(chan struct{}, 2)

	go func() {
		for i := 0; i < 200; i++ {
			addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: i + 1}
			rt.upsert(addr, 10)
		}
		done <- struct{}{}
	}()

	go func() {
		for i := 0; i < 200; i++ {
			rt.prune(0 * time.Second)
		}
		done <- struct{}{}
	}()

	for range 2 {
		<-done
	}
	// No deadlock or panic
}

func TestRateLimiterConcurrent(t *testing.T) {
	rl := newRateLimiter(1 * time.Millisecond)
	done := make(chan struct{}, 10)

	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 1000; j++ {
				rl.allow()
			}
			done <- struct{}{}
		}()
	}

	for range 10 {
		<-done
	}
}
