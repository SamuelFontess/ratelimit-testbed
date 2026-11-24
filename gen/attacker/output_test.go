package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriterEmitsContractJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attacker.jsonl")
	w, err := newWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	records := []record{
		{tStart: 0.5, tEnd: 1.25, status: 200, outcome: outcomeOK},
		{tStart: 2, tEnd: 2.0000004, status: 0, outcome: outcomeTotal},
	}
	for _, r := range records {
		if err := w.write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"t_start":0.500000,"t_end":1.250000,"status":200,"outcome":"ok"}` + "\n" +
		`{"t_start":2.000000,"t_end":2.000000,"status":0,"outcome":"fail_total"}` + "\n"
	if string(got) != want {
		t.Fatalf("saída:\n%s\nesperada:\n%s", got, want)
	}
}

func TestNewWriterFailsOnUnwritablePath(t *testing.T) {
	if _, err := newWriter(filepath.Join(t.TempDir(), "ausente", "x.jsonl")); err == nil {
		t.Fatal("devia falhar ao criar arquivo em diretório inexistente")
	}
}
