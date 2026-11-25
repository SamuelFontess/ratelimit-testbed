package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	testMaxInflight = 1000
	testPlanSeed    = 77
)

func loopbackPool(n int) []net.IP {
	pool := make([]net.IP, n)
	for i := range n {
		pool[i] = net.IPv4(127, 0, 0, byte(i+2))
	}
	return pool
}

func testSenderConfig(target string, partial, total time.Duration) senderConfig {
	return senderConfig{
		target:      target,
		cache:       newTransportCache(false),
		tokens:      &tokenSource{},
		partial:     partial,
		total:       total,
		maxInflight: testMaxInflight,
	}
}

func singleRequestPlan(pool []net.IP) planConfig {
	cfg := planCfg(pool)
	cfg.rate = 1000
	cfg.duration = 0.001
	cfg.postRatio = 0
	cfg.heavyRatio = 0
	return cfg
}

func collectRecords(records <-chan record) <-chan []record {
	out := make(chan []record, 1)
	go func() {
		var rs []record
		for r := range records {
			rs = append(rs, r)
		}
		out <- rs
	}()
	return out
}

func runPlan(ctx context.Context, sc senderConfig, plan planConfig) ([]record, error) {
	records := make(chan record, 4096)
	got := collectRecords(records)
	s := newSender(sc, records)
	err := s.run(ctx, time.Now(), newPlanner(plan, testPlanSeed))
	close(records)
	sc.cache.closeAll()
	return <-got, err
}

func hungServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	started := make(chan struct{}, testMaxInflight)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv, started
}

func plannedOrigins(cfg planConfig) map[string]net.IP {
	origins := map[string]net.IP{}
	p := newPlanner(cfg, testPlanSeed)
	for {
		pr, ok := p.Next()
		if !ok {
			return origins
		}
		origins[pr.cacheBust] = pr.ip
	}
}

func TestPerRequestSourceBind(t *testing.T) {
	cases := []struct {
		name      string
		keepalive bool
		rate      float64
		want      int
	}{
		{"keepalive", true, 2000, 200},
		{"sem keepalive", false, 1000, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := planCfg(loopbackPool(100))
			cfg.rate = c.rate
			cfg.duration = 0.1
			cfg.heavyRatio = 0
			cfg.postRatio = 0
			origins := plannedOrigins(cfg)

			var mu sync.Mutex
			var wrong []string
			var conns atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				host, _, err := net.SplitHostPort(r.RemoteAddr)
				if err != nil {
					t.Error(err)
					return
				}
				cb := r.URL.Query().Get("cb")
				if want := origins[cb]; want.String() != host {
					mu.Lock()
					wrong = append(wrong, fmt.Sprintf("cb %q saiu de %s, planejado %s", cb, host, want))
					mu.Unlock()
				}
			}))
			srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					conns.Add(1)
				}
			}
			srv.Start()
			defer srv.Close()

			sc := senderConfig{
				target:      srv.URL,
				cache:       newTransportCache(c.keepalive),
				tokens:      &tokenSource{},
				partial:     defaultTotalTimeout,
				total:       defaultTotalTimeout,
				maxInflight: testMaxInflight,
			}
			records, err := runPlan(context.Background(), sc, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != c.want {
				t.Fatalf("%d registros, esperava %d", len(records), c.want)
			}
			if len(wrong) > 0 {
				t.Fatalf("origem por requisição divergente: %v", wrong)
			}
			if got := conns.Load(); !c.keepalive && got < int32(c.want) {
				t.Fatalf("sem keepalive: %d conexões para %d requisições", got, c.want)
			}
			for _, r := range records {
				if r.outcome != outcomeOK || r.status != http.StatusOK {
					t.Fatalf("registro com falha: %+v", r)
				}
			}
		})
	}
}

func TestSenderRequestShape(t *testing.T) {
	var mu sync.Mutex
	var paths, tokens, busts []string
	heavyMS := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.URL.Path)
		tokens = append(tokens, r.Header.Get(tokenHeader))
		busts = append(busts, r.URL.Query().Get("cb"))
		if r.URL.Path == "/heavy" {
			heavyMS[r.URL.Query().Get("ms")] = true
		}
	}))
	defer srv.Close()

	sc := testSenderConfig(srv.URL, defaultPartialTimeout, defaultTotalTimeout)
	sc.tokens = &tokenSource{tokens: []string{"tokA", "tokB"}}
	cfg := planCfg(loopbackPool(4))
	cfg.rate = 400
	cfg.duration = 0.25
	records, err := runPlan(context.Background(), sc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 100 || len(paths) != 100 {
		t.Fatalf("%d registros e %d requisições, esperava 100", len(records), len(paths))
	}
	usedTokens := map[string]bool{}
	for i := range paths {
		if paths[i] != "/api" && paths[i] != "/heavy" {
			t.Fatalf("caminho inesperado %q", paths[i])
		}
		if len(busts[i]) != cacheBustLen {
			t.Fatalf("cache-bust %q com tamanho errado", busts[i])
		}
		usedTokens[tokens[i]] = true
	}
	if len(usedTokens) != 2 || !usedTokens["tokA"] || !usedTokens["tokB"] {
		t.Fatalf("tokens vistos no header X-Token: %v", usedTokens)
	}
	if !heavyMS["200"] || len(heavyMS) != 1 {
		t.Fatalf("/heavy sem ms=200: %v", heavyMS)
	}
}

