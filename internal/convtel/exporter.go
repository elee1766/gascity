package convtel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Stats are the exporter's delivery counters.
type Stats struct {
	Enqueued      int64 // items accepted into the queue
	Dropped       int64 // items rejected because the queue was full or closed
	Exported      int64 // items in batches the collector accepted
	Failed        int64 // items in batches abandoned after all retries
	Retries       int64 // extra POST attempts
	SessionDrops  int64 // raw wire lines dropped before correlation
	Truncated     int64 // content fields cut at MaxFieldBytes
	Redactions    int64 // credential substitutions
	RejectedBatch int64 // batches the collector answered with a non-2xx status
}

type item struct {
	log  *LogRecord
	span *Span
}

// Exporter queues conversation records and spans and posts them as OTLP/HTTP
// JSON. Enqueue never blocks. Delivery is at-most-once per item from the
// queue's point of view, with bounded retries per batch: a batch the collector
// stored but whose response was lost is resent, so duplicates are possible.
// Stable span IDs and gc.event.id let a reader de-duplicate.
type Exporter struct {
	cfg      Config
	resource []Attr
	client   *http.Client

	mu     sync.Mutex
	closed bool
	queue  chan item
	done   chan struct{}
	flush  chan chan struct{}

	enqueued, dropped, exported, failed, retries  atomic.Int64
	sessionDrops, truncated, redactions, rejected atomic.Int64
}

// NewExporter starts an exporter. resource attributes are attached to every
// batch (service.name and similar).
func NewExporter(cfg Config, resource []Attr) *Exporter {
	cfg = cfg.withDefaults()
	e := &Exporter{
		cfg:      cfg,
		resource: resource,
		client:   &http.Client{Timeout: cfg.HTTPTimeout},
		queue:    make(chan item, cfg.QueueSize),
		done:     make(chan struct{}),
		flush:    make(chan chan struct{}),
	}
	go e.run()
	return e
}

// Config returns the exporter's resolved config.
func (e *Exporter) Config() Config {
	if e == nil {
		return Config{}
	}
	return e.cfg
}

// Stats returns a snapshot of the counters.
func (e *Exporter) Stats() Stats {
	if e == nil {
		return Stats{}
	}
	return Stats{
		Enqueued: e.enqueued.Load(), Dropped: e.dropped.Load(), Exported: e.exported.Load(),
		Failed: e.failed.Load(), Retries: e.retries.Load(), SessionDrops: e.sessionDrops.Load(),
		Truncated: e.truncated.Load(), Redactions: e.redactions.Load(), RejectedBatch: e.rejected.Load(),
	}
}

func (e *Exporter) enqueue(it item) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		e.dropped.Add(1)
		return
	}
	select {
	case e.queue <- it:
		e.enqueued.Add(1)
	default:
		e.dropped.Add(1)
	}
}

// EmitLog queues a log record.
func (e *Exporter) EmitLog(r LogRecord) { e.enqueue(item{log: &r}) }

// EmitSpan queues a finished span.
func (e *Exporter) EmitSpan(s Span) { e.enqueue(item{span: &s}) }

