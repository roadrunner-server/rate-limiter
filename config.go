package ratelimiter

import (
	"net/http"
	"time"

	"github.com/roadrunner-server/errors"
	"golang.org/x/net/http/httpguts"
)

type Key string

const (
	KeyGlobal Key = "global"
	KeyIP     Key = "ip"
	KeyHeader Key = "header"
)

type Config struct {
	Key        Key           `mapstructure:"key"`
	Header     string        `mapstructure:"header"`
	Rate       int           `mapstructure:"rate"`
	Interval   time.Duration `mapstructure:"interval"`
	Burst      int           `mapstructure:"burst"`
	MaxEntries int           `mapstructure:"max_entries"`
}

func (c *Config) validate() error {
	if c.Rate <= 0 {
		return errors.Str("rate must be greater than zero")
	}
	if c.Interval <= 0 {
		return errors.Str("interval must be greater than zero")
	}
	if c.Burst <= 0 {
		return errors.Str("burst must be greater than zero")
	}
	if c.MaxEntries <= 0 {
		return errors.Str("max_entries must be greater than zero")
	}

	switch c.Key {
	case KeyGlobal, KeyIP:
		if c.Header != "" {
			return errors.Str("header requires key: header")
		}
	case KeyHeader:
		if !httpguts.ValidHeaderFieldName(c.Header) {
			return errors.Str("header must be a valid HTTP header name")
		}
		c.Header = http.CanonicalHeaderKey(c.Header)
	default:
		return errors.Str("key must be global, ip, or header")
	}

	return nil
}
