package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

const (
	defaultRate           = 1000
	defaultDuration       = 60
	defaultPoolSize       = 2000
	defaultClient24       = 5
	defaultPostRatio      = 0.3
	defaultHeavyRatio     = 0.5
	defaultHeavyMS        = 200
	defaultPRepeat        = 0.02
	defaultOutPath        = "attacker.jsonl"
	defaultTokenPath      = "/token"
	defaultPartialTimeout = 4 * time.Second
	defaultTotalTimeout   = 60 * time.Second
	pRepeatMin            = 0.002
	pRepeatMax            = 0.06
	prefixV4              = "10.66.0.0/16"
	prefixV6              = "fd12:3456:789a::/48"
	scenarioS1            = "s1"
	scenarioS2a           = "s2a"
	scenarioS3            = "s3"
	forbiddenPathChars    = "@?#"
	doubleSlash           = "//"
	maxTokenRunSeconds    = 270
	maxTokenBudget        = 1000
	startAtAheadLimit     = time.Hour
)

var defaultUAs = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Mobile Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
	"Mozilla/5.0 (SMART-TV; Linux; Tizen 6.0) AppleWebKit/537.36 (KHTML, like Gecko) Version/6.0 TV Safari/537.36",
}

var defaultHeaders = [][2]string{
	{"Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
	{"Accept-Language", "en-US,en;q=0.9"},
	{"Accept-Encoding", "gzip, deflate"},
}

var (
	baseV4 = mustParseCIDR(prefixV4)
	baseV6 = mustParseCIDR(prefixV6)
)

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("CIDR constante %q inválido: %v", s, err))
	}
	return n
}

type config struct {
	seed        int64
	scenario    string
	target      string
	duration    float64
	warmup      float64
	rate        float64
	out         string
	per24       int
	pRepeat     float64
	poolSize    int
	client24    int
	postRatio   float64
	heavyRatio  float64
	heavyMS     int
	waveform    string
	churn       float64
	sticky      float64
	tokenMode   string
	tokenBudget int
	tokenPath   string
	keepalive   bool
	pipeline    bool
	partial     time.Duration
	total       time.Duration
	startAt     int64
	uas         []string
	headers     [][2]string
}

func defaults() config {
	return config{
		seed:       1,
		scenario:   scenarioS1,
		duration:   defaultDuration,
		rate:       defaultRate,
		out:        defaultOutPath,
		per24:      hostsPerGroupDefault,
		pRepeat:    defaultPRepeat,
		poolSize:   defaultPoolSize,
		client24:   defaultClient24,
		postRatio:  defaultPostRatio,
		heavyRatio: defaultHeavyRatio,
		heavyMS:    defaultHeavyMS,
		waveform:   waveSteady,
		tokenMode:  tokenNone,
		tokenPath:  defaultTokenPath,
		partial:    defaultPartialTimeout,
		total:      defaultTotalTimeout,
		uas:        defaultUAs,
		headers:    defaultHeaders,
	}
}

type scheduleHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type scheduleFile struct {
	Per24       *int             `json:"per_24"`
	PRepeat     *float64         `json:"p_repeat"`
	PoolSize    *int             `json:"pool_size"`
	Client24    *int             `json:"client_24_count"`
	PostRatio   *float64         `json:"post_ratio"`
	HeavyRatio  *float64         `json:"heavy_ratio"`
	HeavyMS     *int             `json:"heavy_ms"`
	Waveform    *string          `json:"waveform"`
	Churn       *float64         `json:"churn"`
	StickyS     *float64         `json:"sticky_s"`
	TokenMode   *string          `json:"token_mode"`
	TokenBudget *int             `json:"token_budget"`
	TokenPath   *string          `json:"token_path"`
	UserAgents  []string         `json:"user_agents"`
	Headers     []scheduleHeader `json:"headers"`
}

