package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const recordBuffer = 1 << 16

var (
	errInterrupted    = errors.New("execução interrompida por sinal")
	errPrefetchWindow = errors.New("prefetch de tokens excedeu a janela de --start-at")
	errStartAtPassed  = errors.New("--start-at já passou")
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "attacker:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}
	target, err := validateTarget(cfg.target)
	if err != nil {
		return err
	}
	spec := cfg.poolSpec()
	pool, err := buildPool(spec.base, spec.perGroup, spec.groups, spec.exclude, newRNG(cfg.seed, poolStream))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	maxInflight, err := inflightLimit()
	if err != nil {
		return err
	}
	return execute(ctx, cfg, target, pool, maxInflight)
}

func execute(ctx context.Context, cfg config, target *url.URL, pool []net.IP, maxInflight int) error {
	if err := checkSources(pool, target.Hostname()); err != nil {
		return err
	}
	planCfg, err := planFromConfig(cfg, pool)
	if err != nil {
		return err
	}

	base := target.String()
	cache := newTransportCache(cfg.keepalive)
	defer cache.closeAll()
	prefetchCtx, cancelPrefetch := prefetchContext(ctx, cfg.startAt)
	defer cancelPrefetch()
	tokens, err := acquireTokens(prefetchCtx, cfg, base, pool, cache)
	if err != nil {
		return prefetchFailure(ctx, cfg.startAt, err)
	}
	start, err := startInstant(cfg.startAt)
	if err != nil {
		return err
	}

	w, err := newWriter(cfg.out)
	if err != nil {
		return err
	}
	records := make(chan record, recordBuffer)
	done := make(chan error, 1)
	go func() {
		var werr error
		for r := range records {
			if werr == nil {
				werr = w.write(r)
			}
		}
		done <- werr
	}()

	s := newSender(senderConfig{
		target:      base,
		cache:       cache,
		tokens:      tokens,
		partial:     cfg.partial,
		total:       cfg.total,
		maxInflight: maxInflight,
	}, records)
	senderErr := s.run(ctx, start, newPlanner(planCfg, cfg.seed))

	close(records)
	writeErr := errors.Join(<-done, w.close())
	var interrupted error
	if ctx.Err() != nil {
		interrupted = errInterrupted
	}
	return errors.Join(senderErr, writeErr, interrupted)
}

func prefetchContext(ctx context.Context, startAt int64) (context.Context, context.CancelFunc) {
	if startAt == 0 {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, time.UnixMilli(startAt))
}

func prefetchFailure(ctx context.Context, startAt int64, err error) error {
	if startAt != 0 && ctx.Err() == nil && time.Now().UnixMilli() >= startAt {
		return fmt.Errorf("%w: %w", errPrefetchWindow, err)
	}
	return err
}

func startInstant(startAt int64) (time.Time, error) {
	if startAt == 0 {
		return time.Now(), nil
	}
	remaining := time.Until(time.UnixMilli(startAt))
	if remaining <= 0 {
		return time.Time{}, errStartAtPassed
	}
	return time.Now().Add(remaining), nil
}

func planFromConfig(cfg config, pool []net.IP) (planConfig, error) {
	wave, err := newWaveform(cfg.waveform)
	if err != nil {
		return planConfig{}, err
	}
	return planConfig{
		pool:       pool,
		rate:       cfg.rate,
		warmup:     cfg.warmup,
		duration:   cfg.duration,
		postRatio:  cfg.postRatio,
		heavyRatio: cfg.heavyRatio,
		heavyMS:    cfg.heavyMS,
		uas:        cfg.uas,
		headerPool: cfg.headers,
		pRepeat:    cfg.pRepeat,
		sticky:     cfg.sticky,
		churn:      cfg.churn,
		wave:       wave,
	}, nil
}
