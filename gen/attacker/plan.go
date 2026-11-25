package main

import (
	"math"
	"math/rand/v2"
	"net"
	"strconv"
)

const cacheBustLen = 10

var cacheBustAlphabet = []byte("abcdefghijklmnopqrstuvwxyz0123456789")

type plannedRequest struct {
	seq       int
	ip        net.IP
	method    string
	route     string
	ua        string
	headers   [][2]string
	cacheBust string
	tPlan     float64
}

type planConfig struct {
	pool       []net.IP
	rate       float64
	warmup     float64
	duration   float64
	postRatio  float64
	heavyRatio float64
	heavyMS    int
	uas        []string
	headerPool [][2]string
	pRepeat    float64
	sticky     float64
	churn      float64
	wave       waveform
}

type planner struct {
	cfg     planConfig
	rng     *rand.Rand
	sel     *selector
	i       int
	warmupN int
	mainN   int
}

func newPlanner(cfg planConfig, seed int64) *planner {
	rng := newRNG(seed, planStream)
	span := cfg.warmup + cfg.duration
	return &planner{
		cfg:     cfg,
		rng:     rng,
		sel:     newSelector(len(cfg.pool), cfg.pRepeat, cfg.sticky, cfg.churn, span, rng),
		warmupN: int(math.Round(cfg.rate * cfg.warmup)),
		mainN:   int(math.Round(cfg.rate * cfg.duration)),
	}
}

func (p *planner) total() int { return p.warmupN + p.mainN }

func (p *planner) Next() (plannedRequest, bool) {
	if p.i >= p.total() {
		return plannedRequest{}, false
	}
	i := p.i
	p.i++

	var t float64
	if i < p.warmupN {
		t = binMidpoint(i, p.warmupN) * p.cfg.warmup
	} else {
		t = p.cfg.warmup + p.cfg.wave.at(i-p.warmupN, p.mainN, p.cfg.duration)
	}

	idx := p.sel.pick(t)
	pr := plannedRequest{seq: i, ip: p.cfg.pool[idx], tPlan: t}
	pr.method = p.pickMethod()
	pr.route = p.pickRoute()
	pr.cacheBust = p.cacheBust()
	pr.ua = p.cfg.uas[p.rng.IntN(len(p.cfg.uas))]
	pr.headers = p.pickHeaders()
	return pr, true
}

func (p *planner) pickMethod() string {
	if p.rng.Float64() < p.cfg.postRatio {
		return "POST"
	}
	return "GET"
}

func (p *planner) pickRoute() string {
	if p.rng.Float64() < p.cfg.heavyRatio {
		return "/heavy?ms=" + strconv.Itoa(p.cfg.heavyMS)
	}
	return "/api"
}

func (p *planner) cacheBust() string {
	b := make([]byte, cacheBustLen)
	for i := range b {
		b[i] = cacheBustAlphabet[p.rng.IntN(len(cacheBustAlphabet))]
	}
	return string(b)
}

func (p *planner) pickHeaders() [][2]string {
	if len(p.cfg.headerPool) == 0 {
		return nil
	}
	n := p.rng.IntN(len(p.cfg.headerPool) + 1)
	if n == 0 {
		return nil
	}
	perm := p.rng.Perm(len(p.cfg.headerPool))
	hs := make([][2]string, n)
	for i := range n {
		hs[i] = p.cfg.headerPool[perm[i]]
	}
	return hs
}

type selector struct {
	poolLen    int
	pRepeat    float64
	sticky     float64
	churn      float64
	span       float64
	rng        *rand.Rand
	fresh      []int
	usedSet    map[int]bool
	usedList   []int
	current    int
	lastSwitch float64
	have       bool
}

func newSelector(poolLen int, pRepeat, sticky, churn, span float64, rng *rand.Rand) *selector {
	return &selector{
		poolLen: poolLen,
		pRepeat: pRepeat,
		sticky:  sticky,
		churn:   churn,
		span:    span,
		rng:     rng,
		usedSet: make(map[int]bool),
	}
}

func (s *selector) pick(t float64) int {
	if s.have && s.sticky > 0 && t-s.lastSwitch < s.sticky {
		return s.current
	}
	var idx int
	if s.have && len(s.usedList) > 0 && s.rng.Float64() < s.pRepeat {
		idx = s.usedList[s.rng.IntN(len(s.usedList))]
	} else {
		idx = s.freshPick(t)
		if !s.usedSet[idx] {
			s.usedSet[idx] = true
			s.usedList = append(s.usedList, idx)
		}
	}
	s.current = idx
	s.lastSwitch = t
	s.have = true
	return idx
}

func (s *selector) freshPick(t float64) int {
	if s.churn <= 0 {
		if len(s.fresh) == 0 {
			s.fresh = s.rng.Perm(s.poolLen)
		}
		idx := s.fresh[len(s.fresh)-1]
		s.fresh = s.fresh[:len(s.fresh)-1]
		return idx
	}
	lo, hi := activeWindow(t/s.span, s.poolLen, s.churn)
	return lo + s.rng.IntN(hi-lo)
}

func activeWindow(f float64, poolLen int, churn float64) (int, int) {
	width := max(int(math.Round(float64(poolLen)*(1-churn))), 1)
	start := int(f * churn * float64(poolLen))
	if start+width > poolLen {
		start = poolLen - width
	}
	return start, start + width
}
