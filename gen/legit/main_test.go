package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	testPartial              = 200 * time.Millisecond
	testTotal                = 600 * time.Millisecond
	generousTimeout          = 5 * time.Second
	contractPartial          = 4 * time.Second
	contractTotal            = 60 * time.Second
	contractTokenAge         = 270 * time.Second
	contractTokenConcurrency = 16
	contractMaxTokenBytes    = 4096
	testTokenConcurrency     = 4
	tokenPrefetchWindow      = 600 * time.Millisecond
	retryPrefetchWindow      = 2 * time.Second
	secondEventAt            = 0.02
	farFutureEvent           = 1e6
	maxLateMS                = 500.0
	trickleInterval          = 10 * time.Millisecond
	groupControl             = "control"
	groupSameIP              = "same_ip"
	unusableSource           = "192.0.2.1"
	tokenPath                = "/token"
)

const (
	srcOK           = "127.0.0.2"
	srcTooMany      = "127.0.0.3"
	srcUnauthorized = "127.0.0.4"
	srcSilent       = "127.0.0.5"
	srcStalled      = "127.0.0.6"
	srcTrickle      = "127.0.0.7"
	srcReset        = "127.0.0.8"
	srcEOF          = "127.0.0.9"
)

var recordKeys = []string{"cid", "group", "late_ms", "outcome", "status", "t_end", "t_plan", "t_start"}

func hostOf(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func testSchedule(srcs []string, groups []string, times []float64) Schedule {
	var s Schedule
	for i, src := range srcs {
		s.Clients = append(s.Clients, Client{CID: i, Src: src, Group: groups[i]})
		s.Events = append(s.Events, Event{T: times[i], CID: i})
	}
	return s
}

func writeSchedule(t *testing.T, s Schedule) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agenda.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func dispatch(t *testing.T, cfg Config) (*dispatcher, []Record) {
	t.Helper()
	d, events := dispatcherFor(t, cfg)
	return d, d.run(events)
}

func dispatcherFor(t *testing.T, cfg Config) (*dispatcher, []Event) {
	t.Helper()
	sched, err := loadSchedule(cfg.Schedule)
	if err != nil {
		t.Fatal(err)
	}
	d, err := newDispatcher(cfg, sched)
	if err != nil {
		t.Fatal(err)
	}
	return d, sched.Events
}

func prepare(t *testing.T, target string, s Schedule, partial, total time.Duration) Config {
	t.Helper()
	return Config{
		Target:           target,
		Schedule:         writeSchedule(t, s),
		Out:              filepath.Join(t.TempDir(), "legit.jsonl"),
		Partial:          partial,
		Total:            total,
		TokenConcurrency: testTokenConcurrency,
	}
}

func execute(t *testing.T, cfg Config) []Record {
	t.Helper()
	if err := run(cfg); err != nil {
		t.Fatal(err)
	}
	return readRecords(t, cfg.Out)
}

func runSchedule(t *testing.T, target string, s Schedule, partial, total time.Duration) []Record {
	t.Helper()
	return execute(t, prepare(t, target, s, partial, total))
}

func runAborting(t *testing.T, cfg Config) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run(cfg) }()
	select {
	case err := <-done:
		return err
	case <-time.After(generousTimeout):
		t.Fatal("run nao abortou antes de disparar eventos")
		return nil
	}
}

