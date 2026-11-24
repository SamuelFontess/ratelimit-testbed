package main

import (
	"math"
	"testing"
)

func TestWaveformsMonotonicWithinSpan(t *testing.T) {
	const (
		total = 2000
		span  = 10.0
	)
	for _, kind := range []string{waveSteady, waveRamp, wavePulse, waveWaves} {
		w, err := newWaveform(kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		prev := -1.0
		for i := range total {
			got := w.at(i, total, span)
			if got < 0 || got > span {
				t.Fatalf("%s: instante %g fora de [0,%g]", kind, got, span)
			}
			if got < prev {
				t.Fatalf("%s: não monotônico em i=%d (%g < %g)", kind, i, got, prev)
			}
			prev = got
		}
	}
}

func TestWaveformSteadyIsUniform(t *testing.T) {
	w, _ := newWaveform(waveSteady)
	const total = 1000
	mid := w.at(total/2, total, 1)
	if math.Abs(mid-0.5) > 0.01 {
		t.Fatalf("steady no meio deu %g, esperava ~0,5", mid)
	}
}

func TestWaveformRampStartsSlow(t *testing.T) {
	w, _ := newWaveform(waveRamp)
	const total = 1000
	if mid := w.at(total/2, total, 1); mid < 0.6 {
		t.Fatalf("ramp no meio deu %g, esperava > 0,6", mid)
	}
}

func TestWaveformPulseOnlyFiresInOnPhase(t *testing.T) {
	w, _ := newWaveform(wavePulse)
	const total = 3000
	for i := range total {
		cyclePos := math.Mod(w.at(i, total, 1)*pulseCycles, 1)
		if cyclePos > pulseDuty {
			t.Fatalf("instante %d caiu na fase off do pulso (posição %g no ciclo)", i, cyclePos)
		}
	}
}

func TestWaveformWavesConcentrateInRisingHalf(t *testing.T) {
	w, _ := newWaveform(waveWaves)
	const total = 4000
	rising := 0
	for i := range total {
		if math.Mod(w.at(i, total, 1)*waveCount, 1) < 0.5 {
			rising++
		}
	}
	if falling := total - rising; rising < 2*falling {
		t.Fatalf("meia-onda crescente com %d e decrescente com %d: sem concentração", rising, falling)
	}
}

func TestUnknownWaveformRejected(t *testing.T) {
	if _, err := newWaveform("zigzag"); err == nil {
		t.Fatal("aceitou forma de onda desconhecida")
	}
}