func parseConfig(args []string) (config, error) {
	cfg := defaults()
	fs := flag.NewFlagSet("attacker", flag.ContinueOnError)
	schedulePath := fs.String("schedule", "", "arquivo JSON de pool/parâmetros do atacante")
	fs.Int64Var(&cfg.seed, "seed", cfg.seed, "semente determinística")
	fs.StringVar(&cfg.scenario, "scenario", cfg.scenario, "cenário s1|s2a|s3")
	fs.StringVar(&cfg.target, "target", cfg.target, "URL do alvo (Nginx do grupo)")
	fs.Float64Var(&cfg.duration, "duration", cfg.duration, "duração do ataque em s")
	fs.Float64Var(&cfg.warmup, "warmup", cfg.warmup, "aquecimento em s antes da onda")
	fs.Float64Var(&cfg.rate, "rate", cfg.rate, "taxa média agregada em req/s")
	fs.StringVar(&cfg.out, "out", cfg.out, "arquivo attacker.jsonl de saída")
	fs.IntVar(&cfg.per24, "per-24", cfg.per24, "IPs por /24 (tolerância ±2)")
	fs.Float64Var(&cfg.pRepeat, "p-repeat", cfg.pRepeat, "probabilidade de repetir um IP (0,002–0,06)")
	fs.IntVar(&cfg.poolSize, "pool-size", cfg.poolSize, "tamanho-alvo do pool de IPs")
	fs.IntVar(&cfg.client24, "client-24", cfg.client24, "quantos /24 de clientes pular em s3")
	fs.Float64Var(&cfg.postRatio, "post-ratio", cfg.postRatio, "fração de requisições POST")
	fs.Float64Var(&cfg.heavyRatio, "heavy-ratio", cfg.heavyRatio, "fração de requisições para /heavy")
	fs.IntVar(&cfg.heavyMS, "heavy-ms", cfg.heavyMS, "ms de trabalho em /heavy")
	fs.StringVar(&cfg.waveform, "waveform", cfg.waveform, "steady|ramp|pulse|waves")
	fs.Float64Var(&cfg.churn, "churn", cfg.churn, "fração do pool rotacionada na corrida (0–1)")
	fs.Float64Var(&cfg.sticky, "sticky", cfg.sticky, "tempo sticky do IP em s (0 = por requisição)")
	fs.StringVar(&cfg.tokenMode, "token-mode", cfg.tokenMode, "none|valid-reuse|farm")
	fs.IntVar(&cfg.tokenBudget, "token-budget", cfg.tokenBudget, "tokens emitidos no farm (teto de chamadas a /token)")
	fs.StringVar(&cfg.tokenPath, "token-path", cfg.tokenPath, "rota de emissão de token")
	fs.BoolVar(&cfg.keepalive, "keepalive", cfg.keepalive, "reusar conexão (padrão desligado)")
	fs.BoolVar(&cfg.pipeline, "pipeline", cfg.pipeline, "concentrar requisições por conexão (implica keepalive)")
	fs.DurationVar(&cfg.partial, "partial-timeout", cfg.partial, "limite de falha parcial")
	fs.DurationVar(&cfg.total, "total-timeout", cfg.total, "limite de falha total")
	fs.Int64Var(&cfg.startAt, "start-at", cfg.startAt, "instante unix_ms do tempo zero comum; 0 = zero ao terminar o prefetch")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if *schedulePath != "" {
		if err := applySchedule(&cfg, *schedulePath, set); err != nil {
			return config{}, err
		}
	}
	if cfg.pipeline {
		cfg.keepalive = true
	}
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func applySchedule(cfg *config, path string, set map[string]bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("lê schedule %s: %w", path, err)
	}
	var sf scheduleFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sf); err != nil {
		return fmt.Errorf("schedule %s inválido: %w", path, err)
	}
	override(sf.Per24, &cfg.per24, set["per-24"])
	override(sf.PRepeat, &cfg.pRepeat, set["p-repeat"])
	override(sf.PoolSize, &cfg.poolSize, set["pool-size"])
	override(sf.Client24, &cfg.client24, set["client-24"])
	override(sf.PostRatio, &cfg.postRatio, set["post-ratio"])
	override(sf.HeavyRatio, &cfg.heavyRatio, set["heavy-ratio"])
	override(sf.HeavyMS, &cfg.heavyMS, set["heavy-ms"])
	override(sf.Waveform, &cfg.waveform, set["waveform"])
	override(sf.Churn, &cfg.churn, set["churn"])
	override(sf.StickyS, &cfg.sticky, set["sticky"])
	override(sf.TokenMode, &cfg.tokenMode, set["token-mode"])
	override(sf.TokenBudget, &cfg.tokenBudget, set["token-budget"])
	override(sf.TokenPath, &cfg.tokenPath, set["token-path"])
	if len(sf.UserAgents) > 0 {
		cfg.uas = sf.UserAgents
	}
	if len(sf.Headers) > 0 {
		cfg.headers = make([][2]string, len(sf.Headers))
		for i, h := range sf.Headers {
			cfg.headers[i] = [2]string{h.Name, h.Value}
		}
	}
	return nil
}

