package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseRoutes(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		content string
		want    int
		wantErr bool
	}{
		{"single route", "127.0.0.1:51811\n", 1, false},
		{"two routes", "127.0.0.1:51811\n127.0.0.1:51812\n", 2, false},
		{"with comments", "# tunnel 1\n127.0.0.1:51811\n# tunnel 2\n127.0.0.1:51812\n", 2, false},
		{"with blank lines", "\n127.0.0.1:51811\n\n127.0.0.1:51812\n\n", 2, false},
		{"trailing newline", "127.0.0.1:51811\n", 1, false},
		{"no trailing newline", "127.0.0.1:51811", 1, false},
		{"empty file", "", 0, true},
		{"only comments and blanks", "# comment\n\n  \n", 0, true},
		{"invalid port", "127.0.0.1:abc\n", 0, true},
		{"missing port", "127.0.0.1\n", 0, true},
		{"invalid host", "not-an-address:51811\n", 0, true},
		{"garbage line", "this is not a route\n127.0.0.1:51811\n", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, []byte(tc.content), 0644); err != nil {
				t.Fatal(err)
			}

			routes, err := parseRoutes(path)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.wantErr && len(routes) != tc.want {
				t.Fatalf("got %d routes, want %d", len(routes), tc.want)
			}
		})
	}
}

func TestParseRoutesNonExistent(t *testing.T) {
	_, err := parseRoutes("/nonexistent/path/routes.conf")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestCounter(t *testing.T) {
	var c counter

	c.recordRx(100)
	c.recordRx(50)

	rxPkts, rxBytes, _, _, lastSeen := c.snapshot()
	if rxPkts != 2 {
		t.Fatalf("rxPkts: got %d, want 2", rxPkts)
	}
	if rxBytes != 150 {
		t.Fatalf("rxBytes: got %d, want 150", rxBytes)
	}
	if lastSeen.IsZero() {
		t.Fatal("lastSeen should not be zero")
	}

	c.recordTx(200)
	_, _, txPkts, txBytes, _ := c.snapshot()
	if txPkts != 1 {
		t.Fatalf("txPkts: got %d, want 1", txPkts)
	}
	if txBytes != 200 {
		t.Fatalf("txBytes: got %d, want 200", txBytes)
	}
}

func TestCounterConcurrent(t *testing.T) {
	var c counter
	done := make(chan struct{})

	go func() {
		for i := 0; i < 1000; i++ {
			c.recordRx(10)
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 1000; i++ {
			c.recordTx(20)
		}
		done <- struct{}{}
	}()

	<-done
	<-done

	rxPkts, rxBytes, txPkts, txBytes, _ := c.snapshot()
	if rxPkts != 1000 {
		t.Fatalf("rxPkts: got %d, want 1000", rxPkts)
	}
	if rxBytes != 10000 {
		t.Fatalf("rxBytes: got %d, want 10000", rxBytes)
	}
	if txPkts != 1000 {
		t.Fatalf("txPkts: got %d, want 1000", txPkts)
	}
	if txBytes != 20000 {
		t.Fatalf("txBytes: got %d, want 20000", txBytes)
	}
}

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(50 * time.Millisecond)

	if !rl.allow() {
		t.Fatal("first call should be allowed")
	}
	if rl.allow() {
		t.Fatal("second immediate call should be denied")
	}

	time.Sleep(60 * time.Millisecond)

	if !rl.allow() {
		t.Fatal("third call after interval should be allowed")
	}
}

func TestRouteTableUpsert(t *testing.T) {
	rt := newRouteTable()

	addr1 := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1111}
	addr2 := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}

	rt.upsert(addr1, 100)
	if rt.count() != 1 {
		t.Fatalf("count: got %d, want 1", rt.count())
	}

	rt.upsert(addr2, 200)
	if rt.count() != 2 {
		t.Fatalf("count: got %d, want 2", rt.count())
	}

	rt.upsert(addr1, 150)
	if rt.count() != 2 {
		t.Fatalf("upsert existing should not increase count: got %d, want 2", rt.count())
	}

	entries := rt.snapshot()
	if len(entries) != 2 {
		t.Fatalf("snapshot: got %d entries, want 2", len(entries))
	}

	rxPkts, _, _, _, _ := entries[0].counter.snapshot()
	if rxPkts < 1 {
		t.Fatal("snapshot entry should have rxPackets")
	}
}

func TestRouteTablePrune(t *testing.T) {
	rt := newRouteTable()

	addr1 := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1111}
	addr2 := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}

	rt.upsert(addr1, 100)
	rt.upsert(addr2, 200)

	time.Sleep(50 * time.Millisecond)

	pruned := rt.prune(100 * time.Millisecond)
	if pruned != 0 {
		t.Fatalf("prune recent entries: got %d pruned, want 0", pruned)
	}

	pruned = rt.prune(10 * time.Millisecond)
	if pruned != 2 {
		t.Fatalf("prune expired entries: got %d pruned, want 2", pruned)
	}

	if rt.count() != 0 {
		t.Fatalf("count after prune: got %d, want 0", rt.count())
	}
}
