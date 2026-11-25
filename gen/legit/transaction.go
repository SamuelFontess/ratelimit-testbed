package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

const (
	OutcomeOK          = "ok"
	OutcomePartial     = "fail_partial"
	OutcomeTotal       = "fail_total"
	OutcomeRefused     = "fail_refused"
	noResponseStatus   = 0
	successStatusFloor = 200
	successStatusCeil  = 300
)

func transact(client *http.Client, base *http.Request, total time.Duration) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()
	resp, err := client.Do(base.Clone(ctx))
	if err != nil {
		return noResponseStatus, classifyError(ctx, err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return resp.StatusCode, classifyError(ctx, err)
	}
	return resp.StatusCode, classifyStatus(resp.StatusCode)
}

func classifyStatus(status int) string {
	if status >= successStatusFloor && status < successStatusCeil {
		return OutcomeOK
	}
	return OutcomeRefused
}

func classifyError(ctx context.Context, err error) string {
	var netErr net.Error
	switch {
	case ctx.Err() != nil:
		return OutcomeTotal
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return OutcomeRefused
	case errors.As(err, &netErr) && netErr.Timeout():
		return OutcomePartial
	default:
		return OutcomeTotal
	}
}
