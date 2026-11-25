package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type k3Server struct {
	srv      *httptest.Server
	mu       sync.Mutex
	issued   map[string]bool
	calls    int
	delay    time.Duration
	firstAPI time.Time
}

func (k *k3Server) firstAPITime() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.firstAPI
}

func newK3Server(t *testing.T) *k3Server {
	t.Helper()
	k := &k3Server{issued: map[string]bool{}}
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		if r.URL.Path == defaultTokenPath {
			cid := r.URL.Query().Get("cid")
			if !cidPattern.MatchString(cid) {
				http.Error(w, "cid inválido", http.StatusBadRequest)
				return
			}
			k.calls++
			tok := "tok-" + cid
			k.issued[tok] = true
			time.Sleep(k.delay)
			w.Write([]byte(tok))
			return
		}
		if k.firstAPI.IsZero() {
			k.firstAPI = time.Now()
		}
		if !k.issued[r.Header.Get(tokenHeader)] {
			http.Error(w, "sem token", http.StatusUnauthorized)
		}
	}))
	t.Cleanup(k.srv.Close)
	return k
}

func executeCfg(t *testing.T, srvURL string) (config, string) {
	t.Helper()
	cfg := defaults()
	cfg.target = srvURL
	cfg.out = filepath.Join(t.TempDir(), "attacker.jsonl")
	cfg.rate = 400
	cfg.duration = 0.25
	cfg.tokenMode = tokenFarm
	cfg.tokenBudget = 4
	cfg.partial = cfg.total
	return cfg, cfg.out
}

type outputRecord struct {
	TStart  float64 `json:"t_start"`
	TEnd    float64 `json:"t_end"`
	Status  int     `json:"status"`
	Outcome string  `json:"outcome"`
}

func readRecords(t *testing.T, path string) []outputRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var records []outputRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r outputRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}

func readOutcomes(t *testing.T, path string) map[string]int {
	t.Helper()
	outcomes := map[string]int{}
	for _, r := range readRecords(t, path) {
		outcomes[r.Outcome]++
	}
	return outcomes
}

func earliestStart(t *testing.T, path string) float64 {
	t.Helper()
	records := readRecords(t, path)
	if len(records) == 0 {
		t.Fatalf("saída %s sem registros", path)
	}
	earliest := records[0].TStart
	for _, r := range records[1:] {
		earliest = min(earliest, r.TStart)
	}
	return earliest
}

func TestExecuteFarmFollowsTokenContract(t *testing.T) {
	k3 := newK3Server(t)
	cfg, out := executeCfg(t, k3.srv.URL)
	target, err := validateTarget(cfg.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := execute(context.Background(), cfg, target, loopbackPool(3), testMaxInflight); err != nil {
		t.Fatal(err)
	}
	if got := readOutcomes(t, out); got[outcomeOK] != 100 || len(got) != 1 {
		t.Fatalf("desfechos %v, esperava 100 ok (token aceito em todas)", got)
	}
	if k3.calls != cfg.tokenBudget {
		t.Fatalf("/token chamado %d vezes, orçamento era %d", k3.calls, cfg.tokenBudget)
	}
}

func TestExecuteAbortsOnTokenFailureWithoutRecords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "cid ausente ou inválido", http.StatusBadRequest)
	}))
	defer srv.Close()
	cfg, out := executeCfg(t, srv.URL)
	target, _ := validateTarget(cfg.target)
	err := execute(context.Background(), cfg, target, loopbackPool(2), testMaxInflight)
	if err == nil {
		t.Fatal("falha de emissão de token devia abortar a execução")
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("execução inválida deixou saída em %s (%v)", out, statErr)
	}
}

func TestExecutePrefetchFailureKeepsReasonBeforeStartAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == defaultTokenPath {
			http.Error(w, "limite", http.StatusTooManyRequests)
		}
	}))
	t.Cleanup(srv.Close)
	cfg, out := executeCfg(t, srv.URL)
	cfg.startAt = time.Now().Add(2 * time.Second).UnixMilli()
	target, _ := validateTarget(cfg.target)
	err := execute(context.Background(), cfg, target, loopbackPool(3), testMaxInflight)
	if err == nil || !strings.Contains(err.Error(), "emissão limitada") {
		t.Fatalf("falha de emissão antes do --start-at devia manter a causa, veio %v", err)
	}
	if strings.Contains(err.Error(), "janela") {
		t.Fatalf("falha dentro da janela virou erro de janela: %v", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("saída criada apesar do abort: %v", statErr)
	}
}

func TestExecuteRejectsUnusablePoolBeforeSending(t *testing.T) {
	cases := []struct {
		name string
		pool []net.IP
		want string
	}{
		{"IP não local", []net.IP{net.ParseIP("192.0.2.1")}, "não é local"},
		{"família diferente do alvo", []net.IP{net.ParseIP("fd12:3456:789a::1")}, "famílias de endereço diferentes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k3 := newK3Server(t)
			cfg, out := executeCfg(t, k3.srv.URL)
			cfg.tokenMode = tokenNone
			target, _ := validateTarget(cfg.target)
			err := execute(context.Background(), cfg, target, c.pool, testMaxInflight)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("pool inválido devia abortar com %q, veio %v", c.want, err)
			}
			if k3.calls != 0 {
				t.Fatal("a checagem do pool devia vir antes de qualquer pedido")
			}
			if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("saída criada apesar do abort: %v", statErr)
			}
		})
	}
}