func TestSenderOmitsTokenHeaderWithoutTokens(t *testing.T) {
	var got atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(tokenHeader) != "" {
			got.Add(1)
		}
	}))
	defer srv.Close()
	if _, err := runPlan(context.Background(), testSenderConfig(srv.URL, defaultPartialTimeout, defaultTotalTimeout), singleRequestPlan(loopbackPool(1))); err != nil {
		t.Fatal(err)
	}
	if got.Load() != 0 {
		t.Fatal("token none enviou X-Token")
	}
}

func TestHungServerBeyondTotalIsFailTotal(t *testing.T) {
	srv, _ := hungServer(t)
	records, err := runPlan(context.Background(), testSenderConfig(srv.URL, 50*time.Millisecond, 200*time.Millisecond), singleRequestPlan(loopbackPool(1)))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].outcome != outcomeTotal || records[0].status != 0 {
		t.Fatalf("servidor travado devia dar fail_total sem status, veio %+v", records)
	}
	if records[0].tEnd-records[0].tStart < 0.2 {
		t.Fatalf("fail_total antes do total-timeout: %+v", records[0])
	}
}

func TestResponseBetweenPartialAndTotalIsFailPartial(t *testing.T) {
	const delay = 150 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
	}))
	defer srv.Close()
	records, err := runPlan(context.Background(), testSenderConfig(srv.URL, 50*time.Millisecond, 10*time.Second), singleRequestPlan(loopbackPool(1)))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].outcome != outcomePartial || records[0].status != http.StatusOK {
		t.Fatalf("resposta lenta devia dar fail_partial com status 200, veio %+v", records)
	}
}

func TestGeneratorBindErrorAbortsWithoutRecords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	foreign := []net.IP{net.ParseIP("192.0.2.1")}
	cfg := planCfg(foreign)
	cfg.rate = 1000
	cfg.duration = 0.01
	records, err := runPlan(context.Background(), testSenderConfig(srv.URL, defaultPartialTimeout, defaultTotalTimeout), cfg)
	if !errors.Is(err, syscall.EADDRNOTAVAIL) {
		t.Fatalf("erro de bind devia abortar com EADDRNOTAVAIL, veio %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("erro do gerador virou %d registros de falha", len(records))
	}
}

func TestSaturatedInflightAbortsRun(t *testing.T) {
	srv, _ := hungServer(t)
	sc := testSenderConfig(srv.URL, defaultPartialTimeout, defaultTotalTimeout)
	sc.maxInflight = 1
	cfg := planCfg(loopbackPool(2))
	cfg.rate = 1000
	cfg.duration = 0.05
	records, err := runPlan(context.Background(), sc, cfg)
	if !errors.Is(err, errSaturated) {
		t.Fatalf("saturação devia abortar com errSaturated, veio %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("requisições abortadas viraram %d registros", len(records))
	}
}

func TestCancelDiscardsInFlightRequests(t *testing.T) {
	srv, started := hungServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		records []record
		err     error
	}
	done := make(chan result, 1)
	go func() {
		records, err := runPlan(ctx, testSenderConfig(srv.URL, defaultPartialTimeout, defaultTotalTimeout), singleRequestPlan(loopbackPool(1)))
		done <- result{records, err}
	}()
	<-started
	cancel()
	res := <-done
	if res.err != nil {
		t.Fatalf("cancelamento externo não é erro do gerador: %v", res.err)
	}
	if len(res.records) != 0 {
		t.Fatalf("requisição em voo virou registro fantasma: %+v", res.records)
	}
}

func connectionCounter(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns
}

