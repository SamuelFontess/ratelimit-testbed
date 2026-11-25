package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"
)

const (
	tokenNone       = "none"
	tokenValidReuse = "valid-reuse"
	tokenFarm       = "farm"
	tokenHeader     = "X-Token"
	maxTokenBody    = 4 << 10
	tokenCIDFormat  = "atk-%d-%d"
)

type tokenSource struct {
	tokens []string
}

func (ts *tokenSource) forRequest(seq int) string {
	if len(ts.tokens) == 0 {
		return ""
	}
	return ts.tokens[seq%len(ts.tokens)]
}

func tokenCount(cfg config) int {
	switch cfg.tokenMode {
	case tokenFarm:
		return cfg.tokenBudget
	case tokenValidReuse:
		return 1
	default:
		return 0
	}
}

func tokenCID(seed int64, index int) string {
	return fmt.Sprintf(tokenCIDFormat, seed, index)
}

func acquireTokens(ctx context.Context, cfg config, base string, pool []net.IP, cache *transportCache) (*tokenSource, error) {
	n := tokenCount(cfg)
	tokens := make([]string, n)
	for i := range n {
		ip := pool[i%len(pool)]
		tok, err := fetchToken(ctx, cache.get(ip), base+cfg.tokenPath, tokenCID(cfg.seed, i), cfg.total)
		if err != nil {
			return nil, fmt.Errorf("token %d/%d a partir de %s: %w", i+1, n, ip, err)
		}
		tokens[i] = tok
	}
	return &tokenSource{tokens: tokens}, nil
}

func fetchToken(ctx context.Context, rt http.RoundTripper, tokenURL, cid string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL+"?cid="+cid, nil)
	if err != nil {
		return "", fmt.Errorf("monta pedido de token: %w", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return "", fmt.Errorf("pede token em %s: %w", tokenURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("emissão limitada: reduza --token-budget ou aumente E (%s: %d)", tokenURL, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token em %s respondeu %d", tokenURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody+1))
	if err != nil {
		return "", fmt.Errorf("lê token: %w", err)
	}
	if len(body) > maxTokenBody {
		return "", fmt.Errorf("token em %s acima de %d bytes", tokenURL, maxTokenBody)
	}
	tok := strings.TrimSpace(string(body))
	if !validToken(tok) {
		return "", fmt.Errorf("token em %s não é um token", tokenURL)
	}
	return tok, nil
}

func validToken(tok string) bool {
	return tok != "" && strings.IndexFunc(tok, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}
