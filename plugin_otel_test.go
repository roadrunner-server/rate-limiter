package ratelimiter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	rrcontext "github.com/roadrunner-server/context"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestMiddlewareTracing(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	ctx, parent := tp.Tracer("test").Start(t.Context(), "parent")
	defer parent.End()
	ctx = context.WithValue(ctx, rrcontext.OtelTracerNameKey, "test-tracer")
	p := newPlugin(t, func(c *Config) { c.Rate, c.Interval = 1, time.Hour })
	calls := 0
	h := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		spans := exporter.GetSpans()
		require.Len(t, spans, 1)
		require.Equal(t, spans[0].SpanContext, trace.SpanContextFromContext(r.Context()))
		extracted := (propagation.TraceContext{}).Extract(context.Background(), propagation.HeaderCarrier(r.Header))
		require.Equal(t, spans[0].SpanContext.TraceID(), trace.SpanContextFromContext(extracted).TraceID())
		require.Equal(t, spans[0].SpanContext.SpanID(), trace.SpanContextFromContext(extracted).SpanID())
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, status := range []int{http.StatusNoContent, http.StatusTooManyRequests} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil))
		require.Equal(t, status, w.Code)
	}
	require.Equal(t, 1, calls)
	spans := exporter.GetSpans()
	require.Len(t, spans, 2)
	for _, span := range spans {
		require.Equal(t, PluginName, span.Name)
		require.Equal(t, trace.SpanKindInternal, span.SpanKind)
		require.Equal(t, parent.SpanContext(), span.Parent)
		require.False(t, span.EndTime.IsZero())
	}
}