func readRecords(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var records []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("linha invalida %q: %v", sc.Text(), err)
		}
		records = append(records, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestOpenLoopSecondEventFiresWhileFirstIsPending(t *testing.T) {
	secondArrived := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hostOf(r) == srcOK {
			select {
			case <-secondArrived:
			case <-r.Context().Done():
			}
		} else {
			close(secondArrived)
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	s := testSchedule([]string{srcOK, srcTooMany}, []string{groupSameIP, groupControl}, []float64{0, secondEventAt})
	records := runSchedule(t, srv.URL, s, generousTimeout, generousTimeout)

	first, second := records[0], records[1]
	if first.Outcome != OutcomeOK || second.Outcome != OutcomeOK {
		t.Fatalf("outcomes %q e %q, esperado ok nos dois", first.Outcome, second.Outcome)
	}
	if second.TStart >= first.TEnd {
		t.Fatalf("segundo saiu em %v, depois do fim do primeiro em %v: laco fechado", second.TStart, first.TEnd)
	}
	if second.TStart < secondEventAt || second.LateMS > maxLateMS {
		t.Fatalf("segundo evento fora do t_plan: t_start=%v late_ms=%v", second.TStart, second.LateMS)
	}
}

func TestOutcomesLateMSGroupAndSourceBind(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[hostOf(r)] = true
		mu.Unlock()
		serveBehavior(t, w, r)
	}))
	defer srv.Close()

	cases := []struct {
		src     string
		group   string
		status  int
		outcome string
	}{
		{srcOK, groupSameIP, http.StatusOK, OutcomeOK},
		{srcTooMany, groupControl, http.StatusTooManyRequests, OutcomeRefused},
		{srcUnauthorized, groupControl, http.StatusUnauthorized, OutcomeRefused},
		{srcSilent, groupSameIP, noResponseStatus, OutcomePartial},
		{srcStalled, groupSameIP, http.StatusOK, OutcomePartial},
		{srcTrickle, groupControl, http.StatusOK, OutcomeTotal},
		{srcReset, groupControl, noResponseStatus, OutcomeRefused},
		{srcEOF, groupControl, noResponseStatus, OutcomeTotal},
	}
	var srcs, groups []string
	var times []float64
	for i, c := range cases {
		srcs = append(srcs, c.src)
		groups = append(groups, c.group)
		times = append(times, float64(i)*0.001)
	}
	records := runSchedule(t, srv.URL, testSchedule(srcs, groups, times), testPartial, testTotal)

	if len(records) != len(cases) {
		t.Fatalf("%d registros, esperado %d", len(records), len(cases))
	}
	for i, c := range cases {
		r := records[i]
		if r.Status != c.status || r.Outcome != c.outcome || r.Group != c.group || r.CID != i {
			t.Errorf("%s: got status=%d outcome=%s group=%s cid=%d, esperado status=%d outcome=%s group=%s cid=%d",
				c.src, r.Status, r.Outcome, r.Group, r.CID, c.status, c.outcome, c.group, i)
		}
		if r.TPlan != times[i] {
			t.Errorf("%s: t_plan=%v, esperado exatamente %v da agenda", c.src, r.TPlan, times[i])
		}
		if r.TEnd < r.TStart || r.TStart < r.TPlan {
			t.Errorf("%s: tempos incoerentes %+v", c.src, r)
		}
		if wantLate := (r.TStart - r.TPlan) * 1000; r.LateMS != wantLate {
			t.Errorf("%s: late_ms=%v, esperado %v ((t_start-t_plan)*1000)", c.src, r.LateMS, wantLate)
		}
		mu.Lock()
		ok := seen[c.src]
		mu.Unlock()
		if !ok {
			t.Errorf("%s: servidor nunca viu essa origem", c.src)
		}
	}
}

func serveBehavior(t *testing.T, w http.ResponseWriter, r *http.Request) {
	switch hostOf(r) {
	case srcTooMany:
		w.WriteHeader(http.StatusTooManyRequests)
	case srcUnauthorized:
		w.WriteHeader(http.StatusUnauthorized)
	case srcSilent:
		<-r.Context().Done()
	case srcStalled:
		w.Write([]byte("ok"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case srcTrickle:
		trickle(w, r)
	case srcReset:
		closeConn(t, w, true)
	case srcEOF:
		closeConn(t, w, false)
	default:
		w.Write([]byte("ok"))
	}
}

func trickle(w http.ResponseWriter, r *http.Request) {
	ticker := time.NewTicker(trickleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			w.Write([]byte("."))
			w.(http.Flusher).Flush()
		}
	}
}

func closeConn(t *testing.T, w http.ResponseWriter, reset bool) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	if tcp, ok := conn.(*net.TCPConn); ok && reset {
		if err := tcp.SetLinger(0); err != nil {
			t.Error(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Error(err)
	}
}

func TestRecordJSONHasExactlyTheContractKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := prepare(t, srv.URL, testSchedule([]string{srcOK}, []string{groupControl}, []float64{0}), generousTimeout, generousTimeout)
	execute(t, cfg)

	raw, err := os.ReadFile(cfg.Out)
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := bytes.Cut(raw, []byte("\n"))
	var decoded map[string]any
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(decoded))
	for k := range decoded {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, recordKeys) {
		t.Fatalf("chaves %v, esperado %v", keys, recordKeys)
	}
}

func TestSameScheduleTwiceKeepsCIDAndTPlan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	const clients, events = 3, 12
	var s Schedule
	for cid := range clients {
		s.Clients = append(s.Clients, Client{CID: cid, Src: fmt.Sprintf("127.0.0.%d", 2+cid), Group: groupControl})
	}
	for i := range events {
		s.Events = append(s.Events, Event{T: float64(i) * 0.001, CID: i % clients})
	}

	c1 := runSchedule(t, srv.URL, s, generousTimeout, generousTimeout)
	c4 := runSchedule(t, srv.URL, s, generousTimeout, generousTimeout)

	if len(c1) != events || len(c4) != events {
		t.Fatalf("%d e %d registros, esperado %d", len(c1), len(c4), events)
	}
	for i, e := range s.Events {
		for name, r := range map[string]Record{"C1": c1[i], "C4": c4[i]} {
			if r.CID != e.CID || r.TPlan != e.T {
				t.Errorf("%s evento %d: (cid=%d,t_plan=%v), esperado (cid=%d,t_plan=%v)", name, i, r.CID, r.TPlan, e.CID, e.T)
			}
		}
	}
}

func TestConnectionRefusedIsFailRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := "http://" + ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	s := testSchedule([]string{srcOK}, []string{groupControl}, []float64{0})
	records := runSchedule(t, target, s, generousTimeout, generousTimeout)
	if records[0].Outcome != OutcomeRefused || records[0].Status != noResponseStatus {
		t.Fatalf("got %+v", records[0])
	}
}

