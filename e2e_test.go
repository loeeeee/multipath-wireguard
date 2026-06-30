package main

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type e2eTopology struct {
	s             *server
	serverCancel  context.CancelFunc
	c             *client
	clientCancel  context.CancelFunc
	f1Cleanup     func()
	f2Cleanup     func()
	echoRecvCount *atomic.Int64
	senderConn    *net.UDPConn
}

func startE2ETopology(t *testing.T, serverTimeout time.Duration, needEchoCount bool) *e2eTopology {
	t.Helper()

	echoConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	echoAddr := echoConn.LocalAddr().(*net.UDPAddr)

	var recvCount *atomic.Int64
	if needEchoCount {
		recvCount = &atomic.Int64{}
	}
	go runEcho(echoConn, recvCount)
	t.Cleanup(func() { echoConn.Close() })

	s, err := newServer("127.0.0.1:0", echoAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	serverAddr := s.OuterAddr()
	serverCtx, serverCancel := context.WithCancel(context.Background())
	go s.run(serverCtx, serverTimeout, 30*time.Second)
	time.Sleep(100 * time.Millisecond)

	fwd1Addr, f1Cleanup := startForwarder(t, serverAddr)
	fwd2Addr, f2Cleanup := startForwarder(t, serverAddr)

	c, err := newClient("127.0.0.1:0", []*net.UDPAddr{fwd1Addr, fwd2Addr})
	if err != nil {
		t.Fatal(err)
	}
	clientAddr := c.InnerAddr()
	clientCtx, clientCancel := context.WithCancel(context.Background())
	go c.run(clientCtx, 30*time.Second)
	time.Sleep(100 * time.Millisecond)

	senderConn, err := net.DialUDP("udp", nil, clientAddr)
	if err != nil {
		t.Fatal(err)
	}

	return &e2eTopology{
		s:             s,
		serverCancel:  serverCancel,
		c:             c,
		clientCancel:  clientCancel,
		f1Cleanup:     f1Cleanup,
		f2Cleanup:     f2Cleanup,
		echoRecvCount: recvCount,
		senderConn:    senderConn,
	}
}

func (topo *e2eTopology) cleanup() {
	topo.senderConn.Close()
	topo.clientCancel()
	topo.c.close()
	topo.f1Cleanup()
	topo.f2Cleanup()
	topo.serverCancel()
	topo.s.close()
}

func TestE2EDuplication(t *testing.T) {
	topo := startE2ETopology(t, 30*time.Second, true)
	defer topo.cleanup()

	numPackets := 50
	received := make(map[string]bool)
	replyBuf := make([]byte, maxPacket)

	for i := 0; i < numPackets; i++ {
		payload := fmt.Sprintf("pkt-%d", i)
		if _, err := topo.senderConn.Write([]byte(payload)); err != nil {
			t.Fatalf("send packet %d: %v", i, err)
		}
	}

	topo.senderConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		n, err := topo.senderConn.Read(replyBuf)
		if err != nil {
			break
		}
		received[string(replyBuf[:n])] = true
		if len(received) >= numPackets {
			break
		}
	}

	if len(received) != numPackets {
		t.Fatalf("received %d unique replies, want %d", len(received), numPackets)
	}

	echoTotal := topo.echoRecvCount.Load()
	t.Logf("echo received %d total packets for %d unique", echoTotal, numPackets)
	if echoTotal <= int64(numPackets) {
		t.Fatalf("echo received %d packets, expected > %d (duplication)", echoTotal, numPackets)
	}
}

func TestE2EFailover(t *testing.T) {
	topo := startE2ETopology(t, 30*time.Second, false)
	defer topo.cleanup()

	for i := 0; i < 5; i++ {
		payload := fmt.Sprintf("warmup-%d", i)
		topo.senderConn.Write([]byte(payload))
	}
	time.Sleep(200 * time.Millisecond)

	t.Log("killing forwarder 1")
	topo.f1Cleanup()

	numPackets := 30
	received := make(map[string]bool)
	replyBuf := make([]byte, maxPacket)

	for i := 0; i < numPackets; i++ {
		payload := fmt.Sprintf("failover-%d", i)
		if _, err := topo.senderConn.Write([]byte(payload)); err != nil {
			t.Fatalf("send packet %d: %v", i, err)
		}
	}

	topo.senderConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		n, err := topo.senderConn.Read(replyBuf)
		if err != nil {
			break
		}
		received[string(replyBuf[:n])] = true
		if len(received) >= numPackets {
			break
		}
	}

	if len(received) != numPackets {
		t.Fatalf("received %d unique replies after failover, want %d", len(received), numPackets)
	}
	t.Logf("failover successful: received %d/%d replies", len(received), numPackets)
}

func TestE2ERouteTablePrune(t *testing.T) {
	topo := startE2ETopology(t, 500*time.Millisecond, false)
	defer topo.cleanup()

	for i := 0; i < 5; i++ {
		payload := fmt.Sprintf("learn-%d", i)
		topo.senderConn.Write([]byte(payload))
	}
	time.Sleep(200 * time.Millisecond)

	if topo.s.RouteCount() != 2 {
		t.Fatalf("server route count: got %d, want 2", topo.s.RouteCount())
	}

	t.Log("killing forwarder 1")
	topo.f1Cleanup()
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 5; i++ {
		payload := fmt.Sprintf("keepalive-%d", i)
		topo.senderConn.Write([]byte(payload))
	}
	time.Sleep(800 * time.Millisecond)

	if topo.s.RouteCount() != 1 {
		t.Fatalf("server route count after prune: got %d, want 1", topo.s.RouteCount())
	}
	t.Log("route table pruned correctly")
}

func runEcho(conn *net.UDPConn, recvCount *atomic.Int64) {
	buf := make([]byte, maxPacket)
	for {
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if recvCount != nil {
			recvCount.Add(1)
		}
		if _, err := conn.WriteToUDP(buf[:n], peer); err != nil {
			return
		}
	}
}

func startForwarder(t *testing.T, target *net.UDPAddr) (*net.UDPAddr, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	fwdAddr := conn.LocalAddr().(*net.UDPAddr)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer conn.Close()
		buf := make([]byte, maxPacket)
		var clientPeer *net.UDPAddr

		for {
			n, peer, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}

			if peer.IP.Equal(target.IP) && peer.Port == target.Port {
				if clientPeer != nil {
					conn.WriteToUDP(buf[:n], clientPeer)
				}
			} else {
				clientPeer = peer
				conn.WriteToUDP(buf[:n], target)
			}
		}
	}()

	cleanup := func() {
		conn.Close()
		<-done
	}
	return fwdAddr, cleanup
}
