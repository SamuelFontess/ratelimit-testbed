package main

import (
	"context"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var cidPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type tokenStub struct {
	srv     *httptest.Server
	mu      sync.Mutex
	cids    []string
	paths   []string
	remotes map[string]string
	status  int
	body    string
}

func newTokenStub(t *testing.T) *tokenStub {
	t.Helper()
	stub := &tokenStub{remotes: map[string]string{}}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid := r.URL.Query().Get("cid")
		if !cidPattern.MatchString(cid) {
			http.Error(w, "cid ausente ou inválido", http.StatusBadRequest)
			return
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		stub.mu.Lock()
		stub.cids = append(stub.cids, cid)
		stub.paths = append(stub.paths, r.URL.Path)
		stub.remotes[cid] = host
		status, body := stub.status, stub.body
		stub.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
		}
		if body != "" {
			w.Write([]byte(body))
			return
		}
		w.Write([]byte("tok-" + cid))
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (s *tokenStub) hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cids)
}

func tokenCfg(mode string, budget int) config {
	cfg := defaults()
	cfg.seed = 7
	cfg.tokenMode = mode
	cfg.tokenBudget = budget
	return cfg
}

func acquire(t *testing.T, stub *tokenStub, cfg config, pool []net.IP) (*tokenSource, error) {
	t.Helper()
	cache := newTransportCache(false)
	t.Cleanup(cache.closeAll)
	return acquireTokens(context.Background(), cfg, stub.srv.URL, pool, cache)
}

func TestFarmFetchesExactlyBudgetTokensFromDistinctSources(t *testing.T) {
	stub := newTokenStub(t)
	pool := loopbackPool(2)
	ts, err := acquire(t, stub, tokenCfg(tokenFarm, 3), pool)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stub.cids, []string{"atk-7-0", "atk-7-1", "atk-7-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cids pedidos %v, esperava %v", got, want)
	}
	for i, cid := range stub.cids {
		if want := pool[i%len(pool)].String(); stub.remotes[cid] != want {
			t.Errorf("token %s saiu de %s, esperava %s", cid, stub.remotes[cid], want)
		}
		if got, want := ts.tokens[i], "tok-"+cid; got != want {
			t.Errorf("token %d = %q, esperava %q", i, got, want)
		}
	}
	for seq := range 100 {
		if got, want := ts.forRequest(seq), ts.tokens[seq%3]; got != want {
			t.Fatalf("requisição %d usou %q, esperava rodízio %q", seq, got, want)
		}
	}
	if got := stub.hits(); got != 3 {
		t.Fatalf("/token chamado %d vezes, orçamento era 3", got)
	}
}

func TestFarmSpreadsRequestsAcrossAllTokens(t *testing.T) {
	stub := newTokenStub(t)
	ts, err := acquire(t, stub, tokenCfg(tokenFarm, 5), loopbackPool(2))
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]int{}
	for seq := range 500 {
		used[ts.forRequest(seq)]++
	}
	if len(used) != 5 {
		t.Fatalf("usou %d tokens distintos, esperava 5", len(used))
	}
	for tok, n := range used {
		if n != 100 {
			t.Errorf("token %s usado %d vezes, esperava 100", tok, n)
		}
	}
}

func TestValidReuseFetchesOnceFromFirstSource(t *testing.T) {
	stub := newTokenStub(t)
	pool := loopbackPool(3)
	ts, err := acquire(t, stub, tokenCfg(tokenValidReuse, 0), pool)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stub.cids, []string{"atk-7-0"}) {
		t.Fatalf("cids pedidos %v", stub.cids)
	}
	if stub.remotes["atk-7-0"] != pool[0].String() {
		t.Fatalf("valid-reuse saiu de %s, esperava %s", stub.remotes["atk-7-0"], pool[0])
	}
	for seq := range 50 {
		if ts.forRequest(seq) != "tok-atk-7-0" {
			t.Fatalf("requisição %d usou outro token", seq)
		}
	}
}

func TestTokenNoneNeverCalls(t *testing.T) {
	stub := newTokenStub(t)
	ts, err := acquire(t, stub, tokenCfg(tokenNone, 0), loopbackPool(1))
	if err != nil {
		t.Fatal(err)
	}
	if ts.forRequest(0) != "" || stub.hits() != 0 {
		t.Fatalf("none devia devolver token vazio sem chamar /token (hits=%d)", stub.hits())
	}
}

