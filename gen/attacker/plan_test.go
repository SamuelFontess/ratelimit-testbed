package main

import (
	"math"
	"net"
	"reflect"
	"strings"
	"testing"
)

func testPool(n int) []net.IP {
	pool := make([]net.IP, n)
	for i := range n {
		pool[i] = net.IPv4(10, 66, byte(i/254), byte(i%254+1))
	}
	return pool
}

func planCfg(pool []net.IP) planConfig {
	wave, _ := newWaveform(waveSteady)
	return planConfig{
		pool:       pool,
		rate:       1000,
		duration:   1,
		postRatio:  defaultPostRatio,
		heavyRatio: defaultHeavyRatio,
		heavyMS:    defaultHeavyMS,
		uas:        defaultUAs,
		headerPool: defaultHeaders,
		pRepeat:    defaultPRepeat,
		wave:       wave,
	}
}

func drain(p *planner) []plannedRequest {
	var out []plannedRequest
	for {
		pr, ok := p.Next()
		if !ok {
			return out
		}
		out = append(out, pr)
	}
}

func fraction(reqs []plannedRequest, match func(plannedRequest) bool) float64 {
	n := 0
	for _, pr := range reqs {
		if match(pr) {
			n++
		}
	}
	return float64(n) / float64(len(reqs))
}

func TestPlanDeterministicSameSeed(t *testing.T) {
	pool := testPool(300)
	a := drain(newPlanner(planCfg(pool), 123))
	b := drain(newPlanner(planCfg(pool), 123))
	if len(a) == 0 || !reflect.DeepEqual(a, b) {
		t.Fatalf("mesma seed divergiu (%d vs %d requisições)", len(a), len(b))
	}
}

func TestPlanDiffersBySeed(t *testing.T) {
	pool := testPool(300)
	a := drain(newPlanner(planCfg(pool), 1))
	b := drain(newPlanner(planCfg(pool), 2))
	if reflect.DeepEqual(a, b) {
		t.Fatal("seeds diferentes produziram a mesma sequência")
	}
}

func TestPlanInstantsWithinRun(t *testing.T) {
	cfg := planCfg(testPool(50))
	cfg.warmup = 0.5
	span := cfg.warmup + cfg.duration
	for _, pr := range drain(newPlanner(cfg, 5)) {
		if pr.tPlan < 0 || pr.tPlan > span {
			t.Fatalf("instante %g fora de [0,%g]", pr.tPlan, span)
		}
	}
}

func TestPlanWarmupPrecedesWave(t *testing.T) {
	cfg := planCfg(testPool(50))
	cfg.warmup = 0.5
	reqs := drain(newPlanner(cfg, 5))
	wantWarmup := int(math.Round(cfg.rate * cfg.warmup))
	wantMain := int(math.Round(cfg.rate * cfg.duration))
	if len(reqs) != wantWarmup+wantMain {
		t.Fatalf("total %d, esperava %d", len(reqs), wantWarmup+wantMain)
	}
	for i, pr := range reqs {
		if pr.seq != i {
			t.Fatalf("seq %d na posição %d", pr.seq, i)
		}
		inWarmup := pr.tPlan < cfg.warmup
		if inWarmup != (i < wantWarmup) {
			t.Fatalf("requisição %d com t=%g: aquecimento deveria terminar em %d", i, pr.tPlan, wantWarmup)
		}
	}
}

func TestPlanPreservesTotalForEveryWaveform(t *testing.T) {
	for _, kind := range []string{waveSteady, waveRamp, wavePulse, waveWaves} {
		cfg := planCfg(testPool(50))
		cfg.wave, _ = newWaveform(kind)
		cfg.rate = 321
		cfg.duration = 2.5
		want := int(math.Round(cfg.rate * cfg.duration))
		if got := len(drain(newPlanner(cfg, 1))); got != want {
			t.Fatalf("%s: %d requisições, esperava %d", kind, got, want)
		}
	}
}

func TestPlanPostRatio(t *testing.T) {
	isPost := func(pr plannedRequest) bool { return pr.method == "POST" }
	for _, c := range []struct{ ratio, lo, hi float64 }{{0, 0, 0}, {1, 1, 1}, {0.3, 0.25, 0.35}} {
		cfg := planCfg(testPool(50))
		cfg.postRatio = c.ratio
		got := fraction(drain(newPlanner(cfg, 9)), isPost)
		if got < c.lo || got > c.hi {
			t.Fatalf("post-ratio %g gerou fração %g, fora de [%g,%g]", c.ratio, got, c.lo, c.hi)
		}
	}
}

