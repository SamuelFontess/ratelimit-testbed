package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

const (
	defaultAddr       = "127.0.0.1:8080"
	maxHeavyMS        = 60000
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 10 * time.Second
)

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api", onlyGet(api))
	mux.HandleFunc("/heavy", onlyGet(heavy))
	return mux
}

func onlyGet(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

func api(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprint(w, "ok")
}

func heavy(w http.ResponseWriter, r *http.Request) {
	ms, err := parseMS(r.URL.Query().Get("ms"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if ms > 0 {
		timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
	}
	fmt.Fprint(w, "ok")
}

func parseMS(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	ms, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("ms invalido: %w", err)
	}
	if ms < 0 || ms > maxHeavyMS {
		return 0, fmt.Errorf("ms fora de 0..%d: %d", maxHeavyMS, ms)
	}
	return ms, nil
}

func run(addr string) error {
	srv := &http.Server{Addr: addr, Handler: handler(), ReadHeaderTimeout: readHeaderTimeout}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		return fmt.Errorf("servidor: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("servidor: %w", err)
	}
	return nil
}

func main() {
	addr := flag.String("addr", defaultAddr, "endereco de escuta")
	flag.Parse()
	if err := run(*addr); err != nil {
		log.Fatal(err)
	}
}
