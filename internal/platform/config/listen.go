package config

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// ErrAPIKeyRequired is returned when HTTP is bound beyond loopback without a key.
var ErrAPIKeyRequired = errors.New("CRAWLD_API_KEY or HTTP_API_KEY is required when HTTP_ADDR is not loopback-only")

// ValidateHTTP enforces that a publicly bound crawld must authenticate callers.
func ValidateHTTP(cfg HTTPConfig) error {
	if cfg.APIKey != "" {
		return nil
	}
	if IsLoopbackBind(cfg.Addr) {
		return nil
	}
	return fmt.Errorf("%w (HTTP_ADDR=%q)", ErrAPIKeyRequired, cfg.Addr)
}

// IsLoopbackBind reports whether addr listens only on loopback interfaces.
func IsLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// ":8080" and similar listen on all interfaces.
		return false
	}
	host = strings.Trim(host, "[]")
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