func TestKeepaliveFlagControlsConnectionReuse(t *testing.T) {
	const requests = 5
	for _, c := range []struct {
		keepalive bool
		want      int32
	}{{false, requests}, {true, 1}} {
		srv, conns := connectionCounter(t)
		cache := newTransportCache(c.keepalive)
		for range requests {
			req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := cache.get(net.ParseIP("127.0.0.2")).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		cache.closeAll()
		if got := conns.Load(); got != c.want {
			t.Errorf("keepalive=%v: %d conexões, esperava %d", c.keepalive, got, c.want)
		}
	}
}

func TestTransportCacheReusesPerIPAndEvictsOldest(t *testing.T) {
	cache := newTransportCache(false)
	first := net.IPv4(10, 0, 0, 0)
	tr := cache.get(first)
	if cache.get(net.IPv4(10, 0, 0, 0)) != tr {
		t.Fatal("mesmo IP devia reaproveitar o transporte")
	}
	for i := 1; i < maxTransports+10; i++ {
		cache.get(net.IPv4(10, 0, byte(i>>8), byte(i)))
	}
	if len(cache.byIP) != maxTransports {
		t.Fatalf("cache com %d transportes, limite %d", len(cache.byIP), maxTransports)
	}
	if _, ok := cache.byIP[first.String()]; ok {
		t.Fatal("o transporte mais antigo devia ter sido removido")
	}
}

func TestInflightLimitFor(t *testing.T) {
	cases := []struct {
		fdLimit uint64
		want    int
	}{
		{1024, 512},
		{2 * inflightCeiling * fdShareDivisor, inflightCeiling},
		{1 << 20, inflightCeiling},
	}
	for _, c := range cases {
		if got := inflightLimitFor(c.fdLimit); got != c.want {
			t.Errorf("RLIMIT_NOFILE=%d deu %d, esperava %d", c.fdLimit, got, c.want)
		}
	}
}

func TestInflightLimitFollowsFileDescriptorLimit(t *testing.T) {
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
		t.Fatal(err)
	}
	lowered := original
	lowered.Cur = 1024
	if lowered.Max < lowered.Cur {
		t.Skipf("soft RLIMIT_NOFILE limitado a %d", lowered.Max)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lowered); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
			t.Errorf("restaura RLIMIT_NOFILE: %v", err)
		}
	})
	limit, err := inflightLimit()
	if err != nil {
		t.Fatal(err)
	}
	if limit != 512 {
		t.Fatalf("limite %d com RLIMIT_NOFILE=1024, esperava 512", limit)
	}
}

func TestCloseAllClosesIdleConnections(t *testing.T) {
	closed := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	srv.Start()
	defer srv.Close()

	cache := newTransportCache(true)
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := cache.get(net.ParseIP("127.0.0.2")).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	cache.closeAll()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closeAll deixou conexão ociosa aberta")
	}
}

func TestClassifyOutcomes(t *testing.T) {
	const partial, total = 4 * time.Second, 60 * time.Second
	wrapped := func(errno syscall.Errno) error { return fmt.Errorf("dial: %w", &net.OpError{Op: "dial", Err: errno}) }
	cases := []struct {
		name       string
		status     int
		err        error
		elapsed    time.Duration
		want       string
		wantStatus int
	}{
		{"2xx rápido", 200, nil, time.Second, outcomeOK, 200},
		{"405 de POST", 405, nil, time.Second, outcomeOK, 405},
		{"5xx rápido", 503, nil, time.Second, outcomeOK, 503},
		{"404 do alvo", 404, nil, time.Second, outcomeOK, 404},
		{"429 contido", 429, nil, time.Second, outcomeRefused, 429},
		{"429 depois do parcial", 429, nil, 5 * time.Second, outcomeRefused, 429},
		{"401 contido", 401, nil, time.Second, outcomeRefused, 401},
		{"lento vira parcial", 200, nil, 5 * time.Second, outcomePartial, 200},
		{"lento além do total", 200, nil, total, outcomeTotal, 200},
		{"recusa de conexão", 0, syscall.ECONNREFUSED, time.Second, outcomeRefused, 0},
		{"reset", 0, wrapped(syscall.ECONNRESET), time.Second, outcomeRefused, 0},
		{"reset depois do parcial", 0, wrapped(syscall.ECONNRESET), 5 * time.Second, outcomeRefused, 0},
		{"pipe quebrado", 0, syscall.EPIPE, time.Second, outcomeRefused, 0},
		{"EOF sem dados", 0, io.EOF, time.Second, outcomeTotal, 0},
		{"EOF depois do parcial", 0, io.EOF, 5 * time.Second, outcomePartial, 0},
		{"servidor mudo além do parcial", 0, errors.New("i/o timeout"), 5 * time.Second, outcomePartial, 0},
		{"timeout total", 0, context.DeadlineExceeded, total, outcomeTotal, 0},
		{"bind em IP não local", 0, wrapped(syscall.EADDRNOTAVAIL), time.Millisecond, outcomeGenerator, 0},
		{"família não suportada", 0, wrapped(syscall.EAFNOSUPPORT), time.Millisecond, outcomeGenerator, 0},
		{"sem descritores", 0, wrapped(syscall.EMFILE), time.Millisecond, outcomeGenerator, 0},
		{"sem descritores no sistema", 0, wrapped(syscall.ENFILE), time.Millisecond, outcomeGenerator, 0},
	}
	for _, c := range cases {
		status, got := classify(c.status, c.err, c.elapsed, partial, total)
		if got != c.want || status != c.wantStatus {
			t.Errorf("%s: got (%d, %q) want (%d, %q)", c.name, status, got, c.wantStatus, c.want)
		}
	}
}

func TestClassifyDropsStatusOnError(t *testing.T) {
	if status, _ := classify(200, errors.New("boom"), time.Second, 4*time.Second, 60*time.Second); status != 0 {
		t.Fatalf("erro devia zerar o status, veio %d", status)
	}
}