func TestClassifyStatusBoundaries(t *testing.T) {
	cases := map[int]string{
		199: OutcomeRefused,
		200: OutcomeOK,
		204: OutcomeOK,
		299: OutcomeOK,
		300: OutcomeRefused,
		500: OutcomeRefused,
	}
	for status, want := range cases {
		if got := classifyStatus(status); got != want {
			t.Errorf("status %d: got %s, esperado %s", status, got, want)
		}
	}
}

func TestClassifyErrorMapsContractCases(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	dial := func(errno syscall.Errno) error {
		return &net.OpError{Op: "write", Err: os.NewSyscallError("write", errno)}
	}
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"contexto encerrado", canceled, os.ErrDeadlineExceeded, OutcomeTotal},
		{"econnrefused", context.Background(), dial(syscall.ECONNREFUSED), OutcomeRefused},
		{"econnreset", context.Background(), dial(syscall.ECONNRESET), OutcomeRefused},
		{"epipe", context.Background(), dial(syscall.EPIPE), OutcomeRefused},
		{"timeout de rede", context.Background(), os.ErrDeadlineExceeded, OutcomePartial},
		{"eof sem dados", context.Background(), io.EOF, OutcomeTotal},
		{"erro desconhecido", context.Background(), errors.New("x"), OutcomeTotal},
	}
	for _, c := range cases {
		if got := classifyError(c.ctx, c.err); got != c.want {
			t.Errorf("%s: got %s, esperado %s", c.name, got, c.want)
		}
	}
}

func TestLoadScheduleRejectsInconsistentFiles(t *testing.T) {
	cases := map[string]Schedule{
		"cid desconhecido": {Clients: []Client{{CID: 0, Src: srcOK}}, Events: []Event{{T: 0, CID: 1}}},
		"fora de ordem":    {Clients: []Client{{CID: 0, Src: srcOK}}, Events: []Event{{T: 1, CID: 0}, {T: 0.5, CID: 0}}},
		"src invalido":     {Clients: []Client{{CID: 0, Src: "nao-e-ip"}}},
	}
	for name, s := range cases {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "agenda.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSchedule(path); err == nil {
			t.Errorf("%s: esperado erro", name)
		}
	}
}

