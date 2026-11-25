package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
)

const (
	writerBufBytes = 1 << 20
	timeDecimals   = 6
)

type record struct {
	tStart  float64
	tEnd    float64
	status  int
	outcome string
}

type writer struct {
	f       *os.File
	buf     *bufio.Writer
	scratch []byte
}

func newWriter(path string) (*writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("cria %s: %w", path, err)
	}
	return &writer{f: f, buf: bufio.NewWriterSize(f, writerBufBytes)}, nil
}

func (w *writer) write(r record) error {
	b := w.scratch[:0]
	b = append(b, `{"t_start":`...)
	b = strconv.AppendFloat(b, r.tStart, 'f', timeDecimals, 64)
	b = append(b, `,"t_end":`...)
	b = strconv.AppendFloat(b, r.tEnd, 'f', timeDecimals, 64)
	b = append(b, `,"status":`...)
	b = strconv.AppendInt(b, int64(r.status), 10)
	b = append(b, `,"outcome":"`...)
	b = append(b, r.outcome...)
	b = append(b, '"', '}', '\n')
	w.scratch = b
	if _, err := w.buf.Write(b); err != nil {
		return fmt.Errorf("escreve linha: %w", err)
	}
	return nil
}

func (w *writer) close() error {
	if err := w.buf.Flush(); err != nil {
		w.f.Close()
		return fmt.Errorf("flush: %w", err)
	}
	return w.f.Close()
}