func TestPlanHeavyRatioAndRoute(t *testing.T) {
	isHeavy := func(pr plannedRequest) bool { return strings.HasPrefix(pr.route, "/heavy?ms=200") }
	for _, c := range []struct{ ratio, lo, hi float64 }{{0, 0, 0}, {1, 1, 1}, {0.5, 0.45, 0.55}} {
		cfg := planCfg(testPool(50))
		cfg.heavyRatio = c.ratio
		reqs := drain(newPlanner(cfg, 9))
		got := fraction(reqs, isHeavy)
		if got < c.lo || got > c.hi {
			t.Fatalf("heavy-ratio %g gerou fração %g, fora de [%g,%g]", c.ratio, got, c.lo, c.hi)
		}
		for _, pr := range reqs {
			if !isHeavy(pr) && pr.route != "/api" {
				t.Fatalf("rota inesperada %q", pr.route)
			}
		}
	}
}

func TestPlanCacheBustShapeAndVariety(t *testing.T) {
	reqs := drain(newPlanner(planCfg(testPool(50)), 4))
	seen := map[string]bool{}
	for _, pr := range reqs {
		if len(pr.cacheBust) != cacheBustLen || strings.Trim(pr.cacheBust, string(cacheBustAlphabet)) != "" {
			t.Fatalf("cache-bust inválido: %q", pr.cacheBust)
		}
		seen[pr.cacheBust] = true
	}
	if len(seen) < len(reqs)/2 {
		t.Fatalf("cache-bust quase constante: %d valores em %d requisições", len(seen), len(reqs))
	}
}

func TestSelectorCoversPoolWhenNoChurn(t *testing.T) {
	const n = 100
	sel := newSelector(n, defaultPRepeat, 0, 0, 1, newRNG(11, planStream))
	seen := map[int]bool{}
	for i := range 1000 {
		seen[sel.pick(float64(i)/1000)] = true
	}
	if len(seen) != n {
		t.Fatalf("cobriu %d de %d índices", len(seen), n)
	}
}

func TestSelectorStickyHoldsIPUntilWindowEnds(t *testing.T) {
	const (
		sticky = 1.0
		calls  = 40
		step   = 0.25
	)
	sel := newSelector(100, 0, sticky, 0, calls*step, newRNG(11, planStream))
	switches := 0
	prev := -1
	for i := range calls {
		idx := sel.pick(float64(i) * step)
		if idx != prev {
			switches++
		}
		prev = idx
	}
	if want := int(calls * step / sticky); switches != want {
		t.Fatalf("%d trocas de IP, esperava %d", switches, want)
	}
}

func TestSelectorWithoutStickyChangesEveryRequest(t *testing.T) {
	sel := newSelector(100, 0, 0, 0, 1, newRNG(11, planStream))
	prev := -1
	for i := range 50 {
		idx := sel.pick(float64(i) / 50)
		if idx == prev {
			t.Fatalf("IP repetido consecutivamente em %d sem sticky nem p_repeat", i)
		}
		prev = idx
	}
}

func TestSelectorChurnMovesActiveWindow(t *testing.T) {
	const poolLen = 100
	sel := newSelector(poolLen, 0, 0, 0.5, 1, newRNG(3, planStream))
	for range 50 {
		if idx := sel.pick(0); idx >= poolLen/2 {
			t.Fatalf("no início só a primeira metade do pool é ativa, veio %d", idx)
		}
	}
	for range 50 {
		if idx := sel.pick(1); idx < poolLen/2 {
			t.Fatalf("no fim só a segunda metade do pool é ativa, veio %d", idx)
		}
	}
}

func TestActiveWindow(t *testing.T) {
	cases := []struct {
		f      float64
		churn  float64
		lo, hi int
	}{
		{0, 0.5, 0, 50},
		{0.5, 0.5, 25, 75},
		{1, 0.5, 50, 100},
		{1, 1, 99, 100},
	}
	for _, c := range cases {
		lo, hi := activeWindow(c.f, 100, c.churn)
		if lo != c.lo || hi != c.hi {
			t.Errorf("f=%g churn=%g: [%d,%d), esperava [%d,%d)", c.f, c.churn, lo, hi, c.lo, c.hi)
		}
	}
}
