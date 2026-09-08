package ratelimiter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/roadrunner-server/errors"
	"github.com/stretchr/testify/require"
)

type stubConfigurer struct {
	enabled   bool
	configure func(*Config)
	err       error
}

func (s stubConfigurer) Has(name string) bool {
	return name == "http" || name == configKey && s.enabled
}

func (s stubConfigurer) UnmarshalKey(name string, out any) error {
	if name != configKey {
		return errors.Str("unexpected configuration key")
	}
	if s.err != nil {
		return s.err
	}
	switch target := out.(type) {
	case *Config:
		s.configure(target)
	case *map[string]any:
	default:
		return errors.Str("unexpected configuration target")
	}
	return nil
}

func newPlugin(t *testing.T, configure func(*Config)) *Plugin {
	t.Helper()
	p := &Plugin{}
	require.NoError(t, p.Init(stubConfigurer{enabled: true, configure: configure}))
	return p
}

func TestInit(t *testing.T) {
	err := (&Plugin{}).Init(stubConfigurer{})
	require.True(t, errors.Is(errors.Disabled, err))

	err = (&Plugin{}).Init(stubConfigurer{enabled: true, err: errors.Str("decode failure")})
	require.ErrorContains(t, err, "decode failure")
	require.False(t, errors.Is(errors.Disabled, err))
}

func TestMiddlewareQuota(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newPlugin(t, func(c *Config) { c.Rate, c.Burst = 2, 3 })
		calls := 0
		h := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			require.Equal(t, "input", r.Header.Get("X-Test"))
			w.Header().Set("X-Worker", "present")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "worker response")
		}))
		request := func(method string, status int) *httptest.ResponseRecorder {
			t.Helper()
			r := httptest.NewRequestWithContext(t.Context(), method, "/", nil)
			r.Header.Set("X-Test", "input")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			require.Equal(t, status, w.Code)
			if status == http.StatusCreated {
				require.Equal(t, "present", w.Header().Get("X-Worker"))
				require.Equal(t, "worker response", w.Body.String())
				require.Empty(t, w.Header().Get("Retry-After"))
			} else {
				require.Empty(t, w.Header().Get("X-Worker"))
				require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
				require.Equal(t, "1", w.Header().Get("Retry-After"))
			}
			return w
		}

		for range 3 {
			request(http.MethodGet, http.StatusCreated)
		}
		request(http.MethodGet, http.StatusTooManyRequests)
		time.Sleep(499 * time.Millisecond)
		request(http.MethodGet, http.StatusTooManyRequests)
		time.Sleep(time.Millisecond)
		request(http.MethodGet, http.StatusCreated)
		require.Empty(t, request(http.MethodHead, http.StatusTooManyRequests).Body.String())
		time.Sleep(10 * time.Second)
		for range 3 {
			request(http.MethodGet, http.StatusCreated)
		}
		request(http.MethodGet, http.StatusTooManyRequests)
		require.Equal(t, 7, calls)
	})
}

