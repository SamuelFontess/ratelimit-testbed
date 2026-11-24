package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	apiPath      = "/api"
	msPerSecond  = 1000.0
	outputBuffer = 1 << 16
)

type Config struct {
	Target           string
	Schedule         string
	Out              string
	TokenPath        string
	TokenConcurrency int
	StartAt          int64
	Partial          time.Duration
	Total            time.Duration
}

type Record struct {
	TPlan   float64 `json:"t_plan"`
	TStart  float64 `json:"t_start"`
	TEnd    float64 `json:"t_end"`
	Status  int     `json:"status"`
	Outcome string  `json:"outcome"`
	CID     int     `json:"cid"`
	Group   string  `json:"group"`
	LateMS  float64 `json:"late_ms"`
}

type client struct {
	http  *http.Client
	group string
	token *tokenSource
}

type dispatcher struct {
	start            time.Time
	base             *http.Request
	clients          map[int]*client
	total            time.Duration
	tokenConcurrency int
	startAt          time.Time
	refreshCtx       context.Context
	background       sync.WaitGroup
	tokenFailures    atomic.Int64
}

func checkSource(src net.IP, targetHost string) error {
	if host := net.ParseIP(targetHost); host != nil && (host.To4() == nil) != (src.To4() == nil) {
		return fmt.Errorf("origem %s e alvo %s de familias IP diferentes", src, host)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(src.String(), "0"))
	if err != nil {
		return fmt.Errorf("origem %s inutilizavel: %w", src, err)
	}
	return ln.Close()
}

func newDispatcher(cfg Config, sched Schedule) (*dispatcher, error) {
	base, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(cfg.Target, "/")+apiPath, nil)
	if err != nil {
		return nil, fmt.Errorf("target invalido %q: %w", cfg.Target, err)
	}
	d := &dispatcher{
		base:             base,
		clients:          make(map[int]*client, len(sched.Clients)),
		total:            cfg.Total,
		tokenConcurrency: cfg.TokenConcurrency,
	}
	if cfg.StartAt != 0 {
		d.startAt = time.UnixMilli(cfg.StartAt)
	}
	for _, c := range sched.Clients {
		src := net.ParseIP(c.Src)
		if err := checkSource(src, base.URL.Hostname()); err != nil {
			return nil, fmt.Errorf("cliente %d: %w", c.CID, err)
		}
		cl := &client{http: newHTTPClient(src, cfg.Partial), group: c.Group}
		if cfg.TokenPath != "" {
			cl.token = newTokenSource(cl.http, cfg.Target, cfg.TokenPath, c.CID, cfg.Total, cfg.Partial)
		}
		d.clients[c.CID] = cl
	}
	return d, nil
}

func (d *dispatcher) elapsed() float64 {
	return time.Since(d.start).Seconds()
}

func (d *dispatcher) request(c *client) *http.Request {
	if c.token == nil {
		return d.base
	}
	token, refresh := c.token.snapshot()
	if refresh {
		d.startRefresh(c.token)
	}
	if token == "" {
		return d.base
	}
	req := d.base.Clone(context.Background())
	req.Header.Set(tokenHeader, token)
	return req
}

func (d *dispatcher) fire(e Event) Record {
	tStart := d.elapsed()
	c := d.clients[e.CID]
	status, outcome := transact(c.http, d.request(c), d.total)
	return Record{
		TPlan:   e.T,
		TStart:  tStart,
		TEnd:    d.elapsed(),
		Status:  status,
		Outcome: outcome,
		CID:     e.CID,
		Group:   c.group,
		LateMS:  (tStart - e.T) * msPerSecond,
	}
}

func (d *dispatcher) prefetchTokens() {
	deadline := d.startAt
	if deadline.IsZero() {
		deadline = time.Now().Add(d.total)
	}
	pending := make(chan *tokenSource)
	var wg sync.WaitGroup
	for range d.tokenConcurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range pending {
				if err := s.prefetch(deadline); err != nil {
					d.tokenFailures.Add(1)
				}
			}
		}()
	}
	for _, c := range d.clients {
		if c.token != nil {
			pending <- c.token
		}
	}
	close(pending)
	wg.Wait()
}

func (d *dispatcher) startRefresh(s *tokenSource) {
	d.background.Add(1)
	go func() {
		defer d.background.Done()
		if err := s.refresh(d.refreshCtx); err != nil && d.refreshCtx.Err() == nil {
			d.tokenFailures.Add(1)
		}
	}()
}

func (d *dispatcher) run(events []Event) []Record {
	d.prefetchTokens()
	d.start = time.Now()
	if !d.startAt.IsZero() {
		if lag := time.Since(d.startAt); lag > 0 {
			log.Printf("--start-at ja passou ha %v: sincronia com o atacante perdida; o atraso aparece em late_ms", lag)
		}
		d.start = time.Now().Add(time.Until(d.startAt))
	}
	var cancel context.CancelFunc
	d.refreshCtx, cancel = context.WithCancel(context.Background())
	records := make([]Record, len(events))
	var wg sync.WaitGroup
	for i, e := range events {
		time.Sleep(time.Until(d.start.Add(time.Duration(e.T * float64(time.Second)))))
		wg.Add(1)
		go func() {
			defer wg.Done()
			records[i] = d.fire(e)
		}()
	}
	wg.Wait()
	cancel()
	d.background.Wait()
	return records
}

func writeRecords(w io.Writer, records []Record) error {
	bw := bufio.NewWriterSize(w, outputBuffer)
	enc := json.NewEncoder(bw)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("gravar registro: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("descarregar saida: %w", err)
	}
	return nil
}

func run(cfg Config) (err error) {
	sched, err := loadSchedule(cfg.Schedule)
	if err != nil {
		return err
	}
	d, err := newDispatcher(cfg, sched)
	if err != nil {
		return err
	}
	f, err := os.Create(cfg.Out)
	if err != nil {
		return fmt.Errorf("criar saida: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("fechar saida %s: %w", cfg.Out, cerr)
		}
	}()
	records := d.run(sched.Events)
	if n := d.tokenFailures.Load(); n > 0 {
		log.Printf("%d buscas de token falharam; os eventos seguiram sem token", n)
	}
	if err := writeRecords(f, records); err != nil {
		return fmt.Errorf("saida %s: %w", cfg.Out, err)
	}
	return nil
}