func TestParseFlagsAcceptsContractFlags(t *testing.T) {
	args := strings.Fields("--seed 1 --scenario s1 --target http://x --duration 120 --warmup 30 --rate 10 --schedule a.json --out l.jsonl")
	cfg, err := parseFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Partial != contractPartial || cfg.Total != contractTotal || cfg.Target != "http://x" || cfg.TokenPath != "" {
		t.Fatalf("got %+v", cfg)
	}
	if cfg.TokenConcurrency != contractTokenConcurrency || cfg.StartAt != 0 {
		t.Fatalf("token: concorrencia=%d start-at=%d, esperado %d e 0", cfg.TokenConcurrency, cfg.StartAt, contractTokenConcurrency)
	}
	cfg, err = parseFlags(strings.Fields("--target http://x --schedule a.json --out l.jsonl --token-path /token --token-concurrency 3 --start-at 1700000000000 --partial-timeout 1s --total-timeout 2s"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TokenPath != tokenPath || cfg.TokenConcurrency != 3 || cfg.StartAt != 1700000000000 || cfg.Partial != time.Second || cfg.Total != 2*time.Second {
		t.Fatalf("got %+v", cfg)
	}
}

func TestParseFlagsRejectsInvalidValues(t *testing.T) {
	const required = "--target http://x --schedule a.json --out l.jsonl "
	cases := map[string]string{
		"sem schedule e out":     "--target http://x",
		"partial zero":           required + "--partial-timeout 0",
		"total negativo":         required + "--total-timeout -1s",
		"token sem barra":        required + "--token-path token",
		"concorrencia zero":      required + "--token-concurrency 0",
		"start-at negativo":      required + "--start-at -1",
		"start-at longe demais":  required + fmt.Sprintf("--start-at %d", time.Now().Add(2*time.Hour).UnixMilli()),
		"flag desconhecida":      required + "--nao-existe",
		"valor de flag invalido": required + "--partial-timeout abc",
	}
	for name, args := range cases {
		if _, err := parseFlags(strings.Fields(args)); err == nil {
			t.Errorf("%s: esperado erro", name)
		}
	}
}

func TestParseFlagsMarksOnlyFlagPackageErrors(t *testing.T) {
	_, err := parseFlags([]string{"--nao-existe"})
	if !errors.Is(err, errFlagParse) {
		t.Errorf("erro do pacote flag sem marca: %v", err)
	}
	_, err = parseFlags([]string{"--target", "http://x"})
	if err == nil || errors.Is(err, errFlagParse) {
		t.Errorf("erro de validacao com marca de flag: %v", err)
	}
}

func TestUnusableSourceAbortsBeforeFiring(t *testing.T) {
	cases := map[string]struct {
		src    string
		target string
	}{
		"origem nao local":  {unusableSource, "http://127.0.0.1:1"},
		"familia diferente": {"::1", "http://127.0.0.1:1"},
	}
	for name, c := range cases {
		s := testSchedule([]string{c.src}, []string{groupControl}, []float64{farFutureEvent})
		err := runAborting(t, prepare(t, c.target, s, testPartial, testTotal))
		if err == nil || !strings.Contains(err.Error(), c.src) {
			t.Errorf("%s: esperado erro citando %s, got %v", name, c.src, err)
		}
	}
}

func TestInvalidOutputFailsBeforeFiring(t *testing.T) {
	s := testSchedule([]string{srcOK}, []string{groupControl}, []float64{farFutureEvent})
	cfg := prepare(t, "http://127.0.0.1:1", s, testPartial, testTotal)
	cfg.Out = filepath.Join(t.TempDir(), "inexistente", "legit.jsonl")
	if err := runAborting(t, cfg); err == nil {
		t.Fatal("esperado erro de saida invalida")
	}
}

type tokenAPI struct {
	tokenCalls  atomic.Int64
	apiCalls    atomic.Int64
	failToken   atomic.Bool
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
	reply       func(n int64) (int, string)
	tokenDelay  time.Duration
	stallFrom   int64
	onAPI       func()
	mu          sync.Mutex
	tokenHosts  []string
	apiTokens   []string
	order       []string
}

func (a *tokenAPI) note(kind string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.order = append(a.order, kind)
}

func (a *tokenAPI) serveToken(w http.ResponseWriter, r *http.Request) {
	n := a.tokenCalls.Add(1)
	current := a.inFlight.Add(1)
	defer a.inFlight.Add(-1)
	for peak := a.maxInFlight.Load(); current > peak && !a.maxInFlight.CompareAndSwap(peak, current); peak = a.maxInFlight.Load() {
	}
	a.note("token")
	a.mu.Lock()
	a.tokenHosts = append(a.tokenHosts, hostOf(r))
	a.mu.Unlock()
	if a.stallFrom > 0 && n >= a.stallFrom {
		<-r.Context().Done()
		return
	}
	time.Sleep(a.tokenDelay)
	if a.failToken.Load() {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if a.reply != nil {
		status, body := a.reply(n)
		w.WriteHeader(status)
		io.WriteString(w, body)
		return
	}
	fmt.Fprintf(w, "tok-%s-%d\n", r.URL.Query().Get("cid"), n)
}

func (a *tokenAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case tokenPath:
		a.serveToken(w, r)
	case apiPath:
		a.apiCalls.Add(1)
		a.note("api")
		token := r.Header.Get("X-Token")
		a.mu.Lock()
		a.apiTokens = append(a.apiTokens, token)
		a.mu.Unlock()
		if a.onAPI != nil {
			a.onAPI()
		}
		if token == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("ok"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (a *tokenAPI) tokenCallAfterAPI() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	seenAPI := false
	for _, kind := range a.order {
		if kind == "api" {
			seenAPI = true
		} else if seenAPI {
			return true
		}
	}
	return false
}

func clientEvents(cid, n int) []Event {
	events := make([]Event, n)
	for i := range events {
		events[i] = Event{T: float64(i) * 0.001, CID: cid}
	}
	return events
}

func singleClient(src string, events []Event) Schedule {
	return Schedule{Clients: []Client{{CID: 0, Src: src, Group: groupControl}}, Events: events}
}

func tokenConfig(t *testing.T, target string, s Schedule) Config {
	t.Helper()
	cfg := prepare(t, target, s, generousTimeout, generousTimeout)
	cfg.TokenPath = tokenPath
	return cfg
}

func TestWithoutTokenPathNeverCallsTokenEndpoint(t *testing.T) {
	api := &tokenAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()

	records := runSchedule(t, srv.URL, singleClient(srcOK, clientEvents(0, 3)), generousTimeout, generousTimeout)

	if api.tokenCalls.Load() != 0 {
		t.Fatalf("%d chamadas a /token sem --token-path", api.tokenCalls.Load())
	}
	for _, r := range records {
		if r.Status != http.StatusUnauthorized || r.Outcome != OutcomeRefused {
			t.Errorf("sem token a API deve responder 401: %+v", r)
		}
	}
}

func TestTokenFetchedOncePerClientAndSentOnEveryEvent(t *testing.T) {
	api := &tokenAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	const events = 6
	s := Schedule{
		Clients: []Client{{CID: 7, Src: srcOK, Group: groupControl}, {CID: 8, Src: srcTooMany, Group: groupControl}},
		Events:  append(clientEvents(7, events), clientEvents(8, events)...),
	}
	sort.SliceStable(s.Events, func(i, j int) bool { return s.Events[i].T < s.Events[j].T })

	records := execute(t, tokenConfig(t, srv.URL, s))

	if len(records) != 2*events {
		t.Fatalf("%d registros, esperado %d: a busca de token nao e transacao", len(records), 2*events)
	}
	for _, r := range records {
		if r.Outcome != OutcomeOK || r.Status != http.StatusOK {
			t.Errorf("evento sem sucesso com token: %+v", r)
		}
	}
	if got := api.tokenCalls.Load(); got != 2 {
		t.Fatalf("%d buscas de token, esperado 1 por cliente (2)", got)
	}
	sort.Strings(api.tokenHosts)
	if want := []string{srcOK, srcTooMany}; !reflect.DeepEqual(api.tokenHosts, want) {
		t.Fatalf("buscas sairam de %v, esperado o bind de cada cliente %v", api.tokenHosts, want)
	}
	if api.tokenCallAfterAPI() {
		t.Fatal("busca de token depois do primeiro /api")
	}
	counts := map[string]int{}
	for _, tok := range api.apiTokens {
		counts[tok]++
	}
	for _, cid := range []string{"7", "8"} {
		total, distinct := 0, 0
		for tok, n := range counts {
			if strings.HasPrefix(tok, "tok-"+cid+"-") {
				total += n
				distinct++
			}
		}
		if total != events || distinct != 1 {
			t.Fatalf("cliente %s: %d usos de %d tokens distintos, esperado %d usos de 1 token (%v)", cid, total, distinct, events, counts)
		}
	}
}

func TestSlowTokenEndpointDoesNotDelayEvents(t *testing.T) {
	const tokenDelay = 500 * time.Millisecond
	api := &tokenAPI{tokenDelay: tokenDelay}
	srv := httptest.NewServer(api)
	defer srv.Close()
	events := []Event{{T: 0, CID: 0}, {T: secondEventAt, CID: 0}}

	records := execute(t, tokenConfig(t, srv.URL, singleClient(srcOK, events)))

	for _, r := range records {
		if r.Outcome != OutcomeOK {
			t.Fatalf("evento sem sucesso: %+v", r)
		}
		if r.TStart >= tokenDelay.Seconds()/2 || r.LateMS >= maxLateMS {
			t.Errorf("busca de token entrou no caminho medido: %+v", r)
		}
	}
}

func TestTokenPrefetchRespectsConcurrencyLimit(t *testing.T) {
	const (
		clients     = 6
		concurrency = 2
	)
	api := &tokenAPI{tokenDelay: 50 * time.Millisecond}
	srv := httptest.NewServer(api)
	defer srv.Close()
	var s Schedule
	for cid := range clients {
		s.Clients = append(s.Clients, Client{CID: cid, Src: srcOK, Group: groupControl})
		s.Events = append(s.Events, Event{T: 0, CID: cid})
	}
	cfg := tokenConfig(t, srv.URL, s)
	cfg.TokenConcurrency = concurrency

	execute(t, cfg)

	if got := api.tokenCalls.Load(); got != clients {
		t.Fatalf("%d buscas, esperado %d", got, clients)
	}
	if peak := api.maxInFlight.Load(); peak != concurrency {
		t.Fatalf("%d buscas simultaneas, esperado o limite exato %d", peak, concurrency)
	}
}

func TestTokenBodyContractLimit(t *testing.T) {
	atLimit := strings.Repeat("a", contractMaxTokenBytes)
	cases := map[string]struct {
		body     string
		accepted bool
	}{
		"exatamente 4096 bytes": {atLimit, true},
		"4097 bytes":            {atLimit + "a", false},
	}
	for name, c := range cases {
		body := c.body
		api := &tokenAPI{reply: func(int64) (int, string) { return http.StatusOK, body }}
		srv := httptest.NewServer(api)
		cfg := tokenConfig(t, srv.URL, singleClient(srcOK, clientEvents(0, 2)))
		d, events := dispatcherFor(t, cfg)
		if !c.accepted {
			d.startAt = time.Now().Add(tokenPrefetchWindow)
		}

		records := d.run(events)
		srv.Close()

		if c.accepted {
			if d.tokenFailures.Load() != 0 {
				t.Errorf("%s: falhas=%d, esperado 0", name, d.tokenFailures.Load())
			}
			if len(api.apiTokens) != 2 || api.apiTokens[0] != atLimit || api.apiTokens[1] != atLimit {
				t.Errorf("%s: X-Token nao levou o corpo inteiro de 4096 bytes", name)
			}
			continue
		}
		if d.tokenFailures.Load() != 1 {
			t.Errorf("%s: falhas=%d, esperado 1", name, d.tokenFailures.Load())
		}
		for _, tok := range api.apiTokens {
			if tok != "" {
				t.Errorf("%s: token %q chegou ao /api", name, tok)
			}
		}
		for _, r := range records {
			if r.Status != http.StatusUnauthorized || r.Outcome != OutcomeRefused {
				t.Errorf("%s: esperado 401/fail_refused, got %+v", name, r)
			}
		}
	}
}

func TestPrefetchDeadlineIsGlobal(t *testing.T) {
	const (
		clients = 6
		margin  = 500 * time.Millisecond
		slack   = 500 * time.Millisecond
	)
	api := &tokenAPI{stallFrom: 1}
	srv := httptest.NewServer(api)
	defer srv.Close()
	var s Schedule
	for cid := range clients {
		s.Clients = append(s.Clients, Client{CID: cid, Src: srcOK, Group: groupControl})
		s.Events = append(s.Events, Event{T: 0, CID: cid})
	}
	cfg := tokenConfig(t, srv.URL, s)
	cfg.TokenConcurrency = 2
	d, events := dispatcherFor(t, cfg)
	d.startAt = time.Now().Add(margin)

	begin := time.Now()
	records := d.run(events)
	elapsed := time.Since(begin)

	if elapsed > margin+slack {
		t.Fatalf("prefetch levou %v, limite global era %v: o prazo esta sendo cobrado por cliente", elapsed, margin)
	}
	if d.tokenFailures.Load() != clients {
		t.Fatalf("falhas=%d, esperado %d clientes sem token", d.tokenFailures.Load(), clients)
	}
	for _, r := range records {
		if r.Status != http.StatusUnauthorized || r.Outcome != OutcomeRefused {
			t.Errorf("cliente sem token devia sair 401/fail_refused: %+v", r)
		}
	}
}

func TestZeroStartAtPrefetchDeadlineIsTotalTimeout(t *testing.T) {
	const total = time.Second
	api := &tokenAPI{}
	api.failToken.Store(true)
	srv := httptest.NewServer(api)
	defer srv.Close()
	cfg := tokenConfig(t, srv.URL, singleClient(srcOK, clientEvents(0, 2)))
	cfg.Total = total
	d, events := dispatcherFor(t, cfg)

	begin := time.Now()
	d.run(events)
	elapsed := time.Since(begin)

	if elapsed >= total+2*tokenRetryBackoff {
		t.Fatalf("prefetch levou %v com --total-timeout %v: o deadline nao e o total", elapsed, total)
	}
	if got := api.tokenCalls.Load(); got > 5 {
		t.Fatalf("%d buscas de token: o prefetch passou do --total-timeout", got)
	}
	if d.tokenFailures.Load() != 1 {
		t.Fatalf("falhas=%d, esperado 1 cliente sem token", d.tokenFailures.Load())
	}
}

func TestStartAtAlignsClockWithAttacker(t *testing.T) {
	const (
		margin  = time.Second
		wakeTol = 5 * time.Millisecond
	)
	api := &tokenAPI{}
	var firstAPI atomic.Int64
	api.onAPI = func() { firstAPI.CompareAndSwap(0, time.Now().UnixNano()) }
	srv := httptest.NewServer(api)
	defer srv.Close()
	cfg := tokenConfig(t, srv.URL, singleClient(srcOK, clientEvents(0, 1)))
	cfg.StartAt = time.Now().Add(margin).UnixMilli()
	d, events := dispatcherFor(t, cfg)
	if got := d.startAt.UnixMilli(); got != cfg.StartAt {
		t.Fatalf("--start-at %d virou %d no dispatcher", cfg.StartAt, got)
	}

	startAt := d.startAt
	records := d.run(events)

	if got := time.Unix(0, firstAPI.Load()); got.Before(startAt.Add(-wakeTol)) {
		t.Fatalf("primeiro /api em %v, antes do start-at %v", got, startAt)
	}
	if records[0].TStart >= margin.Seconds()/2 || records[0].LateMS >= maxLateMS {
		t.Fatalf("t_start=%v late_ms=%v: o evento nao saiu no tempo zero", records[0].TStart, records[0].LateMS)
	}
}

func TestPastStartAtShowsLagAndWarns(t *testing.T) {
	const past = 2 * time.Second
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	api := &tokenAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	cfg := tokenConfig(t, srv.URL, singleClient(srcOK, clientEvents(0, 1)))
	cfg.StartAt = time.Now().Add(-past).UnixMilli()
	d, events := dispatcherFor(t, cfg)
	records := d.run(events)

	if got := api.tokenCalls.Load(); got != 0 {
		t.Fatalf("%d buscas de token com start-at vencido, esperado 0", got)
	}
	if d.tokenFailures.Load() != 1 {
		t.Fatalf("falhas=%d, esperado o cliente sem token", d.tokenFailures.Load())
	}
	if minLagMS := past.Seconds() * msPerSecond * 0.9; records[0].LateMS < minLagMS {
		t.Fatalf("late_ms=%v, esperado >= %v: o atraso do start-at passado sumiu", records[0].LateMS, minLagMS)
	}
	if warn := buf.String(); !strings.Contains(warn, "ja passou") || !strings.Contains(warn, "late_ms") {
		t.Fatalf("aviso de sincronia perdida ausente: %q", warn)
	}
}

func TestZeroStartAtUsesPrefetchEndAsZero(t *testing.T) {
	const tokenDelay = 300 * time.Millisecond
	api := &tokenAPI{tokenDelay: tokenDelay}
	srv := httptest.NewServer(api)
	defer srv.Close()

	records := execute(t, tokenConfig(t, srv.URL, singleClient(srcOK, clientEvents(0, 1))))

	if records[0].Outcome != OutcomeOK {
		t.Fatalf("evento sem sucesso: %+v", records[0])
	}
	if records[0].TStart > tokenDelay.Seconds()/2 {
		t.Fatalf("t_start=%v: o tempo zero foi definido antes do fim do prefetch", records[0].TStart)
	}
}

func TestTokenFetchFailuresAreCountedAndNeverRetriedDuringEvents(t *testing.T) {
	bodies := map[string]func(int64) (int, string){
		"429 sem corpo":      func(int64) (int, string) { return http.StatusTooManyRequests, "" },
		"429 com corpo":      func(int64) (int, string) { return http.StatusTooManyRequests, "tok-malicioso" },
		"401 com corpo":      func(int64) (int, string) { return http.StatusUnauthorized, "tok-malicioso" },
		"200 vazio":          func(int64) (int, string) { return http.StatusOK, "" },
		"200 so espacos":     func(int64) (int, string) { return http.StatusOK, " \r\n" },
		"espaco interno":     func(int64) (int, string) { return http.StatusOK, "ab cd" },
		"quebra de linha":    func(int64) (int, string) { return http.StatusOK, "ab\r\ncd" },
		"byte de controle":   func(int64) (int, string) { return http.StatusOK, "ab\x01cd" },
		"corpo acima do max": func(int64) (int, string) { return http.StatusOK, strings.Repeat("a", contractMaxTokenBytes+1) },
	}
	for name, reply := range bodies {
		api := &tokenAPI{reply: reply}
		srv := httptest.NewServer(api)
		cfg := tokenConfig(t, srv.URL, singleClient(srcOK, clientEvents(0, 3)))
		d, events := dispatcherFor(t, cfg)
		d.startAt = time.Now().Add(tokenPrefetchWindow)

		records := d.run(events)
		srv.Close()

		if d.tokenFailures.Load() != 1 {
			t.Errorf("%s: falhas=%d, esperado 1", name, d.tokenFailures.Load())
		}
		for _, tok := range api.apiTokens {
			if tok != "" {
				t.Errorf("%s: token %q chegou ao /api", name, tok)
			}
		}
		for _, r := range records {
			if r.Status != http.StatusUnauthorized || r.Outcome != OutcomeRefused {
				t.Errorf("%s: esperado 401/fail_refused, got %+v", name, r)
			}
		}
	}
}

func TestTokenPrefetchRetriesWithinDeadlineAndStopsAfterIt(t *testing.T) {
	const failures = 1
	api := &tokenAPI{reply: func(n int64) (int, string) {
		if n <= failures {
			return http.StatusTooManyRequests, ""
		}
		return http.StatusOK, "tok"
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	cfg := tokenConfig(t, srv.URL, singleClient(srcOK, clientEvents(0, 2)))
	d, events := dispatcherFor(t, cfg)
	d.startAt = time.Now().Add(retryPrefetchWindow)

	records := d.run(events)

	if api.tokenCalls.Load() != failures+1 || d.tokenFailures.Load() != 0 {
		t.Fatalf("buscas=%d falhas=%d, esperado %d e 0", api.tokenCalls.Load(), d.tokenFailures.Load(), failures+1)
	}
	for _, r := range records {
		if r.Outcome != OutcomeOK {
			t.Errorf("evento sem sucesso apos retentativa: %+v", r)
		}
	}

	always := &tokenAPI{}
	always.failToken.Store(true)
	srvAlways := httptest.NewServer(always)
	defer srvAlways.Close()
	cfg = tokenConfig(t, srvAlways.URL, singleClient(srcOK, clientEvents(0, 3)))
	d, events = dispatcherFor(t, cfg)
	d.startAt = time.Now().Add(retryPrefetchWindow)

	d.run(events)

	if calls := always.tokenCalls.Load(); calls < 2 || calls > 10 || d.tokenFailures.Load() != 1 {
		t.Fatalf("buscas=%d falhas=%d, esperado ao menos 2 buscas na serie e 1 falha", calls, d.tokenFailures.Load())
	}
	if always.tokenCallAfterAPI() {
		t.Fatal("busca de token durante os eventos de um cliente sem token")
	}
}

func TestStalledRenewalNeitherQueuesNorBlocksEvents(t *testing.T) {
	var offset atomic.Int64
	api := &tokenAPI{stallFrom: 2}
	api.onAPI = func() { offset.Store(int64(contractTokenAge + time.Second)) }
	srv := httptest.NewServer(api)
	defer srv.Close()
	laterEvents := []Event{{T: 0.2, CID: 0}, {T: 0.21, CID: 0}, {T: 0.22, CID: 0}, {T: 0.23, CID: 0}}
	s := singleClient(srcOK, append([]Event{{T: 0, CID: 0}}, laterEvents...))
	sched, err := loadSchedule(writeSchedule(t, s))
	if err != nil {
		t.Fatal(err)
	}
	cfg := tokenConfig(t, srv.URL, s)
	d, err := newDispatcher(cfg, sched)
	if err != nil {
		t.Fatal(err)
	}
	d.clients[0].token.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }

	records := d.run(sched.Events)

	if got := api.tokenCalls.Load(); got != 2 {
		t.Fatalf("%d buscas, esperado 2 (busca inicial e uma unica renovacao em curso)", got)
	}
	if d.tokenFailures.Load() != 0 {
		t.Fatalf("renovacao cancelada no fim contada como falha: %d", d.tokenFailures.Load())
	}
	for _, r := range records {
		if r.Outcome != OutcomeOK || r.TEnd-r.TStart >= generousTimeout.Seconds()/2 {
			t.Errorf("evento afetado pela renovacao parada: %+v", r)
		}
	}
	for _, tok := range api.apiTokens {
		if tok != api.apiTokens[0] {
			t.Fatalf("tokens distintos %v: o token anterior deve seguir ate a renovacao terminar", api.apiTokens)
		}
	}
}

type renewalTransport struct {
	tokenCalls atomic.Int64
	apiCalls   atomic.Int64
	aged       atomic.Bool
	returned   atomic.Bool
}

func (r *renewalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == tokenPath {
		if r.tokenCalls.Add(1) >= 2 {
			<-req.Context().Done()
			r.returned.Store(true)
			return nil, req.Context().Err()
		}
		return tokenResponse("tok"), nil
	}
	if r.apiCalls.Add(1) == 1 {
		r.aged.Store(true)
	}
	return tokenResponse("ok"), nil
}

func tokenResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
}

func TestRunWaitsForRenewalGoroutines(t *testing.T) {
	transport := &renewalTransport{}
	client := &http.Client{Transport: transport}
	s := singleClient(srcOK, []Event{{T: 0, CID: 0}, {T: 0.2, CID: 0}})
	sched, err := loadSchedule(writeSchedule(t, s))
	if err != nil {
		t.Fatal(err)
	}
	cfg := tokenConfig(t, "http://127.0.0.1:1", s)
	d, err := newDispatcher(cfg, sched)
	if err != nil {
		t.Fatal(err)
	}
	d.clients[0].http = client
	d.clients[0].token.client = client
	base := time.Now()
	d.clients[0].token.now = func() time.Time {
		if transport.aged.Load() {
			return base.Add(tokenMaxAge + time.Second)
		}
		return base
	}

	records := d.run(sched.Events)

	if got := transport.tokenCalls.Load(); got != 2 {
		t.Fatalf("%d buscas, esperado 2 (prefetch e uma renovacao parada)", got)
	}
	if !transport.returned.Load() {
		t.Fatal("run retornou com a renovacao em curso")
	}
	for _, r := range records {
		if r.Outcome != OutcomeOK {
			t.Errorf("evento afetado pela renovacao parada: %+v", r)
		}
	}
}

func TestRenewalFailureIsCountedAndKeepsOldToken(t *testing.T) {
	var offset atomic.Int64
	api := &tokenAPI{reply: func(n int64) (int, string) {
		if n == 1 {
			return http.StatusOK, "tok"
		}
		return http.StatusTooManyRequests, ""
	}}
	api.onAPI = func() { offset.Store(int64(tokenMaxAge + time.Second)) }
	srv := httptest.NewServer(api)
	defer srv.Close()
	s := singleClient(srcOK, []Event{{T: 0, CID: 0}, {T: 0.2, CID: 0}, {T: 0.4, CID: 0}})
	sched, err := loadSchedule(writeSchedule(t, s))
	if err != nil {
		t.Fatal(err)
	}
	cfg := tokenConfig(t, srv.URL, s)
	d, err := newDispatcher(cfg, sched)
	if err != nil {
		t.Fatal(err)
	}
	d.clients[0].token.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }

	records := d.run(sched.Events)

	if got := api.tokenCalls.Load(); got != 2 {
		t.Fatalf("%d buscas, esperado 2 (prefetch e uma renovacao que falha)", got)
	}
	if d.tokenFailures.Load() != 1 {
		t.Fatalf("falhas=%d, esperado 1 renovacao falha contada", d.tokenFailures.Load())
	}
	for _, r := range records {
		if r.Outcome != OutcomeOK {
			t.Errorf("evento com token antigo devia seguir: %+v", r)
		}
	}
	for _, tok := range api.apiTokens {
		if tok != "tok" {
			t.Fatalf("token %q em /api: o token anterior devia seguir", tok)
		}
	}
}

func TestTokenSnapshotRefreshesOnceAndBacksOffAfterFailure(t *testing.T) {
	api := &tokenAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	now := time.Unix(0, 0)
	src := newTokenSource(srv.Client(), srv.URL, tokenPath, 5, generousTimeout, testPartial)
	src.now = func() time.Time { return now }
	snap := func(wantToken string, wantRefresh bool) {
		t.Helper()
		token, refresh := src.snapshot()
		if token != wantToken || refresh != wantRefresh {
			t.Fatalf("snapshot()=(%q,%v), esperado (%q,%v)", token, refresh, wantToken, wantRefresh)
		}
	}
	snap("", false)
	if err := src.prefetch(time.Now().Add(tokenPrefetchWindow)); err != nil {
		t.Fatal(err)
	}
	snap("tok-5-1", false)
	now = now.Add(contractTokenAge)
	snap("tok-5-1", false)
	now = now.Add(time.Nanosecond)
	snap("tok-5-1", true)
	snap("tok-5-1", false)
	api.failToken.Store(true)
	if err := src.refresh(context.Background()); err == nil {
		t.Fatal("renovacao devia falhar")
	}
	snap("tok-5-1", false)
	now = now.Add(testPartial - time.Nanosecond)
	snap("tok-5-1", false)
	now = now.Add(time.Nanosecond)
	snap("tok-5-1", true)
	api.failToken.Store(false)
	if err := src.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap("tok-5-3", false)
}

func TestCheckSourceReleasesItsProbeSocket(t *testing.T) {
	const (
		rounds          = 100
		fdLeakTolerance = 10
	)
	openFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := openFDs()
	for range rounds {
		if err := checkSource(net.ParseIP(srcOK), "127.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	if after := openFDs(); after > before+fdLeakTolerance {
		t.Fatalf("descritores abertos %d -> %d: checkSource vaza o socket de sondagem", before, after)
	}
}
