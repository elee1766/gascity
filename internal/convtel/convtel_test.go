package convtel

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// collector is an OTLP/HTTP JSON sink that records what it receives.
type collector struct {
	mu     sync.Mutex
	logs   []map[string]any
	spans  []map[string]any
	status int
	posts  int
	// failFirst answers the first n posts with 500.
	failFirst int
}

func newCollector(t *testing.T) (*collector, *httptest.Server) {
	t.Helper()
	c := &collector{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.posts++
		if c.posts <= c.failFirst {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if c.status != http.StatusOK {
			w.WriteHeader(c.status)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("collector: bad JSON: %v", err)
			return
		}
		switch r.URL.Path {
		case "/v1/logs":
			for _, rl := range payload["resourceLogs"].([]any) {
				for _, sl := range rl.(map[string]any)["scopeLogs"].([]any) {
					for _, lr := range sl.(map[string]any)["logRecords"].([]any) {
						c.logs = append(c.logs, lr.(map[string]any))
					}
				}
			}
		case "/v1/traces":
			for _, rs := range payload["resourceSpans"].([]any) {
				for _, ss := range rs.(map[string]any)["scopeSpans"].([]any) {
					for _, sp := range ss.(map[string]any)["spans"].([]any) {
						c.spans = append(c.spans, sp.(map[string]any))
					}
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func attr(rec map[string]any, key string) string {
	attrs, _ := rec["attributes"].([]any)
	for _, a := range attrs {
		kv := a.(map[string]any)
		if kv["key"] == key {
			v := kv["value"].(map[string]any)
			for _, k := range []string{"stringValue", "intValue"} {
				if s, ok := v[k].(string); ok {
					return s
				}
			}
			if b, ok := v["boolValue"].(bool); ok && b {
				return "true"
			}
		}
	}
	return ""
}

func body(rec map[string]any) string {
	b, _ := rec["body"].(map[string]any)
	s, _ := b["stringValue"].(string)
	return s
}

func testExporter(t *testing.T, url string) *Exporter {
	t.Helper()
	e := NewExporter(Config{BaseURL: url, FlushInterval: 10 * time.Millisecond, RetryBackoff: time.Millisecond}, []Attr{strAttr("service.name", "test")})
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

// replay feeds a recorded ACP capture transcript into s.
func replay(t *testing.T, s *Session, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var rec struct {
			TS  time.Time       `json:"ts"`
			Dir string          `json:"dir"`
			Msg json.RawMessage `json:"msg"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || len(rec.Msg) == 0 {
			continue // header, torn line
		}
		dir := In
		if rec.Dir == "out" {
			dir = Out
		}
		s.ObserveAt(rec.TS, dir, rec.Msg)
	}
}

func byEvent(logs []map[string]any, event string) []map[string]any {
	var out []map[string]any
	for _, l := range logs {
		if l["eventName"] == event {
			out = append(out, l)
		}
	}
	return out
}

func TestRecordedConversationCorrelation(t *testing.T) {
	c, srv := newCollector(t)
	e := testExporter(t, srv.URL)
	s := NewSession(e, Identity{SessionID: "gc-s1", SessionName: "worker-1", ContinuationEpoch: "1", RuntimeEpoch: "1", PID: 4242, Rig: "trade", Provider: "claude"}, nil)
	replay(t, s, "testdata/conversation.jsonl")
	s.Close(5 * time.Second)
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	trace := s.TraceID()
	spanByID := map[string]map[string]any{}
	var turns, tools, agents []map[string]any
	for _, sp := range c.spans {
		if sp["traceId"] != trace {
			t.Fatalf("span %v in trace %v, want %v", sp["name"], sp["traceId"], trace)
		}
		spanByID[sp["spanId"].(string)] = sp
		switch name := sp["name"].(string); {
		case name == "gc.agent":
			agents = append(agents, sp)
		case name == "gc.turn":
			turns = append(turns, sp)
		case strings.HasPrefix(name, "gc.tool"):
			tools = append(tools, sp)
		}
	}
	if len(agents) != 1 || len(turns) != 4 || len(tools) != 1 {
		t.Fatalf("agents=%d turns=%d tools=%d, want 1/4/1", len(agents), len(turns), len(tools))
	}
	root := agents[0]["spanId"]
	for _, tr := range turns {
		if tr["parentSpanId"] != root {
			t.Fatalf("turn parent = %v, want agent %v", tr["parentSpanId"], root)
		}
	}
	tool := tools[0]
	parent := spanByID[tool["parentSpanId"].(string)]
	if parent == nil || parent["name"] != "gc.turn" || attr(parent, "gc.content.prompt") == "" || !strings.HasPrefix(attr(parent, "gc.content.prompt"), "Fix the build.") {
		t.Fatalf("tool parent = %v, want the first turn", parent)
	}
	if attr(tool, "gc.content.tool_input") != `{"command":"go test ./..."}` || !strings.Contains(attr(tool, "gc.content.tool_output"), "ok  ./...") {
		t.Fatalf("tool content = %q / %q", attr(tool, "gc.content.tool_input"), attr(tool, "gc.content.tool_output"))
	}

	statuses := map[string]int{}
	for _, tr := range turns {
		code := int(tr["status"].(map[string]any)["code"].(float64))
		statuses[attr(tr, "gc.turn.stop_reason")+"/"+map[int]string{0: "unset", 1: "ok", 2: "error"}[code]]++
	}
	if statuses["end_turn/ok"] != 2 || statuses["cancelled/ok"] != 1 || //nolint:misspell // ACP stopReason literal
		statuses["/error"] != 1 {
		t.Fatalf("turn statuses = %v", statuses)
	}

	// Streaming chunks coalesce into one record per message, joined to spans.
	msgs := byEvent(c.logs, "conversation.assistant_message")
	var texts []string
	for _, m := range msgs {
		texts = append(texts, body(m))
		if m["traceId"] != trace || spanByID[m["spanId"].(string)] == nil {
			t.Fatalf("message %q not joined to an exported span", body(m))
		}
	}
	want := []string{"I will run the tests.", "Tests pass.", "Starting the refactor", "Yes, I am back."}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("assistant messages = %q, want %q", texts, want)
	}
	thoughts := byEvent(c.logs, "conversation.reasoning")
	if len(thoughts) != 1 || body(thoughts[0]) != "Let me look." || attr(thoughts[0], "gc.reasoning.source") != "provider_exposed" {
		t.Fatalf("reasoning = %v", thoughts)
	}
	if got := byEvent(c.logs, "conversation.prompt"); len(got) != 4 {
		t.Fatalf("prompts = %d, want 4", len(got))
	}
	if got := byEvent(c.logs, "conversation.permission.request"); len(got) != 1 || got[0]["spanId"] != tool["spanId"] {
		t.Fatalf("permission request not on the tool span: %v", got)
	}
	replays := 0
	for _, m := range byEvent(c.logs, "conversation.user_message") {
		if attr(m, "gc.message.replay") == "true" {
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("replayed user messages = %d, want 1", replays)
	}
	for _, l := range c.logs {
		if attr(l, "gc.rig") != "trade" || attr(l, "gc.session.id") != "gc-s1" || attr(l, "gc.event.id") == "" {
			t.Fatalf("log %v missing attribution", l["eventName"])
		}
		if attr(l, "gen_ai.request.model") != "" {
			t.Fatalf("model attributed without a source")
		}
	}

	// A replay of the same wire stream yields the same IDs.
	c2, srv2 := newCollector(t)
	e2 := testExporter(t, srv2.URL)
	s2 := NewSession(e2, Identity{SessionID: "gc-s1", SessionName: "worker-1", ContinuationEpoch: "1", RuntimeEpoch: "1", PID: 4242, Rig: "trade", Provider: "claude"}, nil)
	replay(t, s2, "testdata/conversation.jsonl")
	s2.Close(5 * time.Second)
	_ = e2.Close(context.Background())
	ids := func(cc *collector) string {
		var out []string
		for _, l := range cc.logs {
			out = append(out, attr(l, "gc.event.id"))
		}
		return strings.Join(out, ",")
	}
	if ids(c) != ids(c2) {
		t.Fatal("event IDs differ across replays of the same stream")
	}
}

func TestRedactionBeforeExport(t *testing.T) {
	c, srv := newCollector(t)
	e := testExporter(t, srv.URL)
	envSecret := "s3cr3t-session-value-123"
	s := NewSession(e, Identity{SessionID: "s", ContinuationEpoch: "1"}, SecretEnvValues(map[string]string{"GH_TOKEN": envSecret, "GC_RIG": "trade"}))
	ghp := "ghp_" + strings.Repeat("A", 36)
	lines := []string{
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"a","prompt":[{"type":"text","text":"use token ` + envSecret + ` and run the build"}]}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"shell","kind":"execute","status":"pending","rawInput":{"command":"curl -H 'Authorization: Bearer abcdefghijklmnop1234' https://user:hunter22@example.com/x","api_key":"plainvalue123"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a","update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"GITHUB_TOKEN=` + ghp + `\nbuild ok"}}]}}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"stopReason":"end_turn","usage":{"inputTokens":5,"outputTokens":7}}}`,
	}
	for i, l := range lines {
		dir := In
		if i == 0 {
			dir = Out
		}
		s.Observe(dir, []byte(l))
	}
	s.Close(5 * time.Second)
	_ = e.Close(context.Background())

	raw := marshal(map[string]any{"logs": c.logs, "spans": c.spans})
	for _, leak := range []string{envSecret, ghp, "abcdefghijklmnop1234", "hunter22", "plainvalue123"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("exported data contains %q", leak)
		}
	}
	for _, keep := range []string{"and run the build", "curl -H", "build ok", "example.com/x", "Bearer <redacted>"} {
		if !strings.Contains(string(raw), keep) {
			t.Fatalf("exported data lost useful text %q", keep)
		}
	}
	if e.Stats().Redactions < 5 {
		t.Fatalf("redactions = %d, want >= 5", e.Stats().Redactions)
	}
	turn := byEvent(c.logs, "conversation.turn.end")
	if len(turn) != 1 || attr(turn[0], "gen_ai.usage.input_tokens") != "5" {
		t.Fatalf("token counts were redacted: %v", turn)
	}
}

func TestDisabledByDefault(t *testing.T) {
	env := map[string]string{"GC_OTEL_LOGS_URL": "http://127.0.0.1:9428", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"}
	if _, ok, err := ConfigFromEnv(func(k string) string { return env[k] }); ok || err != nil {
		t.Fatalf("enabled=%v err=%v without %s, want disabled", ok, err, EnvExport)
	}
	if NewSession(nil, Identity{}, nil) != nil {
		t.Fatal("nil exporter must give a disabled session")
	}
	var s *Session
	s.Observe(In, []byte(`{}`)) // no-op on a disabled session
	s.Close(time.Millisecond)

	env[EnvExport] = "true"
	cfg, ok, err := ConfigFromEnv(func(k string) string { return env[k] })
	if !ok || err != nil || cfg.BaseURL != DefaultURL {
		t.Fatalf("opt-in: ok=%v err=%v url=%q", ok, err, cfg.BaseURL)
	}
	env[EnvURL] = "http://collector.example.com:4318"
	if _, ok, err := ConfigFromEnv(func(k string) string { return env[k] }); ok || err == nil {
		t.Fatal("a non-loopback destination must be refused")
	}
	env[EnvURL] = ""
	env["OTEL_SDK_DISABLED"] = "true"
	if _, ok, _ := ConfigFromEnv(func(k string) string { return env[k] }); ok {
		t.Fatal("OTEL_SDK_DISABLED must win")
	}
}

func TestExporterOutageNeverBlocks(t *testing.T) {
	c, srv := newCollector(t)
	c.status = http.StatusServiceUnavailable
	e := NewExporter(Config{BaseURL: srv.URL, QueueSize: 4, SessionQueue: 4, BatchSize: 2, FlushInterval: time.Millisecond, Retries: 1, RetryBackoff: time.Millisecond}, nil)
	s := NewSession(e, Identity{SessionID: "s", ContinuationEpoch: "1"}, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			s.Observe(In, []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"plan","entries":[]}}}`))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Observe blocked while the collector was failing")
	}
	s.Close(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st := e.Stats()
	if st.Exported != 0 || st.Failed == 0 || st.Retries == 0 || st.RejectedBatch == 0 {
		t.Fatalf("stats = %+v, want failures and retries, nothing exported", st)
	}
	if st.Dropped+st.SessionDrops == 0 {
		t.Fatalf("stats = %+v, want bounded queues to drop", st)
	}
	// After Close nothing is accepted and nothing blocks.
	e.EmitLog(LogRecord{Event: "late"})
	if e.Stats().Dropped != st.Dropped+1 {
		t.Fatal("emit after close was not counted as dropped")
	}

	// Recovery: a collector that comes back receives later batches.
	c2, srv2 := newCollector(t)
	c2.failFirst = 2
	e2 := NewExporter(Config{BaseURL: srv2.URL, FlushInterval: time.Millisecond, Retries: 3, RetryBackoff: time.Millisecond}, nil)
	e2.EmitLog(LogRecord{Time: time.Now(), Event: "conversation.prompt", Body: "hi"})
	_ = e2.Close(context.Background())
	if len(c2.logs) != 1 || e2.Stats().Exported != 1 || e2.Stats().Retries != 2 {
		t.Fatalf("after recovery logs=%d stats=%+v", len(c2.logs), e2.Stats())
	}
}

func TestFieldSizeCap(t *testing.T) {
	c, srv := newCollector(t)
	e := NewExporter(Config{BaseURL: srv.URL, MaxFieldBytes: 64, FlushInterval: time.Millisecond}, nil)
	s := NewSession(e, Identity{SessionID: "s", ContinuationEpoch: "1"}, nil)
	big := strings.Repeat("é", 500) // multi-byte, so the cut must respect rune boundaries
	s.Observe(Out, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"prompt":[{"type":"text","text":"`+big+`"}]}}`))
	for i := 0; i < 100; i++ {
		s.Observe(In, []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+big+`"}}}}`))
	}
	s.Observe(In, []byte(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`))
	s.Close(5 * time.Second)
	_ = e.Close(context.Background())
	for _, ev := range []string{"conversation.prompt", "conversation.assistant_message"} {
		got := byEvent(c.logs, ev)
		if len(got) != 1 {
			t.Fatalf("%s records = %d", ev, len(got))
		}
		b := body(got[0])
		if !strings.Contains(b, "…[gc truncated ") || len(b) > 64+40 || !strings.HasPrefix(b, "é") || strings.ContainsRune(b, '\uFFFD') {
			t.Fatalf("%s body = %q (%d bytes), want a capped body with a truncation marker", ev, b, len(b))
		}
	}
	if e.Stats().Truncated < 2 {
		t.Fatalf("truncated = %d", e.Stats().Truncated)
	}
}
