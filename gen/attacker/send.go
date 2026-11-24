package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	connectTimeout  = 5 * time.Second
	idleConnTimeout = 30 * time.Second
	maxTransports   = 4096
	inflightCeiling = 20000
	fdShareDivisor  = 2
	drainLimit      = 1 << 20
)

var postBody = []byte("x=1")

var errSaturated = errors.New("carga acima do que o gerador sustenta (maxInflight)")

const (
	outcomeOK        = "ok"
	outcomePartial   = "fail_partial"
	outcomeTotal     = "fail_total"
	outcomeRefused   = "fail_refused"
	outcomeGenerator = "generator_error"

	statusTooManyRequests = 429
	statusUnauthorized    = 401
)

func inflightLimit() (int, error) {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return 0, fmt.Errorf("lê RLIMIT_NOFILE: %w", err)
	}
	return inflightLimitFor(rl.Cur), nil
}

func inflightLimitFor(cur uint64) int {
	return int(min(cur/fdShareDivisor, inflightCeiling))
}

type transportCache struct {
	keepalive bool
	mu        sync.Mutex
	byIP      map[string]*http.Transport
	order     []string
}

func newTransportCache(keepalive bool) *transportCache {
	return &transportCache{keepalive: keepalive, byIP: make(map[string]*http.Transport)}
}

func (c *transportCache) get(ip net.IP) *http.Transport {
	key := ip.String()
	c.mu.Lock()
	defer c.mu.Unlock()
	if tr, ok := c.byIP[key]; ok {
		return tr
	}
	if len(c.order) >= maxTransports {
		old := c.order[0]
		c.order = c.order[1:]
		c.byIP[old].CloseIdleConnections()
		delete(c.byIP, old)
	}
	tr := newTransport(ip, c.keepalive)
	c.byIP[key] = tr
	c.order = append(c.order, key)
	return tr
}

func (c *transportCache) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tr := range c.byIP {
		tr.CloseIdleConnections()
	}
}

func newTransport(ip net.IP, keepalive bool) *http.Transport {
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: ip}, Timeout: connectTimeout}
	return &http.Transport{
		DialContext:         dialer.DialContext,
		DisableKeepAlives:   !keepalive,
		IdleConnTimeout:     idleConnTimeout,
		MaxIdleConnsPerHost: 1,
	}
}

type senderConfig struct {
	target      string
	cache       *transportCache
	tokens      *tokenSource
	partial     time.Duration
	total       time.Duration
	maxInflight int
}

type sender struct {
	senderConfig
	out      chan<- record
	inflight chan struct{}
	wg       sync.WaitGroup

	mu          sync.Mutex
	cancel      context.CancelFunc
	failures    int
	firstFailed error
}

func newSender(cfg senderConfig, out chan<- record) *sender {
	return &sender{
		senderConfig: cfg,
		out:          out,
		inflight:     make(chan struct{}, cfg.maxInflight),
	}
}

func (s *sender) fail(err error) {
	s.mu.Lock()
	s.failures++
	if s.firstFailed == nil {
		s.firstFailed = err
	}
	s.mu.Unlock()
	s.cancel()
}

func (s *sender) generatorError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures == 0 {
		return nil
	}
	return fmt.Errorf("%d erros do gerador, primeiro: %w", s.failures, s.firstFailed)
}

func (s *sender) run(ctx context.Context, start time.Time, p *planner) error {
	ctx, s.cancel = context.WithCancel(ctx)
	defer s.cancel()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for ctx.Err() == nil {
		pr, ok := p.Next()
		if !ok {
			break
		}
		if d := time.Until(start.Add(time.Duration(pr.tPlan * float64(time.Second)))); d > 0 {
			timer.Reset(d)
			select {
			case <-timer.C:
			case <-ctx.Done():
				continue
			}
		}
		select {
		case s.inflight <- struct{}{}:
		default:
			s.fail(fmt.Errorf("%w: %d requisições em voo", errSaturated, cap(s.inflight)))
			continue
		}
		s.wg.Add(1)
		go func(pr plannedRequest) {
			defer func() { <-s.inflight; s.wg.Done() }()
			s.do(ctx, start, pr)
		}(pr)
	}
	s.wg.Wait()
	return s.generatorError()
}

func (s *sender) do(ctx context.Context, start time.Time, pr plannedRequest) {
	reqStart := time.Now()
	reqCtx, cancel := context.WithTimeout(ctx, s.total)
	defer cancel()

	var body io.Reader
	if pr.method == http.MethodPost {
		body = bytes.NewReader(postBody)
	}
	url := s.target + pr.route
	if strings.Contains(pr.route, "?") {
		url += "&cb=" + pr.cacheBust
	} else {
		url += "?cb=" + pr.cacheBust
	}
	req, err := http.NewRequestWithContext(reqCtx, pr.method, url, body)
	if err != nil {
		s.fail(fmt.Errorf("monta requisição %s %s: %w", pr.method, url, err))
		return
	}
	req.Header.Set("User-Agent", pr.ua)
	for _, h := range pr.headers {
		req.Header.Set(h[0], h[1])
	}
	if tok := s.tokens.forRequest(pr.seq); tok != "" {
		req.Header.Set(tokenHeader, tok)
	}

	resp, rerr := s.cache.get(pr.ip).RoundTrip(req)
	elapsed := time.Since(reqStart)
	status := 0
	if resp != nil {
		status = resp.StatusCode
		io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
		resp.Body.Close()
	}
	if ctx.Err() != nil {
		return
	}
	status, outcome := classify(status, rerr, elapsed, s.partial, s.total)
	if outcome == outcomeGenerator {
		s.fail(fmt.Errorf("origem %s: %w", pr.ip, rerr))
		return
	}
	tStart := reqStart.Sub(start).Seconds()
	s.out <- record{tStart: tStart, tEnd: tStart + elapsed.Seconds(), status: status, outcome: outcome}
}

func classify(status int, err error, elapsed, partial, total time.Duration) (int, string) {
	if err != nil {
		switch {
		case isGeneratorError(err):
			return 0, outcomeGenerator
		case isRefusedOrReset(err):
			return 0, outcomeRefused
		case elapsed >= total:
			return 0, outcomeTotal
		case elapsed >= partial:
			return 0, outcomePartial
		default:
			return 0, outcomeTotal
		}
	}
	switch {
	case status == statusTooManyRequests || status == statusUnauthorized:
		return status, outcomeRefused
	case elapsed >= total:
		return status, outcomeTotal
	case elapsed >= partial:
		return status, outcomePartial
	default:
		return status, outcomeOK
	}
}

func isGeneratorError(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
}

func isRefusedOrReset(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
