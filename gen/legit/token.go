package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	tokenHeader       = "X-Token"
	tokenMaxAge       = 270 * time.Second
	tokenRetryBackoff = 250 * time.Millisecond
	maxTokenBytes     = 4096
)

type tokenSource struct {
	client     *http.Client
	url        string
	total      time.Duration
	pause      time.Duration
	now        func() time.Time
	mu         sync.Mutex
	token      string
	fetched    time.Time
	refreshing bool
	retryAfter time.Time
}

func newTokenSource(client *http.Client, target, path string, cid int, total, pause time.Duration) *tokenSource {
	return &tokenSource{
		client: client,
		url:    fmt.Sprintf("%s%s?cid=%d", strings.TrimSuffix(target, "/"), path, cid),
		total:  total,
		pause:  pause,
		now:    time.Now,
	}
}

func (s *tokenSource) snapshot() (token string, refresh bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.token == "" || s.refreshing || now.Sub(s.fetched) <= tokenMaxAge || now.Before(s.retryAfter) {
		return s.token, false
	}
	s.refreshing = true
	return s.token, true
}

func (s *tokenSource) finish(token string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshing = false
	if err != nil {
		s.retryAfter = s.now().Add(s.pause)
		return
	}
	s.token, s.fetched = token, s.now()
}

func (s *tokenSource) prefetch(deadline time.Time) error {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for {
		token, err := s.fetch(ctx)
		if err == nil {
			s.finish(token, nil)
			return nil
		}
		if time.Until(deadline) < tokenRetryBackoff {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(tokenRetryBackoff):
		}
	}
}

func (s *tokenSource) refresh(ctx context.Context) error {
	token, err := s.fetch(ctx)
	s.finish(token, err)
	return err
}

func (s *tokenSource) fetch(parent context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(parent, s.total)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return "", fmt.Errorf("montar busca de token %s: %w", s.url, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("buscar token %s: %w", s.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("buscar token %s: status %d", s.url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBytes+1))
	if err != nil {
		return "", fmt.Errorf("ler token %s: %w", s.url, err)
	}
	if len(body) > maxTokenBytes {
		return "", fmt.Errorf("buscar token %s: corpo acima de %d bytes", s.url, maxTokenBytes)
	}
	token := strings.TrimSpace(string(body))
	if !validToken(token) {
		return "", fmt.Errorf("buscar token %s: corpo nao e um token", s.url)
	}
	return token, nil
}

func validToken(token string) bool {
	return token != "" && strings.IndexFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}