func TestExecuteSaturationPreservesSenderError(t *testing.T) {
	srv, _ := hungServer(t)
	cfg, _ := executeCfg(t, srv.URL)
	cfg.tokenMode = tokenNone
	cfg.duration = 0.2
	target, _ := validateTarget(cfg.target)
	err := execute(context.Background(), cfg, target, loopbackPool(2), 1)
	if !errors.Is(err, errSaturated) {
		t.Fatalf("saturação devia devolver errSaturated, veio %v", err)
	}
}

func TestExecutePrefetchDoesNotShiftStart(t *testing.T) {
	k3 := newK3Server(t)
	k3.delay = 150 * time.Millisecond
	cfg, out := executeCfg(t, k3.srv.URL)
	cfg.tokenBudget = 3
	cfg.duration = 0.2
	target, _ := validateTarget(cfg.target)
	if err := execute(context.Background(), cfg, target, loopbackPool(3), testMaxInflight); err != nil {
		t.Fatal(err)
	}
	if earliest := earliestStart(t, out); earliest < 0 || earliest >= 0.1 {
		t.Fatalf("primeiro t_start em %v: o prefetch deslocou o tempo zero", earliest)
	}
}

func TestExecuteStartAtDefinesZero(t *testing.T) {
	k3 := newK3Server(t)
	cfg, out := executeCfg(t, k3.srv.URL)
	cfg.startAt = time.Now().Add(400 * time.Millisecond).UnixMilli()
	target, _ := validateTarget(cfg.target)
	if err := execute(context.Background(), cfg, target, loopbackPool(3), testMaxInflight); err != nil {
		t.Fatal(err)
	}
	startAt := time.UnixMilli(cfg.startAt)
	first := k3.firstAPITime()
	if first.Before(startAt) || first.Sub(startAt) > 300*time.Millisecond {
		t.Fatalf("primeiro /api em %v, esperava em [%v, %v]", first, startAt, startAt.Add(300*time.Millisecond))
	}
	if earliest := earliestStart(t, out); earliest < 0 || earliest > 0.3 {
		t.Fatalf("primeiro t_start em %v, esperava ~0 desde o --start-at", earliest)
	}
}

func TestExecuteStartAtCancelDuringMargin(t *testing.T) {
	k3 := newK3Server(t)
	cfg, out := executeCfg(t, k3.srv.URL)
	cfg.tokenMode = tokenNone
	cfg.startAt = time.Now().Add(5 * time.Second).UnixMilli()
	target, _ := validateTarget(cfg.target)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	begin := time.Now()
	err := execute(ctx, cfg, target, loopbackPool(2), testMaxInflight)
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("cancelamento durante a margem demorou %v", elapsed)
	}
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("cancelamento durante a margem devia devolver errInterrupted, veio %v", err)
	}
	if got := readOutcomes(t, out); len(got) != 0 {
		t.Fatalf("cancelamento durante a margem gravou registros: %v", got)
	}
}

func TestExecuteStartAtWindowExceeded(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		delay  time.Duration
		offset time.Duration
		want   string
	}{
		{"vencido com prefetch lento", tokenFarm, 150 * time.Millisecond, -200 * time.Millisecond, "prefetch de tokens excedeu a janela de --start-at"},
		{"vencido sem prefetch", tokenNone, 0, -200 * time.Millisecond, "--start-at já passou"},
		{"janela curta com prefetch lento", tokenFarm, 150 * time.Millisecond, 100 * time.Millisecond, "prefetch de tokens excedeu a janela de --start-at"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k3 := newK3Server(t)
			k3.delay = c.delay
			cfg, out := executeCfg(t, k3.srv.URL)
			cfg.tokenMode = c.mode
			cfg.startAt = time.Now().Add(c.offset).UnixMilli()
			target, _ := validateTarget(cfg.target)
			err := execute(context.Background(), cfg, target, loopbackPool(3), testMaxInflight)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("start-at %v devia abortar com %q, veio %v", c.offset, c.want, err)
			}
			if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("saída criada apesar do abort: %v", statErr)
			}
		})
	}
}

func TestExecuteInterruptedExitsWithErrorAfterFlush(t *testing.T) {
	k3 := newK3Server(t)
	cfg, out := executeCfg(t, k3.srv.URL)
	cfg.tokenMode = tokenNone
	target, _ := validateTarget(cfg.target)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := execute(ctx, cfg, target, loopbackPool(2), testMaxInflight)
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("interrupção devia devolver errInterrupted, veio %v", err)
	}
	if got := readOutcomes(t, out); len(got) != 0 {
		t.Fatalf("interrupção antes do envio gravou registros: %v", got)
	}
}
