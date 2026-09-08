package ratelimiter_test

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/roadrunner-server/config/v6"
	"github.com/roadrunner-server/endure/v2"
	httpPlugin "github.com/roadrunner-server/http/v6"
	"github.com/roadrunner-server/http/v6/api"
	"github.com/roadrunner-server/logger/v6"
	ratelimiter "github.com/roadrunner-server/rate-limiter/v6"
	"github.com/roadrunner-server/server/v6"
	"github.com/stretchr/testify/require"
)

var _ api.Middleware = (*ratelimiter.Plugin)(nil)

func TestHTTPMiddleware(t *testing.T) {
	type request struct {
		method  string
		key     string
		status  int
		counter int
	}

	for _, tc := range []struct {
		name       string
		middleware string
		requests   []request
	}{
		{
			name:       "configured",
			middleware: "rate_limiter",
			requests: []request{
				{http.MethodGet, "client-a", http.StatusOK, 1},
				{http.MethodGet, "client-a", http.StatusOK, 2},
				{http.MethodGet, "client-a", http.StatusTooManyRequests, 0},
				{http.MethodGet, "client-b", http.StatusOK, 3},
				{http.MethodGet, "client-c", http.StatusServiceUnavailable, 0},
				{http.MethodGet, "client-b", http.StatusOK, 4},
				{http.MethodHead, "client-a", http.StatusTooManyRequests, 0},
			},
		},
		{
			name: "unconfigured",
			requests: []request{
				{http.MethodGet, "client-a", http.StatusOK, 1},
				{http.MethodGet, "client-a", http.StatusOK, 2},
				{http.MethodGet, "client-a", http.StatusOK, 3},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, url := startHTTP(t, tc.middleware)
			for i, step := range tc.requests {
				req, err := http.NewRequestWithContext(t.Context(), step.method, url, nil)
				require.NoError(t, err)
				req.Header.Set("X-Client-ID", step.key)

				resp, err := client.Do(req)
				require.NoError(t, err)
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				require.NoError(t, err)
				require.Equal(t, step.status, resp.StatusCode, "request %d", i+1)

				if step.status == http.StatusOK {
					require.Equal(t, strconv.Itoa(step.counter), string(body))
					require.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))
					require.Empty(t, resp.Header.Get("Retry-After"))
				}
				if step.status == http.StatusTooManyRequests {
					require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
					retryAfter, err := strconv.Atoi(resp.Header.Get("Retry-After"))
					require.NoError(t, err)
					require.GreaterOrEqual(t, retryAfter, 1)
					require.LessOrEqual(t, retryAfter, 3600)
				}
				if step.method == http.MethodHead {
					require.Empty(t, body)
				}
			}
		})
	}
}

func startHTTP(t *testing.T, middleware string) (*http.Client, string) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())

	cfg := &config.Plugin{
		Version: "2024.2.0",
		Type:    "yaml",
		ReadInCfg: fmt.Appendf(nil, `version: "3"
server:
  command: "php php_test_files/worker.php"
  relay: pipes
http:
  address: %q
  middleware: [%s]
  rate_limiter:
    key: header
    header: X-Client-ID
    rate: 1
    interval: 1h
    burst: 2
    max_entries: 2
  pool:
    num_workers: 1
    allocate_timeout: 10s
    destroy_timeout: 5s
logs:
  mode: development
  level: error
`, address, middleware),
	}
	container := endure.New(slog.LevelError, endure.GracefulShutdownTimeout(10*time.Second))
	require.NoError(t, container.RegisterAll(cfg, &logger.Plugin{}, &server.Plugin{}, &httpPlugin.Plugin{}, &ratelimiter.Plugin{}))
	require.NoError(t, container.Init())

	done := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() {
		if err := container.Stop(); err != nil {
			t.Errorf("stop container: %v", err)
		}
		close(done)
		wg.Wait()
	})
	results, err := container.Serve()
	require.NoError(t, err)
	wg.Go(func() {
		for {
			select {
			case result := <-results:
				if result == nil {
					return
				}
				t.Errorf("plugin %s: %v", result.VertexID, result.Error)
			case <-done:
				return
			}
		}
	})

	dialer := net.Dialer{Timeout: time.Second}
	require.Eventually(t, func() bool {
		conn, err := dialer.DialContext(t.Context(), "tcp", address)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 15*time.Second, 20*time.Millisecond, "HTTP server did not become ready")

	transport := &http.Transport{DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}, "http://" + address
}
