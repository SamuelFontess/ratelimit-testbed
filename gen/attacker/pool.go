package main

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net"
)

const (
	hostsPerGroupDefault = 8
	groupTolerance       = 2
	minHostV4            = 1
	maxHostV4            = 254
	slash24Bits          = 24
	v6SubnetWidth        = 64
	poolStream           = 0x9e3779b97f4a7c15
	planStream           = 0xbf58476d1ce4e5b9
)

func newRNG(seed int64, stream uint64) *rand.Rand {
	return rand.New(rand.NewPCG(uint64(seed), stream))
}

func buildPool(base *net.IPNet, perGroup, groups int, exclude map[int]bool, rng *rand.Rand) ([]net.IP, error) {
	if perGroup < 1 {
		return nil, fmt.Errorf("per-grupo deve ser >= 1, recebido %d", perGroup)
	}
	if groups < 1 {
		return nil, fmt.Errorf("número de grupos deve ser >= 1, recebido %d", groups)
	}
	if base.IP.To4() != nil {
		return poolV4(base, perGroup, groups, exclude, rng)
	}
	return poolV6(base, perGroup, groups, rng)
}

func poolV4(base *net.IPNet, perGroup, groups int, exclude map[int]bool, rng *rand.Rand) ([]net.IP, error) {
	ones, _ := base.Mask.Size()
	if ones > slash24Bits {
		return nil, fmt.Errorf("prefixo /%d é menor que um /24", ones)
	}
	baseU := binary.BigEndian.Uint32(base.IP.To4())
	maxIdx := 1 << (slash24Bits - ones)
	var ips []net.IP
	picked := 0
	for idx := 0; idx < maxIdx && picked < groups; idx++ {
		if exclude[idx] {
			continue
		}
		net24 := baseU + uint32(idx)<<(32-slash24Bits)
		for _, h := range distinctHosts(groupCount(perGroup, rng), rng) {
			ip := make(net.IP, net.IPv4len)
			binary.BigEndian.PutUint32(ip, net24+uint32(h))
			ips = append(ips, ip)
		}
		picked++
	}
	if picked < groups {
		return nil, fmt.Errorf("prefixo /%d comporta só %d grupos /24 úteis, pedidos %d", ones, picked, groups)
	}
	return ips, nil
}

func poolV6(base *net.IPNet, perGroup, groups int, rng *rand.Rand) ([]net.IP, error) {
	ones, _ := base.Mask.Size()
	if ones > v6SubnetWidth {
		return nil, fmt.Errorf("prefixo /%d é menor que um /64", ones)
	}
	subnetBits := v6SubnetWidth - ones
	if subnetBits < 64 && uint64(groups) > uint64(1)<<subnetBits {
		return nil, fmt.Errorf("prefixo /%d comporta só %d sub-redes /64, pedidas %d", ones, uint64(1)<<subnetBits, groups)
	}
	net16 := base.IP.To16()
	subnetByte := ones / 8
	var ips []net.IP
	for idx := range groups {
		for range groupCount(perGroup, rng) {
			ip := make(net.IP, net.IPv6len)
			copy(ip, net16)
			binary.BigEndian.PutUint16(ip[subnetByte:], binary.BigEndian.Uint16(net16[subnetByte:])+uint16(idx))
			binary.BigEndian.PutUint64(ip[8:], rng.Uint64()|1)
			ips = append(ips, ip)
		}
	}
	return ips, nil
}

func groupCount(perGroup int, rng *rand.Rand) int {
	c := perGroup + rng.IntN(2*groupTolerance+1) - groupTolerance
	if c < 1 {
		c = 1
	}
	return c
}

func distinctHosts(n int, rng *rand.Rand) []int {
	span := maxHostV4 - minHostV4 + 1
	if n > span {
		n = span
	}
	perm := rng.Perm(span)
	hosts := make([]int, n)
	for i := range n {
		hosts[i] = perm[i] + minHostV4
	}
	return hosts
}

func ceilDiv(a, b int) int {
	return (a + b - 1) / b
}

func rangeSet(from, count int) map[int]bool {
	s := make(map[int]bool, count)
	for i := range count {
		s[from+i] = true
	}
	return s
}
