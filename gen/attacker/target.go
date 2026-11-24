package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
)

const (
	vethV4     = "10.77.255.1"
	vethV6     = "fd77:255::1"
	envHostLAN = "HOST_LAN_IP"
)

func validateTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("alvo inválido %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("alvo %q deve usar http ou https", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("alvo %q deve ser só esquema e host, sem credenciais, caminho, query ou fragmento", raw)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("alvo %q sem host", raw)
	}
	if !hostAllowed(host) {
		return nil, fmt.Errorf("alvo %q fora da lista permitida (loopback, %s, %s ou $%s)", host, vethV4, vethV6, envHostLAN)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

func hostAllowed(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	if ip.Equal(net.ParseIP(vethV4)) || ip.Equal(net.ParseIP(vethV6)) {
		return true
	}
	if lan := os.Getenv(envHostLAN); lan != "" && ip.Equal(net.ParseIP(lan)) {
		return true
	}
	return false
}
