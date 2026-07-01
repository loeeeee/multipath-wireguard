package main

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestNewClientSuccess(t *testing.T) {
	c, err := newClient("127.0.0.1:0", []*net.UDPAddr{
		{IP: net.IPv4(127, 0, 0, 1), Port: 1},
	})
	if err != nil {
		t.Fatalf("newClient failed: %v", err)
	}
	defer c.close()
	if c.inner == nil {
		t.Fatal("inner should not be nil")
	}
	if len(c.routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(c.routes))
	}
}

func TestNewClientZeroRoutes(t *testing.T) {
	c, err := newClient("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("newClient with nil routes: %v", err)
	}
	defer c.close()
	if len(c.routes) != 0 {
		t.Fatalf("got %d routes, want 0", len(c.routes))
	}
}

func TestNewClientInvalidListen(t *testing.T) {
	_, err := newClient("not-a-valid-address", []*net.UDPAddr{
		{IP: net.IPv4(127, 0, 0, 1), Port: 1},
	})
	if err == nil {
		t.Fatal("expected error for invalid listen address")
	}
}

func TestNewClientMultipleRoutes(t *testing.T) {
	// Create two listeners that we can dial, to verify multiple routes work
	l1, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Close()
	l2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()

	routes := []*net.UDPAddr{
		l1.LocalAddr().(*net.UDPAddr),
		l2.LocalAddr().(*net.UDPAddr),
	}
	c, err := newClient("127.0.0.1:0", routes)
	if err != nil {
		t.Fatalf("newClient with 2 routes: %v", err)
	}
	defer c.close()
	if len(c.routes) != 2 {
		t.Fatalf("got %d routes, want 2", len(c.routes))
	}
}

func TestClientInnerAddr(t *testing.T) {
	c, err := newClient("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()

	addr := c.InnerAddr()
	if addr.Port == 0 {
		t.Fatal("InnerAddr should have a valid port")
	}
	if !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("unexpected IP: %v", addr.IP)
	}
}

func TestClientClose(t *testing.T) {
	c, err := newClient("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.close()
	// Double close must not panic
	c.close()
}

func TestClientRunAndCancel(t *testing.T) {
	c, err := newClient("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.run(ctx, 30*time.Second)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("client run did not return after cancel")
	}
}

func TestClientRoundTrip(t *testing.T) {
	// Start a UDP echo server as the route target
	echoConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echoConn.Close()
	echoAddr := echoConn.LocalAddr().(*net.UDPAddr)
	go runEcho(echoConn, nil)

	// Client with echo as the route
	c, err := newClient("127.0.0.1:0", []*net.UDPAddr{echoAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	clientAddr := c.InnerAddr()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx, 30*time.Second)
	time.Sleep(100 * time.Millisecond)

	// Simulate WireGuard sending to the client inner addr
	senderConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer senderConn.Close()

	payload := []byte("hello-multipath")
	_, err = senderConn.WriteToUDP(payload, clientAddr)
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, maxPacket)
	senderConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err := senderConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("got %q, want %q", buf[:n], payload)
	}
}

func TestClientWgPeerNilDrop(t *testing.T) {
	echoConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echoConn.Close()
	echoAddr := echoConn.LocalAddr().(*net.UDPAddr)
	go runEcho(echoConn, nil)

	c, err := newClient("127.0.0.1:0", []*net.UDPAddr{echoAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx, 30*time.Second)
	time.Sleep(100 * time.Millisecond)

	// Get the route socket's local address so we can send a packet that
	// arrives at the route reader before wgPeer is learned.
	routeLocalAddr := c.routes[0].LocalAddr().(*net.UDPAddr)

	// Send a spontaneous packet from echo to the route socket.
	// This triggers clientRouteReader with wgPeer == nil; it must drop cleanly.
	echoConn.WriteToUDP([]byte("spam-before-learn"), routeLocalAddr)
	time.Sleep(200 * time.Millisecond)

	// Learn wgPeer by sending a real packet from a sender
	senderConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer senderConn.Close()

	_, err = senderConn.WriteToUDP([]byte("hello"), c.InnerAddr())
	if err != nil {
		t.Fatal(err)
	}

	replyBuf := make([]byte, maxPacket)
	senderConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err := senderConn.ReadFromUDP(replyBuf)
	if err != nil {
		t.Fatalf("read reply after wgPeer learned: %v", err)
	}
	if string(replyBuf[:n]) != "hello" {
		t.Fatalf("got %q, want %q", replyBuf[:n], "hello")
	}
}

func TestClientRunWithZeroRoutes(t *testing.T) {
	// Client with zero routes should start and not panic
	c, err := newClient("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.run(ctx, 30*time.Second)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("run with zero routes did not shut down")
	}
}

func TestClientRoundTripMultipleRoutes(t *testing.T) {
	// Verify round-trip with 2 routes (both pointing at same echo)
	echoConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echoConn.Close()
	echoAddr := echoConn.LocalAddr().(*net.UDPAddr)
	go runEcho(echoConn, nil)

	routes := []*net.UDPAddr{echoAddr, echoAddr}
	c, err := newClient("127.0.0.1:0", routes)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	clientAddr := c.InnerAddr()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx, 30*time.Second)
	time.Sleep(100 * time.Millisecond)

	senderConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer senderConn.Close()

	for i := 0; i < 10; i++ {
		payload := []byte{byte(i)}
		_, err := senderConn.WriteToUDP(payload, clientAddr)
		if err != nil {
			t.Fatal(err)
		}
	}

	buf := make([]byte, maxPacket)
	received := 0
	senderConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for received < 10 {
		_, _, err := senderConn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		received++
	}
	// With 2 routes pointing at the same echo, we should still get all replies
	// (the echo echoes each copy, route readers forward back to wgPeer)
	if received < 10 {
		t.Fatalf("received %d replies, want at least 10", received)
	}
}
