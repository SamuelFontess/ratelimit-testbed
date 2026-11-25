package main

import (
	"fmt"
	"net"
)

func checkSources(pool []net.IP, targetHost string) error {
	targetIP := net.ParseIP(targetHost)
	for _, ip := range pool {
		if targetIP != nil && (ip.To4() == nil) != (targetIP.To4() == nil) {
			return fmt.Errorf("IP de origem %s e alvo %s são de famílias de endereço diferentes", ip, targetHost)
		}
		l, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
		if err != nil {
			return fmt.Errorf("IP de origem %s não é local: %w", ip, err)
		}
		l.Close()
	}
	return nil
}
