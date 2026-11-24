package main

import (
	"fmt"
	"math"
)

const (
	waveSteady = "steady"
	waveRamp   = "ramp"
	wavePulse  = "pulse"
	waveWaves  = "waves"

	pulseDuty      = 0.5
	pulseCycles    = 6
	waveAmplitude  = 0.8
	waveCount      = 4
	bisectionSteps = 60
	binCenter      = 0.5
)

type waveform struct {
	kind string
}

func newWaveform(kind string) (waveform, error) {
	switch kind {
	case waveSteady, waveRamp, wavePulse, waveWaves:
		return waveform{kind: kind}, nil
	default:
		return waveform{}, fmt.Errorf("forma de onda desconhecida: %q", kind)
	}
}

func binMidpoint(i, total int) float64 {
	return (float64(i) + binCenter) / float64(total)
}

func (w waveform) at(i, total int, span float64) float64 {
	u := binMidpoint(i, total)
	switch w.kind {
	case waveRamp:
		return math.Sqrt(u) * span
	case wavePulse:
		return pulseCumulativeAt(u) * span
	case waveWaves:
		return inverseWaveCDF(u) * span
	default:
		return u * span
	}
}

func pulseCumulativeAt(u float64) float64 {
	x := u * pulseCycles
	c := math.Floor(x)
	off := x - c
	return (c + off*pulseDuty) / pulseCycles
}

func waveCDF(t float64) float64 {
	k := 2 * math.Pi * waveCount
	return t - waveAmplitude/k*(math.Cos(k*t)-1)
}

func inverseWaveCDF(u float64) float64 {
	lo, hi := 0.0, 1.0
	for range bisectionSteps {
		mid := (lo + hi) / 2
		if waveCDF(mid) < u {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}
