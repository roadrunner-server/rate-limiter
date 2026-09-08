package ratelimiter_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/roadrunner-server/config/v6"
	ratelimiter "github.com/roadrunner-server/rate-limiter/v6"
	"github.com/stretchr/testify/require"
)

var _ ratelimiter.Configurer = (*config.Plugin)(nil)

func TestConfigDecoder(t *testing.T) {
	for _, tc := range []struct {
		name        string
		policy      string
		wantErr     bool
		burst       int
		retryAfter  string
		otherStatus int
	}{
		{
			name:        "defaults",
			policy:      `{rate: 1}`,
			burst:       1,
			retryAfter:  "1",
			otherStatus: http.StatusNoContent,
		},
		{
			name:        "explicit values and duration",
			policy:      `{key: header, header: X-Client-ID, rate: 2, interval: 4h, burst: 3, max_entries: 1}`,
			burst:       3,
			retryAfter:  "7200",
			otherStatus: http.StatusServiceUnavailable,
		},
		{
			name:        "string overrides",
			policy:      `{rate: "1", burst: "1", max_entries: "1"}`,
			burst:       1,
			retryAfter:  "1",
			otherStatus: http.StatusServiceUnavailable,
		},
		{name: "missing rate", policy: `{key: ip}`, wantErr: true},
		{name: "zero rate", policy: `{rate: 0}`, wantErr: true},
		{name: "zero interval", policy: `{rate: 1, interval: 0s}`, wantErr: true},
		{name: "zero burst", policy: `{rate: 1, burst: 0}`, wantErr: true},
		{name: "zero max entries", policy: `{rate: 1, max_entries: 0}`, wantErr: true},
		{name: "negative rate", policy: `{rate: -1}`, wantErr: true},
		{name: "negative interval", policy: `{rate: 1, interval: -1s}`, wantErr: true},
		{name: "negative burst", policy: `{rate: 1, burst: -1}`, wantErr: true},
		{name: "negative max entries", policy: `{rate: 1, max_entries: -1}`, wantErr: true},
		{name: "invalid key", policy: `{rate: 1, key: cookie}`, wantErr: true},
		{name: "missing header", policy: `{rate: 1, key: header}`, wantErr: true},
		{name: "unexpected header", policy: `{rate: 1, key: ip, header: X-Client-ID}`, wantErr: true},
		{name: "invalid header", policy: `{rate: 1, key: header, header: "bad name"}`, wantErr: true},
		{name: "invalid duration", policy: `{rate: 1, interval: later}`, wantErr: true},
		{name: "invalid rate type", policy: `{rate: fast}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := &config.Plugin{
					Type: "yaml",
					ReadInCfg: fmt.Appendf(nil, `version: "3"
http:
  middleware: [rate_limiter]
  rate_limiter: %s
`, tc.policy),
				}
				require.NoError(t, cfg.Init())
				plugin := &ratelimiter.Plugin{}
				err := plugin.Init(cfg)
				if tc.wantErr {
					require.Error(t, err)
					return
				}
				require.NoError(t, err)

				handler := plugin.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}))
				for i := range tc.burst + 2 {
					req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
					req.RemoteAddr = "192.0.2.1:1234"
					req.Header.Set("X-Client-ID", "client-a")
					wantStatus := http.StatusNoContent
					if i == tc.burst {
						wantStatus = http.StatusTooManyRequests
					}
					if i == tc.burst+1 {
						req.RemoteAddr = "192.0.2.2:5678"
						req.Header.Set("X-Client-ID", "client-b")
						wantStatus = tc.otherStatus
					}

					resp := httptest.NewRecorder()
					handler.ServeHTTP(resp, req)
					require.Equal(t, wantStatus, resp.Code, "request %d", i+1)
					if wantStatus == http.StatusTooManyRequests {
						require.Equal(t, tc.retryAfter, resp.Header().Get("Retry-After"))
					}
				}
			})
		})
	}
}
