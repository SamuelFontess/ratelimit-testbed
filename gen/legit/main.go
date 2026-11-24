package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

const (
	defaultPartialTimeout   = 4 * time.Second
	defaultTotalTimeout     = 60 * time.Second
	defaultTokenConcurrency = 16
	startAtAheadLimit       = time.Hour
	usageExitCode           = 2
)

var errFlagParse = errors.New("flags invalidas")

func parseFlags(args []string) (Config, error) {
	fs := flag.NewFlagSet("legit", flag.ContinueOnError)
	fs.Int("seed", 0, "ignorado: a agenda ja carrega a seed")
	fs.String("scenario", "", "ignorado: a agenda ja carrega o cenario")
	fs.Float64("duration", 0, "ignorado: a agenda ja carrega a duracao")
	fs.Float64("warmup", 0, "ignorado: o consolidador descarta o warmup")
	fs.Float64("rate", 0, "ignorado: a taxa vem da agenda")
	var cfg Config
	fs.StringVar(&cfg.TokenPath, "token-path", "", "caminho do endpoint de token K3; vazio = sem token")
	fs.IntVar(&cfg.TokenConcurrency, "token-concurrency", defaultTokenConcurrency, "buscas de token simultaneas antes do tempo zero")
	fs.Int64Var(&cfg.StartAt, "start-at", 0, "instante Unix em ms do tempo zero; 0 = fim do prefetch de tokens")
	fs.StringVar(&cfg.Target, "target", "", "URL base do alvo (obrigatorio)")
	fs.StringVar(&cfg.Schedule, "schedule", "", "agenda JSON gerada por schedule.py (obrigatorio)")
	fs.StringVar(&cfg.Out, "out", "", "legit.jsonl de saida (obrigatorio)")
	fs.DurationVar(&cfg.Partial, "partial-timeout", defaultPartialTimeout, "espera maxima ate o primeiro byte ou entre bytes")
	fs.DurationVar(&cfg.Total, "total-timeout", defaultTotalTimeout, "duracao maxima da transacao")
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("%w: %w", errFlagParse, err)
	}
	if cfg.Target == "" || cfg.Schedule == "" || cfg.Out == "" {
		return Config{}, errors.New("--target, --schedule e --out sao obrigatorios")
	}
	if cfg.Partial <= 0 || cfg.Total <= 0 {
		return Config{}, fmt.Errorf("timeouts devem ser positivos: partial=%v total=%v", cfg.Partial, cfg.Total)
	}
	if cfg.TokenPath != "" && !strings.HasPrefix(cfg.TokenPath, "/") {
		return Config{}, fmt.Errorf("--token-path deve comecar com /: %q", cfg.TokenPath)
	}
	if cfg.TokenConcurrency < 1 {
		return Config{}, fmt.Errorf("--token-concurrency deve ser >= 1: %d", cfg.TokenConcurrency)
	}
	if cfg.StartAt < 0 {
		return Config{}, fmt.Errorf("--start-at nao pode ser negativo: %d", cfg.StartAt)
	}
	if cfg.StartAt > time.Now().Add(startAtAheadLimit).UnixMilli() {
		return Config{}, fmt.Errorf("--start-at deve ser unix em milissegundos e no máximo %v à frente", startAtAheadLimit)
	}
	return cfg, nil
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		if !errors.Is(err, errFlagParse) {
			log.Print(err)
		}
		os.Exit(usageExitCode)
	}
	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}
