package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	heavyWait   = 50 * time.Millisecond
	longWaitMS  = "5000"
	cancelBound = 2 * time.Second
	okBody      = "ok"
)

func serve(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler().ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestAPI(t *testing.T) {
	rec := serve(t, http.MethodGet, "/api")
	if rec.Code != http.StatusOK || rec.Body.String() != okBody {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestHeavyWaitsAtLeastRequested(t *testing.T) {
	start := time.Now()
	rec := serve(t, http.MethodGet, "/heavy?ms=50")
	if elapsed := time.Since(start); elapsed < heavyWait {
		t.Fatalf("respondeu em %v, menos que %v", elapsed, heavyWait)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != okBody {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestHeavyWithoutMSIsImmediate(t *testing.T) {
	for _, target := range []string{"/heavy", "/heavy?ms=0"} {
		if rec := serve(t, http.MethodGet, target); rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d", target, rec.Code)
		}
	}
}

func TestHeavyInvalidMS(t *testing.T) {
	for _, target := range []string{"/heavy?ms=abc", "/heavy?ms=-1", "/heavy?ms=60001"} {
		if rec := serve(t, http.MethodGet, target); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d", target, rec.Code)
		}
	}
}

func TestHeavyCanceledContextReturnsEarly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/heavy?ms="+longWaitMS, nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		handler().ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(cancelBound):
		t.Fatal("handler nao retornou apos cancelamento")
	}
}

func TestNonGetIsRejected(t *testing.T) {
	for _, target := range []string{"/api", "/heavy"} {
		if rec := serve(t, http.MethodPost, target); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: got %d", target, rec.Code)
		}
	}
}
