package main

import (
	"bytes"
	"net"
	"testing"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("CIDR %q: %v", s, err)
	}
	return n
}

func TestPoolV4WithinPrefixAndTolerance(t *testing.T) {
	base := mustCIDR(t, prefixV4)
	const groups = 10
	ips, err := buildPool(base, hostsPerGroupDefault, groups, nil, newRNG(7, poolStream))
	if err != nil {
		t.Fatal(err)
	}
	perOctet := map[byte]int{}
	for _, ip := range ips {
		if !base.Contains(ip) {
			t.Fatalf("%s fora de %s", ip, base)
		}
		perOctet[ip.To4()[2]]++
	}
	if len(perOctet) != groups {
		t.Fatalf("esperava %d grupos /24, vi %d", groups, len(perOctet))
	}
	for octet, n := range perOctet {
		if n < hostsPerGroupDefault-groupTolerance || n > hostsPerGroupDefault+groupTolerance {
			t.Fatalf("/24 .%d com %d IPs, fora de 8±2", octet, n)
		}
	}
}

func TestPoolDeterministic(t *testing.T) {
	base := mustCIDR(t, prefixV4)
	a, err := buildPool(base, hostsPerGroupDefault, 20, nil, newRNG(42, poolStream))
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildPool(base, hostsPerGroupDefault, 20, nil, newRNG(42, poolStream))
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("tamanhos diferentes: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			t.Fatalf("IP %d diverge: %s vs %s", i, a[i], b[i])
		}
	}
}

func TestPoolS3ExcludesClientGroups(t *testing.T) {
	base := mustCIDR(t, prefixV4)
	exclude := rangeSet(1, 5)
	ips, err := buildPool(base, hostsPerGroupDefault, 10, exclude, newRNG(3, poolStream))
	if err != nil {
		t.Fatal(err)
	}
	sawZero := false
	for _, ip := range ips {
		octet := int(ip.To4()[2])
		if exclude[octet] {
			t.Fatalf("usou /24 de cliente .%d", octet)
		}
		if octet == 0 {
			sawZero = true
		}
	}
	if !sawZero {
		t.Fatal("não incluiu o /24 10.66.0.0/24 do atacante")
	}
}

func TestPoolV6WithinPrefix(t *testing.T) {
	base := mustCIDR(t, prefixV6)
	ips, err := buildPool(base, 5, 1, nil, newRNG(9, poolStream))
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) < 3 {
		t.Fatalf("poucos endereços v6: %d", len(ips))
	}
	zero := net.IPv6zero
	for _, ip := range ips {
		if ip.To4() != nil {
			t.Fatalf("%s não é v6", ip)
		}
		if !base.Contains(ip) {
			t.Fatalf("%s fora de %s", ip, base)
		}
		if ip.Equal(zero) {
			t.Fatal("endereço v6 nulo")
		}
	}
}

func TestPoolV6StaysInFirstSlash64(t *testing.T) {
	base := mustCIDR(t, prefixV6)
	ips, err := buildPool(base, 5, 1, nil, newRNG(9, poolStream))
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range ips {
		if !bytes.Equal(ip[:8], base.IP.To16()[:8]) {
			t.Fatalf("%s fora do primeiro /64 de %s", ip, base)
		}
	}
}

func TestPoolV6GroupsUseDistinctSlash64(t *testing.T) {
	base := mustCIDR(t, prefixV6)
	const groups = 3
	ips, err := buildPool(base, 5, groups, nil, newRNG(9, poolStream))
	if err != nil {
		t.Fatal(err)
	}
	subnets := map[string]bool{}
	for _, ip := range ips {
		subnets[string(ip[:8])] = true
	}
	if len(subnets) != groups {
		t.Fatalf("esperava %d /64 distintos, vi %d", groups, len(subnets))
	}
}
