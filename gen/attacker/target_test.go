package main

import "testing"

func TestValidateTargetAllowsAndNormalizes(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8081":      "http://127.0.0.1:8081",
		"http://localhost:8081/":     "http://localhost:8081",
		"http://10.77.255.1:8081":    "http://10.77.255.1:8081",
		"http://[fd77:255::1]:8081/": "http://[fd77:255::1]:8081",
	}
	for raw, want := range cases {
		u, err := validateTarget(raw)
		if err != nil {
			t.Fatalf("%q devia ser aceito: %v", raw, err)
		}
		if got := u.String(); got != want {
			t.Fatalf("%q normalizou para %q, esperava %q", raw, got, want)
		}
	}
}

func TestValidateTargetRejects(t *testing.T) {
	for _, raw := range []string{
		"http://10.0.0.5",
		"http://example.com/",
		"https://8.8.8.8/",
		"ftp://127.0.0.1/",
		"http://[fd12:3456:789a::1]/",
		"http://127.0.0.1:8081/api",
		"http://127.0.0.1:8081?x=1",
		"http://127.0.0.1:8081/?",
		"http://127.0.0.1:8081/#frag",
		"http://user:pw@127.0.0.1:8081",
		"http://127.0.0.1:8081@evil.example",
	} {
		if _, err := validateTarget(raw); err == nil {
			t.Fatalf("%q devia ser recusado", raw)
		}
	}
}

func TestValidateTargetHostLANIP(t *testing.T) {
	t.Setenv(envHostLAN, "10.0.0.42")
	if _, err := validateTarget("http://10.0.0.42:8081"); err != nil {
		t.Fatalf("IP de LAN informado devia ser aceito: %v", err)
	}
	if _, err := validateTarget("http://10.0.0.43:8081"); err == nil {
		t.Fatal("outro IP de LAN devia ser recusado")
	}
}
