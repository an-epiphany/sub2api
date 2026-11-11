package telemetry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// InstrumentUpstreamClient returns a shallow copy of client whose transport
// records each upstream attempt as a client span. The span lasts until the
// response body is fully read or closed, so streamed (SSE) responses are
// covered end to end.
//
// Only requests that already belong to a traced inbound request are
// recorded; background calls are passed through untouched. Trace headers are
// never injected into upstream requests, and the URL query string (which may
// carry provider API keys) is never recorded.
func InstrumentUpstreamClient(client *http.Client, accountID int64) *http.Client {
	state := active.Load()
	if state == nil || client == nil {
		return client
	}
	clone := *client
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone.Transport = &upstreamTransport{base: base, tracer: state.tracer, accountID: accountID}
	return &clone
}

type upstreamTransport struct {
	base      http.RoundTripper
	tracer    trace.Tracer
	accountID int64
}

func (t *upstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || !trace.SpanContextFromContext(req.Context()).IsValid() {
		return t.base.RoundTrip(req)
	}

	host := req.URL.Hostname()
	ctx, span := t.tracer.Start(req.Context(), "upstream "+req.Method+" "+host,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.HTTPRequestMethodKey.String(req.Method),
			semconv.ServerAddress(host),
			semconv.URLFull(urlWithoutQuery(req.URL)),
		),
	)
	if port := urlPort(req.URL); port > 0 {
		span.SetAttributes(semconv.ServerPort(port))
	}
	if t.accountID > 0 {
		span.SetAttributes(attrAccountID.Int64(t.accountID))
	}

	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		endSpan(ctx, span, err)
		return resp, err
	}

	span.SetAttributes(semconv.HTTPResponseStatusCode(resp.StatusCode))
	if resp.StatusCode >= http.StatusBadRequest {
		span.SetStatus(codes.Error, http.StatusText(resp.StatusCode))
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		span.End()
		return resp, nil
	}
	resp.Body = &spanBody{ReadCloser: resp.Body, ctx: ctx, span: span}
	return resp, nil
}

// spanBody ends the upstream span when the body reaches EOF, fails, or is
// closed, whichever happens first.
type spanBody struct {
	io.ReadCloser
	ctx       context.Context
	span      trace.Span
	firstByte bool
	once      sync.Once
}

func (b *spanBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.firstByte {
		b.firstByte = true
		b.span.AddEvent("first_byte")
	}
	switch {
	case err == nil:
	case errors.Is(err, io.EOF):
		b.end(nil)
	default:
		b.end(err)
	}
	return n, err
}

func (b *spanBody) Close() error {
	err := b.ReadCloser.Close()
	b.end(nil)
	return err
}

func (b *spanBody) end(err error) {
	b.once.Do(func() { endSpan(b.ctx, b.span, err) })
}

// endSpan ends an upstream span, marking err as an error unless the request
// context was cancelled. A cancelled request was stopped by the gateway, not
// failed by the upstream: the gateway cancels an attempt when it is done with
// it (for example, it closes the body right after a terminal SSE event, which
// cancels the request first) or when the client has left. Reads still in
// flight at that point, such as the zstd decoder's read-ahead, then fail with
// context.Canceled. A deadline is still an error: the upstream was too slow.
func endSpan(ctx context.Context, span trace.Span, err error) {
	if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		// *url.Error embeds the full request URL, query string included.
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Err != nil {
			err = urlErr.Err
		}
		span.SetAttributes(semconv.ErrorType(err))
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func urlWithoutQuery(u *url.URL) string {
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}).String()
}

func urlPort(u *url.URL) int {
	if p := u.Port(); p != "" {
		port, _ := strconv.Atoi(p)
		return port
	}
	switch u.Scheme {
	case "https", "wss":
		return 443
	case "http", "ws":
		return 80
	}
	return 0
}
