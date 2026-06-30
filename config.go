package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

func parseRoutes(path string) ([]*net.UDPAddr, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open routes file: %w", err)
	}
	defer f.Close()

	var routes []*net.UDPAddr
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		addr, err := net.ResolveUDPAddr("udp", line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		routes = append(routes, addr)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read routes file: %w", err)
	}
	if len(routes) == 0 {
		return nil, errors.New("routes file contains no routes")
	}
	return routes, nil
}
