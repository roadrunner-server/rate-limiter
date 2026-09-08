package ratelimiter

import (
	"crypto/sha256"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	rrcontext "github.com/roadrunner-server/context"
	"github.com/roadrunner-server/errors"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	jprop "go.opentelemetry.io/contrib/propagators/jaeger"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"
)

const (
	PluginName = "rate_limiter"
	configKey  = "http." + PluginName
)

type Configurer interface {
	UnmarshalKey(name string, out any) error
	Has(name string) bool
}

type Plugin struct {
	cfg         Config
	prop        propagation.TextMapPropagator
	limit       rate.Limit
	mu          sync.Mutex
	buckets     map[string]*rate.Limiter
	nextCleanup time.Time
}

func (p *Plugin) Init(cfg Configurer) error {
	const op = errors.Op("rate_limiter_plugin_init")

	if !cfg.Has("http") || !cfg.Has(configKey) {
		return errors.E(op, errors.Disabled)
	}

	p.cfg = Config{Key: KeyIP, Interval: time.Second, Burst: 1, MaxEntries: 10000}
	if err := cfg.UnmarshalKey(configKey, &p.cfg); err != nil {
		return errors.E(op, err)
	}
	if err := p.cfg.validate(); err != nil {
		return errors.E(op, err)
	}

	p.limit = rate.Limit(float64(p.cfg.Rate) / p.cfg.Interval.Seconds())
	p.buckets = make(map[string]*rate.Limiter)
	p.nextCleanup = time.Now().Add(time.Minute)
	p.prop = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}, jprop.Jaeger{})
	return nil
}

func (p *Plugin) Name() string {
	return PluginName
}

func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var span trace.Span
		if name, ok := r.Context().Value(rrcontext.OtelTracerNameKey).(string); ok {
			tp := trace.SpanFromContext(r.Context()).TracerProvider()
			ctx, s := tp.Tracer(name, trace.WithSchemaURL(semconv.SchemaURL),
				trace.WithInstrumentationVersion(otelhttp.Version)).
				Start(r.Context(), PluginName, trace.WithSpanKind(trace.SpanKindInternal))
			span = s
			r = r.WithContext(ctx)
		}

		status := http.StatusBadRequest
		var retryAfter int64
		if key, ok := p.requestKey(r); ok {
			status, retryAfter = p.allow(key)
		}
		if span != nil {
			p.prop.Inject(r.Context(), propagation.HeaderCarrier(r.Header))
			span.End()
		}
		if status == 0 {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Del("Content-Length")
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
		}
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, http.StatusText(status)+"\n")
		}
	})
}

func (p *Plugin) requestKey(r *http.Request) (string, bool) {
	switch p.cfg.Key {
	case KeyGlobal:
		return "", true
	case KeyHeader:
		value := r.Header.Get(p.cfg.Header)
		if p.cfg.Header == "Host" {
			value = r.Host
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return "", false
		}
		digest := sha256.Sum256([]byte(value))
		return string(digest[:]), true
	default:
		address := strings.TrimSpace(r.RemoteAddr)
		if host, _, err := net.SplitHostPort(address); err == nil {
			address = host
		} else if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
			address = address[1 : len(address)-1]
		}
		ip, err := netip.ParseAddr(address)
		if err != nil {
			return "", false
		}
		return ip.Unmap().WithZone("").String(), true
	}
}

func (p *Plugin) allow(key string) (int, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	if !now.Before(p.nextCleanup) {
		// A full bucket can be recreated without granting extra tokens.
		maps.DeleteFunc(p.buckets, func(_ string, bucket *rate.Limiter) bool {
			return bucket.TokensAt(now) >= float64(p.cfg.Burst)
		})
		p.nextCleanup = now.Add(time.Minute)
	}

	bucket, ok := p.buckets[key]
	if !ok {
		if len(p.buckets) >= p.cfg.MaxEntries {
			return http.StatusServiceUnavailable, 0
		}
		bucket = rate.NewLimiter(p.limit, p.cfg.Burst)
		p.buckets[key] = bucket
	}
	if bucket.AllowN(now, 1) {
		return 0, 0
	}

	retryAfter := int64(math.Ceil((1 - bucket.TokensAt(now)) / float64(p.limit)))
	return http.StatusTooManyRequests, max(1, retryAfter)
}
