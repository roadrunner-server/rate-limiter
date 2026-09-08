# rate-limiter

Process-local HTTP rate limiting for the RoadRunner bundle. The Go package is `ratelimiter`. The middleware name is `rate_limiter`.

## Configuration

Define one policy under `http.rate_limiter`. Select it through `http.middleware`:

```yaml
version: "3"

server:
  command: "php worker.php"
  relay: pipes

http:
  address: "127.0.0.1:8080"
  middleware: ["rate_limiter"]
  rate_limiter:
    key: ip
    rate: 10
    interval: 1s
    burst: 20
    max_entries: 10000
```

| Field | Default | Constraint |
| --- | --- | --- |
| `key` | `ip` | One of `global`, `ip`, or `header`. |
| `rate` | Required | Positive integer. Tokens added per `interval`. |
| `interval` | `1s` | Positive Go duration, such as `250ms`, `1s`, or `1m`. |
| `burst` | `1` | Positive integer. Bucket capacity and initial token count. |
| `max_entries` | `10000` | Positive integer. Maximum number of stored buckets. |
| `header` | Empty | A valid HTTP header name is required in `header` mode. It must be empty in other modes. |

Defaults apply before configuration decoding. An explicit zero or negative value for `rate`, `interval`, `burst`, or `max_entries` is invalid. Invalid configuration stops plugin initialization. An absent `http.rate_limiter` section disables the plugin.

In Go, `Config.Key` uses the `Key` enum: `KeyGlobal`, `KeyIP`, or `KeyHeader`. YAML uses the string values in the table.

## Policy

The plugin uses the [`golang.org/x/time/rate` token bucket](https://pkg.go.dev/golang.org/x/time/rate#Limiter). Tokens refill continuously at `rate / interval`, up to `burst`. A new bucket starts with `burst` tokens. Each allowed request uses one token. This is not a fixed-window counter: quotas do not reset at interval boundaries, and `burst` can exceed `rate`.

| Key | Identity |
| --- | --- |
| `global` | One bucket for all requests. |
| `ip` | The normalized IP from `RemoteAddr`, without its port. IPv4-mapped IPv6 addresses share the IPv4 bucket. |
| `header` | The SHA-256 hash of the trimmed header value. Values are case-sensitive. Raw values are not stored. For `Host`, the value comes from `Request.Host`. |

For header mode, set `key: header` and `header: X-Client-ID`. An arbitrary header is not an authenticated identity. A client can change the value to obtain another bucket and fill the map.

| Result | Response |
| --- | --- |
| A token is available | Call the next handler. The limiter does not change the request or response, apart from standard RoadRunner OpenTelemetry context propagation. |
| The selected header is missing or empty after trimming | `400 Bad Request`. |
| `RemoteAddr` has no valid IP in `ip` mode | `400 Bad Request`. |
| An existing bucket has no token | `429 Too Many Requests`, `Cache-Control: no-store`, and `Retry-After`. |
| A new identity arrives when the map is full | `503 Service Unavailable`. Existing identities still use their buckets. |

`Retry-After` is the delay until the next token, rounded up to whole seconds, with a minimum of `1`. It does not reserve a token. Rejected requests do not reach PHP. Rejected HEAD requests have no response body. See [RFC 6585, section 4](https://www.rfc-editor.org/rfc/rfc6585.html#section-4) and [RFC 9110, section 10.2.3](https://www.rfc-editor.org/rfc/rfc9110.html#section-10.2.3).

## State

The plugin owns one mutex-protected map. All HTTP listeners and PHP workers in the same RoadRunner process share it. Separate RoadRunner processes have independent quotas. A process restart clears quotas. A PHP worker reset does not clear them.

`max_entries` bounds the number of buckets. Cleanup runs on requests, at most once per minute. It removes only fully refilled buckets. The plugin does not evict depleted buckets to admit new identities. Idle entries can remain until another request starts cleanup. Cleanup has no background goroutine or timer.

The policy applies to every request that reaches this middleware. There are no path rules, dynamic policies, RPC controls, or distributed stores.

## Middleware Order

The limiter reads `RemoteAddr`. It does not read `Forwarded` or `X-Forwarded-For` directly. Behind a proxy, place `proxy_ip_parser` before `rate_limiter` to use the resolved client IP. The proxy must overwrite client-supplied forwarding headers. Configure the parser for the trusted proxy setup. Without this setup, IP mode limits the proxy address or can use a forged identity.

Place `headers` before the limiter when rejected responses need CORS headers. Place `http_metrics` before the limiter to count `429` responses. Place `otel` before the limiter to trace rejected requests. For example:

```yaml
http:
  middleware: ["otel", "proxy_ip_parser", "headers", "http_metrics", "rate_limiter"]
```

The HTTP access logger runs inside the configured middleware chain. It does not record requests that the limiter rejects. The internal OpenTelemetry limiter span ends before the next handler starts.
