package main

import (
	"net"
	"strings"
	"testing"
)

func TestCheckSourcesAcceptsLocalPool(t *testing.T) {
	if err := checkSources(loopbackPool(3), "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckSourcesAcceptsHostnameTarget(t *testing.T) {
	if err := checkSources(loopbackPool(1), "localhost"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckSourcesRejectsNonLocalIP(t *testing.T) {
	foreign := net.ParseIP("192.0.2.1")
	err := checkSources([]net.IP{foreign}, "127.0.0.1")
	if err == nil || !strings.Contains(err.Error(), foreign.String()) {
		t.Fatalf("IP não local devia falhar citando o IP, veio %v", err)
	}
}

func TestCheckSourcesRejectsFamilyMismatch(t *testing.T) {
	v6 := net.ParseIP("fd12:3456:789a::1")
	if err := checkSources([]net.IP{v6}, "127.0.0.1"); err == nil || !strings.Contains(err.Error(), "famílias") {
		t.Fatalf("pool v6 com alvo v4 devia falhar por família, veio %v", err)
	}
	v4 := net.ParseIP("127.0.0.2")
	if err := checkSources([]net.IP{v4}, "fd77:255::1"); err == nil || !strings.Contains(err.Error(), "famílias") {
		t.Fatalf("pool v4 com alvo v6 devia falhar por família, veio %v", err)
	}
}
