// Package convtel exports ACP conversation content (prompts, provider-exposed
// reasoning, assistant replies, tool calls and results, lifecycle) as OTLP
// logs and traces to a local collector.
//
// It is disabled by default and independent of the ordinary GC_OTEL_*
// telemetry: content export needs its own explicit opt-in,
// GC_CONVERSATION_EXPORT=true. The destination must be a loopback address.
//
// The bridge observes the JSON-RPC lines the ACP provider already exchanges
// with the agent. It never reads or rewrites the session capture files, never
// blocks the read loop or stdin writers, and never fails a session: every
// queue is bounded and drops are counted. See docs/guides/conversation-telemetry.md.
package convtel

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvExport enables conversation content export when "true" or "1".
	EnvExport = "GC_CONVERSATION_EXPORT"
	// EnvURL is the OTLP/HTTP base URL; /v1/logs and /v1/traces are appended.
	EnvURL = "GC_CONVERSATION_OTLP_URL"
	// EnvMaxFieldBytes caps each exported text field.
	EnvMaxFieldBytes = "GC_CONVERSATION_MAX_FIELD_BYTES"
	// EnvQueue bounds the export queue (records plus spans).
	EnvQueue = "GC_CONVERSATION_QUEUE"

	// DefaultURL is the local OTel collector's OTLP/HTTP receiver.
	DefaultURL = "http://127.0.0.1:14318"
	// DefaultMaxFieldBytes is the default per-field cap.
	DefaultMaxFieldBytes = 32 * 1024
	// DefaultQueue is the default export queue size.
	DefaultQueue = 8192
)

// Config controls the exporter.
type Config struct {
	BaseURL       string
	MaxFieldBytes int
	QueueSize     int
	// SessionQueue bounds raw wire messages awaiting correlation per session.
	SessionQueue  int
	BatchSize     int
	FlushInterval time.Duration
	HTTPTimeout   time.Duration
	// Retries is how many extra attempts a failed batch gets.
	Retries      int
	RetryBackoff time.Duration
}

func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = DefaultURL
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.MaxFieldBytes <= 0 {
		c.MaxFieldBytes = DefaultMaxFieldBytes
	}
	if c.QueueSize <= 0 {
		c.QueueSize = DefaultQueue
	}
	if c.SessionQueue <= 0 {
		c.SessionQueue = 2048
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 256
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = time.Second
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = 5 * time.Second
	}
	if c.Retries < 0 {
		c.Retries = 0
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = 250 * time.Millisecond
	}
	return c
}

// ConfigFromEnv returns the exporter config and whether export is enabled.
// A non-loopback destination is an error: this exporter is for local
// collection only.
func ConfigFromEnv(getenv func(string) string) (Config, bool, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	switch strings.ToLower(strings.TrimSpace(getenv(EnvExport))) {
	case "1", "true", "yes":
	default:
		return Config{}, false, nil
	}
	if strings.EqualFold(getenv("OTEL_SDK_DISABLED"), "true") {
		return Config{}, false, nil
	}
	cfg := Config{BaseURL: getenv(EnvURL), Retries: 2}
	if v := getenv(EnvMaxFieldBytes); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, false, fmt.Errorf("%s: invalid value %q", EnvMaxFieldBytes, v)
		}
		cfg.MaxFieldBytes = n
	}
	if v := getenv(EnvQueue); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, false, fmt.Errorf("%s: invalid value %q", EnvQueue, v)
		}
		cfg.QueueSize = n
	}
	cfg = cfg.withDefaults()
	if err := requireLoopback(cfg.BaseURL); err != nil {
		return Config{}, false, err
	}
	return cfg, true, nil
}

func requireLoopback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", EnvURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: scheme must be http or https", EnvURL)
	}
	if u.User != nil {
		return fmt.Errorf("%s: credentials in URL are not allowed", EnvURL)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%s: %q is not a loopback address; conversation content is exported only to a local collector", EnvURL, host)
	}
	return nil
}
