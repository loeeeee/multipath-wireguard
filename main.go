package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const defaultLogInterval = 10 * time.Second

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: multipath-wireguard <client|server> [flags]\n")
		os.Exit(2)
	}
	mode := os.Args[1]
	args := os.Args[2:]

	switch mode {
	case "client":
		runClient(args)
	case "server":
		runServer(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode: %s (expected client or server)\n", mode)
		os.Exit(2)
	}
}

func runClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	listenStr := fs.String("listen", "", "inner listen address (where WireGuard sends)")
	routesFile := fs.String("routes", "", "routes file (one host:port per line)")
	logInterval := fs.Duration("log-interval", defaultLogInterval, "counter dump interval")
	verbose := fs.Bool("v", false, "verbose logging")
	fs.Parse(args)

	if *listenStr == "" {
		fmt.Fprintf(os.Stderr, "client: -listen is required\n")
		os.Exit(2)
	}
	if *routesFile == "" {
		fmt.Fprintf(os.Stderr, "client: -routes is required\n")
		os.Exit(2)
	}

	setupLogging(*verbose)

	if *logInterval <= 0 {
		fmt.Fprintf(os.Stderr, "client: -log-interval must be positive\n")
		os.Exit(2)
	}

	routes, err := parseRoutes(*routesFile)
	if err != nil {
		slog.Error("failed to parse routes", "file", *routesFile, "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	c, err := newClient(*listenStr, routes)
	if err != nil {
		slog.Error("failed to initialize client", "err", err)
		os.Exit(1)
	}
	defer c.close()

	slog.Info("client started", "inner", c.InnerAddr(), "route_count", len(c.routes))

	c.run(ctx, *logInterval)
}

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	listenStr := fs.String("listen", "", "outer listen address (where tunnels forward to)")
	targetStr := fs.String("target", "", "inner target address (local WireGuard UDP port)")
	clientTimeout := fs.Duration("client-timeout", 30*time.Second, "prune idle return paths after this duration")
	logInterval := fs.Duration("log-interval", defaultLogInterval, "counter dump interval")
	verbose := fs.Bool("v", false, "verbose logging")
	fs.Parse(args)

	if *listenStr == "" {
		fmt.Fprintf(os.Stderr, "server: -listen is required\n")
		os.Exit(2)
	}
	if *targetStr == "" {
		fmt.Fprintf(os.Stderr, "server: -target is required\n")
		os.Exit(2)
	}

	setupLogging(*verbose)

	if *clientTimeout <= 0 {
		fmt.Fprintf(os.Stderr, "server: -client-timeout must be positive\n")
		os.Exit(2)
	}
	if *logInterval <= 0 {
		fmt.Fprintf(os.Stderr, "server: -log-interval must be positive\n")
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	s, err := newServer(*listenStr, *targetStr)
	if err != nil {
		slog.Error("failed to initialize server", "err", err)
		os.Exit(1)
	}
	defer s.close()

	slog.Info("server started", "outer", s.OuterAddr(), "target", *targetStr)

	if err := s.run(ctx, *clientTimeout, *logInterval); err != nil {
		slog.Error("server fatal error", "err", err)
		os.Exit(1)
	}
}

func setupLogging(verbose bool) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
}