func TestTokenPathIsAppendedToBase(t *testing.T) {
	stub := newTokenStub(t)
	cfg := tokenCfg(tokenValidReuse, 0)
	cfg.tokenPath = "/v1/token"
	if _, err := acquire(t, stub, cfg, loopbackPool(1)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stub.paths, []string{"/v1/token"}) {
		t.Fatalf("caminhos pedidos: %v", stub.paths)
	}
}

func TestTokenIssuanceFailureAbortsWithContext(t *testing.T) {
	for _, mode := range []string{tokenValidReuse, tokenFarm} {
		stub := newTokenStub(t)
		stub.status = http.StatusBadRequest
		stub.body = "cid inválido"
		ts, err := acquire(t, stub, tokenCfg(mode, 4), loopbackPool(2))
		if err == nil || ts != nil {
			t.Fatalf("%s: falha de emissão devia abortar, veio %v", mode, err)
		}
		if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "127.0.0.2") {
			t.Errorf("%s: erro sem status ou IP de origem: %v", mode, err)
		}
		if stub.hits() != 1 {
			t.Errorf("%s: %d chamadas, devia parar na primeira falha", mode, stub.hits())
		}
	}
}

func TestTokenEmptyBodyIsFailure(t *testing.T) {
	stub := newTokenStub(t)
	stub.body = "  \n"
	if _, err := acquire(t, stub, tokenCfg(tokenValidReuse, 0), loopbackPool(1)); err == nil {
		t.Fatal("token vazio devia falhar")
	}
}

func TestTokenIssuanceDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.Write([]byte("tok-fora"))
	}))
	defer other.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer redirecting.Close()

	cfg := tokenCfg(tokenValidReuse, 0)
	cache := newTransportCache(false)
	defer cache.closeAll()
	_, err := acquireTokens(context.Background(), cfg, redirecting.URL, loopbackPool(1), cache)
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirect devia virar erro 302, veio %v", err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("seguiu o redirect para outro host")
	}
}

func TestTokenCIDMatchesContract(t *testing.T) {
	for _, seed := range []int64{0, 1, -5, math.MaxInt64, math.MinInt64} {
		for _, idx := range []int{0, 7, 9999999} {
			if cid := tokenCID(seed, idx); !cidPattern.MatchString(cid) {
				t.Errorf("cid %q fora de %s", cid, cidPattern)
			}
		}
	}
	if tokenCID(1, 0) == tokenCID(1, 1) || tokenCID(1, 0) == tokenCID(2, 0) {
		t.Fatal("cid devia mudar com a semente e com o índice")
	}
}

func TestFetchTokenRejectsOversizedOrMalformedBody(t *testing.T) {
	const contractMax = 4096
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"no limite", strings.Repeat("a", contractMax), true},
		{"acima do limite", strings.Repeat("a", contractMax+1), false},
		{"espaço no interior", "abc def", false},
		{"controle no interior", "abc\x00def", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := newTokenStub(t)
			stub.body = c.body
			cache := newTransportCache(false)
			defer cache.closeAll()
			tok, err := fetchToken(context.Background(), cache.get(net.ParseIP("127.0.0.2")), stub.srv.URL+"/token", "atk-1-0", time.Second)
			if !c.ok {
				if err == nil {
					t.Fatalf("corpo de %d bytes devia ser recusado", len(c.body))
				}
				return
			}
			if err != nil || tok != c.body {
				t.Fatalf("corpo de %d bytes recusado ou truncado (%v)", len(c.body), err)
			}
		})
	}
}

func TestTokenIssuance429NamesBudgetAndE(t *testing.T) {
	stub := newTokenStub(t)
	stub.status = http.StatusTooManyRequests
	_, err := acquire(t, stub, tokenCfg(tokenFarm, 2), loopbackPool(1))
	if err == nil || !strings.Contains(err.Error(), "emissão limitada: reduza --token-budget ou aumente E") {
		t.Fatalf("429 devia apontar orçamento e E, veio %v", err)
	}
}