func override[T any](src *T, dst *T, flagSet bool) {
	if src != nil && !flagSet {
		*dst = *src
	}
}

func inUnitRange(v float64) bool { return v >= 0 && v <= 1 }

func (c config) validate() error {
	switch c.scenario {
	case scenarioS1, scenarioS2a, scenarioS3:
	default:
		return fmt.Errorf("cenário desconhecido: %q", c.scenario)
	}
	checks := []struct {
		ok  bool
		msg string
	}{
		{c.target != "", "--target é obrigatório"},
		{c.rate > 0 && c.duration > 0, "--rate e --duration devem ser > 0"},
		{c.warmup >= 0, "--warmup deve ser >= 0"},
		{c.pRepeat >= pRepeatMin && c.pRepeat <= pRepeatMax, fmt.Sprintf("--p-repeat deve estar em [%g, %g]", pRepeatMin, pRepeatMax)},
		{c.per24 >= 1, "--per-24 deve ser >= 1"},
		{c.poolSize >= 1, "--pool-size deve ser >= 1"},
		{c.client24 >= 0, "--client-24 deve ser >= 0"},
		{inUnitRange(c.postRatio), "--post-ratio deve estar em [0, 1]"},
		{inUnitRange(c.heavyRatio), "--heavy-ratio deve estar em [0, 1]"},
		{c.heavyMS >= 0, "--heavy-ms deve ser >= 0"},
		{inUnitRange(c.churn), "--churn deve estar em [0, 1]"},
		{c.sticky >= 0, "--sticky deve ser >= 0"},
		{c.tokenBudget >= 0, "--token-budget deve ser >= 0"},
		{c.tokenBudget <= maxTokenBudget, fmt.Sprintf("--token-budget deve ser <= %d", maxTokenBudget)},
		{c.startAt >= 0, "--start-at deve ser >= 0"},
		{c.startAt <= time.Now().Add(startAtAheadLimit).UnixMilli(), fmt.Sprintf("--start-at deve ser unix em milissegundos e no máximo %v à frente", startAtAheadLimit)},
		{c.partial <= c.total, "--partial-timeout deve ser <= --total-timeout"},
		{len(c.uas) > 0, "pool de User-Agent vazio"},
	}
	for _, ch := range checks {
		if !ch.ok {
			return errors.New(ch.msg)
		}
	}
	if _, err := newWaveform(c.waveform); err != nil {
		return err
	}
	if err := c.validateToken(); err != nil {
		return err
	}
	return nil
}

func (c config) validateToken() error {
	switch c.tokenMode {
	case tokenNone, tokenValidReuse:
	case tokenFarm:
		if c.tokenBudget < 1 {
			return fmt.Errorf("token-mode %q exige --token-budget >= 1", c.tokenMode)
		}
	default:
		return fmt.Errorf("token-mode desconhecido: %q", c.tokenMode)
	}
	if c.tokenMode != tokenNone && c.warmup+c.duration > maxTokenRunSeconds {
		return fmt.Errorf("--warmup + --duration deve ser <= %d s em token-mode %q: o token vale 300 s e não é renovado", maxTokenRunSeconds, c.tokenMode)
	}
	if !strings.HasPrefix(c.tokenPath, "/") || strings.ContainsAny(c.tokenPath, forbiddenPathChars) || strings.Contains(c.tokenPath, doubleSlash) {
		return fmt.Errorf("--token-path %q deve começar com / e não conter @, //, ? nem #", c.tokenPath)
	}
	return nil
}

type poolSpec struct {
	base     *net.IPNet
	perGroup int
	groups   int
	exclude  map[int]bool
}

func (c config) poolSpec() poolSpec {
	switch c.scenario {
	case scenarioS2a:
		return poolSpec{base: baseV6, perGroup: c.poolSize, groups: 1}
	case scenarioS3:
		return poolSpec{base: baseV4, perGroup: c.per24, groups: ceilDiv(c.poolSize, c.per24), exclude: rangeSet(1, c.client24)}
	default:
		return poolSpec{base: baseV4, perGroup: c.per24, groups: ceilDiv(c.poolSize, c.per24)}
	}
}
