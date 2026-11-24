package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
)

type Client struct {
	CID   int    `json:"cid"`
	Src   string `json:"src"`
	Group string `json:"group"`
}

type Event struct {
	T   float64 `json:"t"`
	CID int     `json:"cid"`
}

type Schedule struct {
	Clients []Client `json:"clients"`
	Events  []Event  `json:"events"`
}

func loadSchedule(path string) (Schedule, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Schedule{}, fmt.Errorf("ler agenda: %w", err)
	}
	var s Schedule
	if err := json.Unmarshal(raw, &s); err != nil {
		return Schedule{}, fmt.Errorf("decodificar agenda %s: %w", path, err)
	}
	if err := s.validate(); err != nil {
		return Schedule{}, fmt.Errorf("agenda %s: %w", path, err)
	}
	return s, nil
}

func (s Schedule) validate() error {
	known := make(map[int]bool, len(s.Clients))
	for _, c := range s.Clients {
		if net.ParseIP(c.Src) == nil {
			return fmt.Errorf("cliente %d com src invalido %q", c.CID, c.Src)
		}
		known[c.CID] = true
	}
	for i, e := range s.Events {
		if !known[e.CID] {
			return fmt.Errorf("evento %d usa cid %d ausente em clients", i, e.CID)
		}
		if i > 0 && e.T < s.Events[i-1].T {
			return fmt.Errorf("evento %d fora de ordem: t=%v antes de t=%v", i, e.T, s.Events[i-1].T)
		}
	}
	return nil
}