// Flush waits until everything queued before the call has been attempted, or
// ctx ends.
func (e *Exporter) Flush(ctx context.Context) error {
	if e == nil {
		return nil
	}
	ack := make(chan struct{})
	select {
	case e.flush <- ack:
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting items and waits for the queue to drain or ctx to end.
// Safe to call more than once.
func (e *Exporter) Close(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	if !e.closed {
		e.closed = true
		close(e.queue)
	}
	e.mu.Unlock()
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("conversation export: flush incomplete: %w", ctx.Err())
	}
}

func (e *Exporter) run() {
	defer close(e.done)
	ticker := time.NewTicker(e.cfg.FlushInterval)
	defer ticker.Stop()
	var batch []item
	send := func() {
		if len(batch) > 0 {
			e.send(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case it, ok := <-e.queue:
			if !ok {
				send()
				return
			}
			batch = append(batch, it)
			if len(batch) >= e.cfg.BatchSize {
				send()
			}
		case <-ticker.C:
			send()
		case ack := <-e.flush:
			// Drain what is already queued, then send.
			for n := len(e.queue); n > 0; n-- {
				it, ok := <-e.queue
				if !ok {
					break
				}
				batch = append(batch, it)
				if len(batch) >= e.cfg.BatchSize {
					send()
				}
			}
			send()
			close(ack)
		}
	}
}

func (e *Exporter) send(batch []item) {
	var logs []*LogRecord
	var spans []*Span
	for _, it := range batch {
		if it.log != nil {
			logs = append(logs, it.log)
		}
		if it.span != nil {
			spans = append(spans, it.span)
		}
	}
	if len(logs) > 0 {
		e.post("/v1/logs", encodeLogs(e.resource, logs), int64(len(logs)))
	}
	if len(spans) > 0 {
		e.post("/v1/traces", encodeSpans(e.resource, spans), int64(len(spans)))
	}
}

func (e *Exporter) post(path string, body []byte, n int64) {
	backoff := e.cfg.RetryBackoff
	for attempt := 0; ; attempt++ {
		err := e.postOnce(e.cfg.BaseURL+path, body)
		if err == nil {
			e.exported.Add(n)
			return
		}
		if attempt >= e.cfg.Retries {
			e.failed.Add(n)
			return
		}
		e.retries.Add(1)
		time.Sleep(backoff)
		backoff *= 2
	}
}

func (e *Exporter) postOnce(url string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		e.rejected.Add(1)
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// OTLP/JSON encoding (opentelemetry-proto JSON mapping).

type otlpAnyValue struct {
	StringValue *string `json:"stringValue,omitempty"`
	IntValue    *string `json:"intValue,omitempty"`
	BoolValue   *bool   `json:"boolValue,omitempty"`
}

type otlpKV struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

func encodeAttrs(attrs []Attr) []otlpKV {
	out := make([]otlpKV, 0, len(attrs))
	for _, a := range attrs {
		kv := otlpKV{Key: a.Key}
		switch {
		case a.IsInt:
			s := strconv.FormatInt(a.Int, 10)
			kv.Value.IntValue = &s
		case a.IsBool:
			b := a.Bool
			kv.Value.BoolValue = &b
		default:
			s := a.Str
			kv.Value.StringValue = &s
		}
		out = append(out, kv)
	}
	return out
}

type otlpResource struct {
	Attributes []otlpKV `json:"attributes"`
}

type otlpScope struct {
	Name string `json:"name"`
}

const scopeName = "github.com/gastownhall/gascity/internal/convtel"

func unixNano(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

type otlpLog struct {
	TimeUnixNano         string       `json:"timeUnixNano"`
	ObservedTimeUnixNano string       `json:"observedTimeUnixNano"`
	SeverityNumber       int          `json:"severityNumber"`
	SeverityText         string       `json:"severityText"`
	Body                 otlpAnyValue `json:"body"`
	EventName            string       `json:"eventName,omitempty"`
	Attributes           []otlpKV     `json:"attributes"`
	TraceID              string       `json:"traceId,omitempty"`
	SpanID               string       `json:"spanId,omitempty"`
}

func severityText(n int) string {
	switch {
	case n >= sevError:
		return "ERROR"
	case n >= sevWarn:
		return "WARN"
	default:
		return "INFO"
	}
}

func encodeLogs(resource []Attr, recs []*LogRecord) []byte {
	logs := make([]otlpLog, 0, len(recs))
	now := unixNano(time.Now())
	for _, r := range recs {
		body := r.Body
		sev := r.Severity
		if sev == 0 {
			sev = sevInfo
		}
		attrs := append([]Attr{strAttr("event.name", r.Event)}, r.Attrs...)
		logs = append(logs, otlpLog{
			TimeUnixNano:         unixNano(r.Time),
			ObservedTimeUnixNano: now,
			SeverityNumber:       sev,
			SeverityText:         severityText(sev),
			Body:                 otlpAnyValue{StringValue: &body},
			EventName:            r.Event,
			Attributes:           encodeAttrs(attrs),
			TraceID:              r.TraceID,
			SpanID:               r.SpanID,
		})
	}
	payload := map[string]any{
		"resourceLogs": []any{map[string]any{
			"resource":  otlpResource{Attributes: encodeAttrs(resource)},
			"scopeLogs": []any{map[string]any{"scope": otlpScope{Name: scopeName}, "logRecords": logs}},
		}},
	}
	return marshal(payload)
}

// marshal encodes without HTML escaping so content stays readable in queries.
func marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

type otlpStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

type otlpSpan struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []otlpKV   `json:"attributes"`
	Status            otlpStatus `json:"status"`
}

func encodeSpans(resource []Attr, spans []*Span) []byte {
	out := make([]otlpSpan, 0, len(spans))
	for _, s := range spans {
		end := s.End
		if end.Before(s.Start) {
			end = s.Start
		}
		out = append(out, otlpSpan{
			TraceID: s.TraceID, SpanID: s.SpanID, ParentSpanID: s.ParentSpanID, Name: s.Name,
			Kind:              1, // SPAN_KIND_INTERNAL
			StartTimeUnixNano: unixNano(s.Start), EndTimeUnixNano: unixNano(end),
			Attributes: encodeAttrs(s.Attrs),
			Status:     otlpStatus{Code: s.StatusCode, Message: s.StatusMsg},
		})
	}
	payload := map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource":   otlpResource{Attributes: encodeAttrs(resource)},
			"scopeSpans": []any{map[string]any{"scope": otlpScope{Name: scopeName}, "spans": out}},
		}},
	}
	return marshal(payload)
}