func TestMiddlewareKeys(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		key                                          Key
		firstIP, secondIP, firstHeader, secondHeader string
		status                                       int
	}{
		{"global", KeyGlobal, "192.0.2.1:1234", "", "a", "b", http.StatusTooManyRequests},
		{"source ports", KeyIP, "192.0.2.1:1234", "192.0.2.1:5678", "", "", http.StatusTooManyRequests},
		{"bare IPv4", KeyIP, "192.0.2.1:1234", "192.0.2.1", "", "", http.StatusTooManyRequests},
		{"mapped IPv4", KeyIP, "192.0.2.1:1234", "[::ffff:192.0.2.1]:5678", "", "", http.StatusTooManyRequests},
		{"IPv6", KeyIP, "[2001:db8::1]:1234", "2001:0db8:0:0:0:0:0:1", "", "", http.StatusTooManyRequests},
		{"bracketed IPv6", KeyIP, "[2001:db8::1]:1234", "[2001:db8::1]", "", "", http.StatusTooManyRequests},
		{"distinct IPs", KeyIP, "192.0.2.1:1234", "192.0.2.2:1234", "", "", http.StatusNoContent},
		{"invalid IP", KeyIP, "192.0.2.1:1234", "unknown", "", "", http.StatusBadRequest},
		{"missing IP", KeyIP, "192.0.2.1:1234", "", "", "", http.StatusBadRequest},
		{"same header", KeyHeader, "192.0.2.1:1234", "192.0.2.2:1234", "client", " client ", http.StatusTooManyRequests},
		{"distinct headers", KeyHeader, "", "", "client", "CLIENT", http.StatusNoContent},
		{"missing header", KeyHeader, "", "", "client", "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlugin(t, func(c *Config) {
				c.Key, c.Rate, c.Interval = tc.key, 1, time.Hour
				if tc.key == KeyHeader {
					c.Header = "x-client-id"
				}
			})
			h := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			for i, ip := range []string{tc.firstIP, tc.secondIP} {
				r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
				r.RemoteAddr = ip
				r.Header.Set("X-Client-ID", []string{tc.firstHeader, tc.secondHeader}[i])
				r.Header.Set("X-Forwarded-For", []string{"198.51.100.1", "198.51.100.2"}[i])
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				status := http.StatusNoContent
				if i == 1 {
					status = tc.status
				}
				require.Equal(t, status, w.Code)
			}
		})
	}
}

func TestMiddlewareHost(t *testing.T) {
	p := newPlugin(t, func(c *Config) {
		c.Key, c.Header, c.Rate, c.Interval = KeyHeader, "host", 1, time.Hour
	})
	h := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for i, host := range []string{"a.example", "a.example", "b.example"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host+"/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		status := http.StatusNoContent
		if i == 1 {
			status = http.StatusTooManyRequests
		}
		require.Equal(t, status, w.Code)
	}
}

func TestMiddlewareConcurrentWrappers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newPlugin(t, func(c *Config) { c.Rate, c.Burst = 1, 7 })
		var allowed, denied, unexpected atomic.Int32
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			allowed.Add(1)
			w.WriteHeader(http.StatusNoContent)
		})
		handlers := []http.Handler{p.Middleware(next), p.Middleware(next)}
		var wg sync.WaitGroup
		for i := range 64 {
			wg.Go(func() {
				w := httptest.NewRecorder()
				handlers[i%2].ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
				switch w.Code {
				case http.StatusTooManyRequests:
					denied.Add(1)
				case http.StatusNoContent:
				default:
					unexpected.Add(1)
				}
			})
		}
		wg.Wait()
		require.EqualValues(t, 7, allowed.Load())
		require.EqualValues(t, 57, denied.Load())
		require.Zero(t, unexpected.Load())
	})
}

func TestMiddlewareCapacityAndCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newPlugin(t, func(c *Config) {
			c.Key, c.Header = KeyHeader, "X-Client-ID"
			c.Rate, c.Interval, c.Burst, c.MaxEntries = 1, time.Minute, 2, 2
		})
		h := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		request := func(key string, status int) {
			t.Helper()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			r.Header.Set("X-Client-ID", key)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			require.Equal(t, status, w.Code, "key %s", key)
			require.LessOrEqual(t, len(p.buckets), 2)
		}

		request("a", http.StatusNoContent)
		request("a", http.StatusNoContent)
		request("b", http.StatusNoContent)
		request("c", http.StatusServiceUnavailable)
		time.Sleep(time.Minute - time.Millisecond)
		request("c", http.StatusServiceUnavailable)
		time.Sleep(time.Millisecond)
		request("c", http.StatusNoContent)
		request("a", http.StatusNoContent)
		request("a", http.StatusTooManyRequests)
		time.Sleep(time.Minute)
		request("d", http.StatusNoContent)
	})
}
