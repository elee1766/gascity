package convtel

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Direction is the side a wire line traveled.
type Direction int

const (
	// Out is a line gc wrote to the agent's stdin.
	Out Direction = iota
	// In is a line gc read from the agent's stdout.
	In
)

// Identity is the attribution known for one agent start. Empty fields are
// omitted from the export rather than guessed.
type Identity struct {
	SessionID         string // GC_SESSION_ID
	SessionName       string
	ContinuationEpoch string
	RuntimeEpoch      string
	PID               int
	City              string
	Rig               string
	Agent             string // GC_ALIAS, else GC_AGENT
	Template          string
	Provider          string // GC_PROVIDER
	Model             string // GC_MODEL
	BeadID            string // GC_BEAD_ID as set at launch
	WorkflowTrace     string // GC_WORKFLOW_TRACE
}

// IdentityFromEnv reads attribution from a session's launch environment.
func IdentityFromEnv(name string, env map[string]string, pid int) Identity {
	agent := env["GC_ALIAS"]
	if agent == "" {
		agent = env["GC_AGENT"]
	}
	return Identity{
		SessionID: env["GC_SESSION_ID"], SessionName: name,
		ContinuationEpoch: env["GC_CONTINUATION_EPOCH"], RuntimeEpoch: env["GC_RUNTIME_EPOCH"], PID: pid,
		City: env["GC_CITY"], Rig: env["GC_RIG"], Agent: agent, Template: env["GC_TEMPLATE"],
		Provider: env["GC_PROVIDER"], Model: env["GC_MODEL"], BeadID: env["GC_BEAD_ID"],
		WorkflowTrace: env["GC_WORKFLOW_TRACE"],
	}
}

// SecretEnvValues returns the values of env entries whose names say they hold
// credentials, for exact-match redaction. Only the session's launch env is
// consulted, never the process env.
func SecretEnvValues(env map[string]string) []string {
	var out []string
	for k, v := range env {
		if isSecretKey(k) || isSecretKey(strings.ReplaceAll(k, "_", "")) {
			out = append(out, v)
		}
	}
	return out
}

type wireLine struct {
	ts  time.Time
	dir Direction
	raw []byte
}

// Session correlates one agent start's JSON-RPC traffic into log records and
// spans. Observe never blocks; lines beyond the bounded queue are dropped and
// counted. A nil *Session is disabled.
type Session struct {
	exp      *Exporter
	id       Identity
	redactor *Redactor
	maxField int

	mu      sync.Mutex
	closed  bool
	queue   chan wireLine
	dropped int64
	done    chan struct{}

	// Correlation state, owned by the run goroutine.
	traceID    string
	rootSpan   string
	start      time.Time
	last       time.Time
	replay     bool
	seq        int64
	acpSession string
	turns      map[string]*turnState // by prompt request id
	current    *turnState
	tools      map[string]*toolState
	perms      map[string]permState
	msg        *msgState
	turnCount  int64
}

type turnState struct {
	reqID  string
	spanID string
	start  time.Time
	prompt string
	tools  int
}

type toolState struct {
	id       string
	spanID   string
	parent   string
	start    time.Time
	title    string
	kind     string
	status   string
	input    json.RawMessage
	output   []string
	rawOut   json.RawMessage
	location string
}

type permState struct {
	toolCallID string
	title      string
}

type msgState struct {
	kind      string // agent_message_chunk, agent_thought_chunk, user_message_chunk
	messageID string
	start     time.Time
	text      strings.Builder
	chunks    int
	outside   bool // started with no turn open
}

// NewSession starts correlating one agent start. It returns nil when exp is nil.
func NewSession(exp *Exporter, id Identity, secrets []string) *Session {
	if exp == nil {
		return nil
	}
	cfg := exp.Config()
	s := &Session{
		exp:      exp,
		id:       id,
		redactor: NewRedactor(secrets),
		maxField: cfg.MaxFieldBytes,
		queue:    make(chan wireLine, cfg.SessionQueue),
		done:     make(chan struct{}),
		turns:    map[string]*turnState{},
		tools:    map[string]*toolState{},
		perms:    map[string]permState{},
		start:    time.Now(),
	}
	key := id.SessionID
	if key == "" {
		key = id.SessionName
	}
	s.traceID = traceID("gc.session", key, id.ContinuationEpoch)
	s.rootSpan = spanID(s.traceID, "agent", id.RuntimeEpoch, strconv.Itoa(id.PID))
	go s.run()
	return s
}

// TraceID returns the trace that carries this session's spans.
func (s *Session) TraceID() string {
	if s == nil {
		return ""
	}
	return s.traceID
}

// Observe queues one complete JSON-RPC line. raw is owned by the session.
func (s *Session) Observe(dir Direction, raw []byte) {
	s.ObserveAt(time.Now(), dir, raw)
}

// ObserveAt is Observe with an explicit timestamp, for replaying recorded wire
// traffic.
func (s *Session) ObserveAt(ts time.Time, dir Direction, raw []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.queue <- wireLine{ts: ts, dir: dir, raw: raw}:
	default:
		s.dropped++
		s.exp.sessionDrops.Add(1)
	}
}

// Close finishes open turns and tools, emits the agent span, and waits up to
// timeout for correlation to finish. It does not wait for network delivery.
func (s *Session) Close(timeout time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-s.done:
	case <-t.C:
	}
}

func (s *Session) run() {
	defer close(s.done)
	started := false
	begin := func() {
		if !started {
			started = true
			s.emitLog(s.start, sevInfo, "conversation.session.start", "", s.rootSpan, nil)
		}
	}
	for l := range s.queue {
		if !started && l.ts.Before(s.start.Add(-time.Minute)) {
			s.start, s.replay = l.ts, true
		}
		begin()
		s.last = l.ts
		s.handle(l)
	}
	begin()
	end := time.Now()
	if s.replay || s.last.After(end) {
		end = s.last
	}
	s.finishOpen(end, "agent exited")
	s.mu.Lock()
	dropped := s.dropped
	s.mu.Unlock()
	attrs := []Attr{intAttr("gc.turns", s.turnCount)}
	if dropped > 0 {
		attrs = append(attrs, intAttr("gc.dropped_lines", dropped))
	}
	s.emitLog(end, sevInfo, "conversation.session.end", "", s.rootSpan, attrs)
	s.exp.EmitSpan(Span{
		TraceID: s.traceID, SpanID: s.rootSpan, Name: "gc.agent",
		Start: s.start, End: end, Attrs: append(s.baseAttrs(), attrs...), StatusCode: 1,
	})
}

type rpc struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func idKey(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return "s:" + str
	}
	return "n:" + string(raw)
}

func (s *Session) handle(l wireLine) {
	var m rpc
	if err := json.Unmarshal(l.raw, &m); err != nil {
		return
	}
	id := idKey(m.ID)
	switch {
	case l.dir == Out && m.Method == "session/prompt" && id != "":
		s.startTurn(l.ts, id, m.Params)
	case l.dir == Out && m.Method == "session/new":
		s.contextSnapshot(l.ts, "session/new", m.Params)
	case l.dir == Out && m.Method == "session/cancel":
		s.emitLog(l.ts, sevInfo, "conversation.turn.cancel_requested", "", s.turnSpan(), nil)
	case l.dir == Out && m.Method == "" && id != "":
		// gc's reply to an agent request (permission, fs).
		if p, ok := s.perms[id]; ok {
			delete(s.perms, id)
			body := s.json(m.Result)
			s.emitLog(l.ts, sevInfo, "conversation.permission.reply", body, s.toolSpan(p.toolCallID),
				appendStr(nil, "gc.tool.call_id", p.toolCallID))
		}
	case l.dir == In && m.Method == "session/update":
		s.update(l.ts, m.Params)
	case l.dir == In && m.Method == "session/request_permission" && id != "":
		s.permission(l.ts, id, m.Params)
	case l.dir == In && m.Method != "" && id != "":
		body := s.json(m.Params)
		s.emitLog(l.ts, sevInfo, "conversation.agent_request", body, s.turnSpan(), []Attr{strAttr("rpc.method", m.Method)})
	case l.dir == In && m.Method == "" && id != "":
		if t, ok := s.turns[id]; ok {
			s.endTurn(l.ts, t, m.Result, m.Error)
		} else if len(m.Result) > 0 {
			var r struct {
				SessionID string `json:"sessionId"`
				Models    *struct {
					Current string `json:"currentModelId"`
				} `json:"models"`
			}
			if json.Unmarshal(m.Result, &r) == nil && r.SessionID != "" {
				s.acpSession = r.SessionID
				if r.Models != nil && r.Models.Current != "" && s.id.Model == "" {
					s.id.Model = r.Models.Current
				}
				s.contextSnapshot(l.ts, "session/new.result", m.Result)
			}
		}
	}
}

func (s *Session) startTurn(ts time.Time, id string, params json.RawMessage) {
	s.flushMsg(ts)
	var p struct {
		SessionID string            `json:"sessionId"`
		Prompt    []json.RawMessage `json:"prompt"`
	}
	_ = json.Unmarshal(params, &p)
	if p.SessionID != "" {
		s.acpSession = p.SessionID
	}
	text := promptText(p.Prompt)
	s.turnCount++
	t := &turnState{reqID: id, start: ts, spanID: spanID(s.traceID, s.rootSpan, "turn", id, strconv.FormatInt(s.turnCount, 10))}
	t.prompt, _ = s.text(text)
	s.turns[id] = t
	s.current = t
	s.emitLog(ts, sevInfo, "conversation.prompt", t.prompt, t.spanID, []Attr{intAttr("gc.prompt.blocks", int64(len(p.Prompt)))})
}

// promptText renders prompt blocks: text verbatim, other blocks as compact JSON.
func promptText(blocks []json.RawMessage) string {
	var parts []string
	for _, b := range blocks {
		var tb struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(b, &tb) == nil && tb.Type == "text" {
			parts = append(parts, tb.Text)
			continue
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "\n")
}

func (s *Session) endTurn(ts time.Time, t *turnState, result json.RawMessage, rpcErr *struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
},
) {
	s.flushMsg(ts)
	delete(s.turns, t.reqID)
	var r struct {
		StopReason string `json:"stopReason"`
		Usage      *struct {
			Input   int64 `json:"inputTokens"`
			Output  int64 `json:"outputTokens"`
			Thought int64 `json:"thoughtTokens"`
			Cached  int64 `json:"cachedReadTokens"`
			Total   int64 `json:"totalTokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(result, &r)
	attrs := []Attr{intAttr("gc.turn.tools", int64(t.tools))}
	attrs = appendStr(attrs, "gc.turn.stop_reason", r.StopReason)
	if r.Usage != nil {
		attrs = append(attrs, intAttr("gen_ai.usage.input_tokens", r.Usage.Input), intAttr("gen_ai.usage.output_tokens", r.Usage.Output))
		if r.Usage.Thought > 0 {
			attrs = append(attrs, intAttr("gc.usage.thought_tokens", r.Usage.Thought))
		}
		if r.Usage.Cached > 0 {
			attrs = append(attrs, intAttr("gc.usage.cached_read_tokens", r.Usage.Cached))
		}
	}
	status, msg, sev := 1, "", sevInfo
	if rpcErr != nil {
		status, sev = 2, sevError
		msg, _ = s.text(rpcErr.Message)
		attrs = append(attrs, intAttr("rpc.error_code", int64(rpcErr.Code)))
	}
	s.finishToolsOf(ts, t.spanID, "turn ended")
	s.emitLog(ts, sev, "conversation.turn.end", msg, t.spanID, attrs)
	spanAttrs := append(s.baseAttrs(), attrs...)
	spanAttrs = append(spanAttrs, strAttr("gc.content.prompt", t.prompt))
	s.exp.EmitSpan(Span{
		TraceID: s.traceID, SpanID: t.spanID, ParentSpanID: s.rootSpan, Name: "gc.turn",
		Start: t.start, End: ts, Attrs: spanAttrs, StatusCode: status, StatusMsg: msg,
	})
	if s.current == t {
		s.current = nil
	}
}

func (s *Session) turnSpan() string {
	if s.current != nil {
		return s.current.spanID
	}
	return s.rootSpan
}

func (s *Session) toolSpan(callID string) string {
	if t, ok := s.tools[callID]; ok {
		return t.spanID
	}
	return s.turnSpan()
}

type updateParams struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		Type       string          `json:"sessionUpdate"`
		MessageID  string          `json:"messageId"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		Kind       string          `json:"kind"`
		Status     string          `json:"status"`
		RawInput   json.RawMessage `json:"rawInput"`
		RawOutput  json.RawMessage `json:"rawOutput"`
		Locations  json.RawMessage `json:"locations"`
	} `json:"update"`
}

func (s *Session) update(ts time.Time, params json.RawMessage) {
	var p updateParams
	if json.Unmarshal(params, &p) != nil {
		return
	}
	u := p.Update
	switch u.Type {
	case "agent_message_chunk", "agent_thought_chunk", "user_message_chunk":
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		text := ""
		if json.Unmarshal(u.Content, &block) == nil && block.Type == "text" {
			text = block.Text
		} else if len(u.Content) > 0 {
			text = string(u.Content)
		}
		if s.msg != nil && (s.msg.kind != u.Type || (u.MessageID != "" && s.msg.messageID != "" && u.MessageID != s.msg.messageID)) {
			s.flushMsg(ts)
		}
		if s.msg == nil {
			s.msg = &msgState{kind: u.Type, messageID: u.MessageID, start: ts, outside: s.current == nil}
		}
		if s.msg.text.Len() <= s.maxField*2 {
			s.msg.text.WriteString(text)
		}
		s.msg.chunks++
	case "tool_call", "tool_call_update":
		s.flushMsg(ts)
		s.tool(ts, u.Type == "tool_call", p)
	default:
		// plan, usage_update, available_commands_update, current_mode_update and
		// future variants: exported as provider-reported context snapshots.
		// They do not end a streaming message.
		body := s.json(params)
		s.emitLog(ts, sevInfo, "conversation.update", body, s.turnSpan(), []Attr{strAttr("acp.update", u.Type)})
	}
}

var msgEvents = map[string]string{
	"agent_message_chunk": "conversation.assistant_message",
	"agent_thought_chunk": "conversation.reasoning",
	"user_message_chunk":  "conversation.user_message",
}

func (s *Session) flushMsg(_ time.Time) {
	m := s.msg
	if m == nil {
		return
	}
	s.msg = nil
	body, _ := s.text(m.text.String())
	attrs := []Attr{intAttr("gc.message.chunks", int64(m.chunks))}
	attrs = appendStr(attrs, "acp.message_id", m.messageID)
	if m.kind == "agent_thought_chunk" {
		// Only reasoning the provider chose to stream over ACP is visible.
		attrs = append(attrs, strAttr("gc.reasoning.source", "provider_exposed"))
	}
	if m.kind == "user_message_chunk" && m.outside {
		attrs = append(attrs, boolAttr("gc.message.replay", true))
	}
	s.emitLog(m.start, sevInfo, msgEvents[m.kind], body, s.turnSpan(), attrs)
}

func (s *Session) tool(ts time.Time, announce bool, p updateParams) {
	u := p.Update
	if u.ToolCallID == "" {
		return
	}
	t, ok := s.tools[u.ToolCallID]
	if !ok {
		parent := s.turnSpan()
		t = &toolState{id: u.ToolCallID, start: ts, parent: parent, spanID: spanID(s.traceID, parent, "tool", u.ToolCallID)}
		s.tools[u.ToolCallID] = t
		if s.current != nil {
			s.current.tools++
		}
	}
	if u.Title != "" {
		t.title = u.Title
	}
	if u.Kind != "" {
		t.kind = u.Kind
	}
	if u.Status != "" {
		t.status = u.Status
	}
	if len(u.RawInput) > 0 && string(u.RawInput) != "{}" {
		t.input = u.RawInput
	}
	if len(u.RawOutput) > 0 && string(u.RawOutput) != "{}" {
		t.rawOut = u.RawOutput
	}
	if len(u.Locations) > 0 {
		t.location = string(u.Locations)
	}
	if len(u.Content) > 0 {
		t.output = append(t.output, toolContentText(u.Content))
	}
	if announce || !ok {
		input := s.json(t.input)
		s.emitLog(ts, sevInfo, "conversation.tool_call", input, t.spanID, s.toolAttrs(t))
	}
	if t.status == "completed" || t.status == "failed" {
		s.finishTool(ts, t, "")
	}
}

func toolContentText(raw json.RawMessage) string {
	var parts []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Path    string          `json:"path"`
		OldText *string         `json:"oldText"`
		NewText string          `json:"newText"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	var out []string
	for _, p := range parts {
		switch p.Type {
		case "content":
			var b struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(p.Content, &b) == nil && b.Type == "text" {
				out = append(out, b.Text)
			} else {
				out = append(out, string(p.Content))
			}
		case "diff":
			out = append(out, "diff "+p.Path+"\n"+p.NewText)
		default:
			b, _ := json.Marshal(p)
			out = append(out, string(b))
		}
	}
	return strings.Join(out, "\n")
}

func (s *Session) toolAttrs(t *toolState) []Attr {
	var a []Attr
	a = appendStr(a, "gc.tool.call_id", t.id)
	a = appendStr(a, "gc.tool.title", t.title)
	a = appendStr(a, "gc.tool.kind", t.kind)
	a = appendStr(a, "gc.tool.status", t.status)
	return a
}

func (s *Session) finishTool(ts time.Time, t *toolState, incomplete string) {
	delete(s.tools, t.id)
	output := strings.Join(t.output, "\n")
	if len(t.rawOut) > 0 {
		if output != "" {
			output += "\n"
		}
		output += string(t.rawOut)
	}
	out, _ := s.text(output)
	in := s.json(t.input)
	attrs := s.toolAttrs(t)
	status, sev := 1, sevInfo
	if t.status == "failed" {
		status, sev = 2, sevWarn
	}
	if incomplete != "" {
		status = 0
		attrs = append(attrs, strAttr("gc.tool.incomplete", incomplete))
	}
	s.emitLog(ts, sev, "conversation.tool_result", out, t.spanID, attrs)
	spanAttrs := append(s.baseAttrs(), attrs...)
	spanAttrs = appendStr(spanAttrs, "gc.content.tool_input", in)
	spanAttrs = appendStr(spanAttrs, "gc.content.tool_output", out)
	if t.location != "" {
		loc, _ := s.text(t.location)
		spanAttrs = append(spanAttrs, strAttr("gc.tool.locations", loc))
	}
	name := "gc.tool"
	if t.kind != "" {
		name += " " + t.kind
	}
	s.exp.EmitSpan(Span{
		TraceID: s.traceID, SpanID: t.spanID, ParentSpanID: t.parent, Name: name,
		Start: t.start, End: ts, Attrs: spanAttrs, StatusCode: status,
	})
}

func (s *Session) finishToolsOf(ts time.Time, parent, why string) {
	for _, t := range s.tools {
		if t.parent == parent {
			s.finishTool(ts, t, why)
		}
	}
}

func (s *Session) finishOpen(ts time.Time, why string) {
	s.flushMsg(ts)
	for _, t := range s.turns {
		s.finishToolsOf(ts, t.spanID, why)
		s.emitLog(ts, sevWarn, "conversation.turn.end", why, t.spanID, []Attr{boolAttr("gc.turn.incomplete", true)})
		attrs := append(s.baseAttrs(), boolAttr("gc.turn.incomplete", true), strAttr("gc.content.prompt", t.prompt))
		s.exp.EmitSpan(Span{
			TraceID: s.traceID, SpanID: t.spanID, ParentSpanID: s.rootSpan, Name: "gc.turn",
			Start: t.start, End: ts, Attrs: attrs, StatusMsg: why,
		})
	}
	s.turns = map[string]*turnState{}
	s.current = nil
	for _, t := range s.tools {
		s.finishTool(ts, t, why)
	}
}

func (s *Session) permission(ts time.Time, id string, params json.RawMessage) {
	var p struct {
		ToolCall struct {
			ID    string `json:"toolCallId"`
			Title string `json:"title"`
		} `json:"toolCall"`
	}
	_ = json.Unmarshal(params, &p)
	s.perms[id] = permState{toolCallID: p.ToolCall.ID, title: p.ToolCall.Title}
	body := s.json(params)
	s.emitLog(ts, sevInfo, "conversation.permission.request", body, s.toolSpan(p.ToolCall.ID),
		appendStr(nil, "gc.tool.call_id", p.ToolCall.ID))
}

func (s *Session) contextSnapshot(ts time.Time, what string, raw json.RawMessage) {
	body := s.json(raw)
	s.emitLog(ts, sevInfo, "conversation.context", body, s.rootSpan, []Attr{strAttr("gc.context.source", what)})
}

// text redacts then caps s.
func (s *Session) text(v string) (string, bool) {
	r, n := s.redactor.Text(v)
	if n > 0 {
		s.exp.redactions.Add(int64(n))
	}
	out, cut := Truncate(r, s.maxField)
	if cut {
		s.exp.truncated.Add(1)
	}
	return out, cut
}

func (s *Session) json(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	r, n := s.redactor.JSON(raw)
	if n > 0 {
		s.exp.redactions.Add(int64(n))
	}
	out, cut := Truncate(r, s.maxField)
	if cut {
		s.exp.truncated.Add(1)
	}
	return out
}

func (s *Session) baseAttrs() []Attr {
	id := s.id
	var a []Attr
	a = appendStr(a, "gc.session.id", id.SessionID)
	a = appendStr(a, "gc.session.name", id.SessionName)
	a = appendStr(a, "gc.continuation_epoch", id.ContinuationEpoch)
	a = appendStr(a, "gc.runtime_epoch", id.RuntimeEpoch)
	if id.PID > 0 {
		a = append(a, intAttr("process.pid", int64(id.PID)))
	}
	a = appendStr(a, "gc.city", id.City)
	a = appendStr(a, "gc.rig", id.Rig)
	a = appendStr(a, "gc.agent", id.Agent)
	a = appendStr(a, "gc.template", id.Template)
	a = appendStr(a, "gc.provider", id.Provider)
	a = appendStr(a, "gen_ai.request.model", id.Model)
	a = appendStr(a, "gc.bead_id.launch", id.BeadID)
	a = appendStr(a, "gc.workflow_trace", id.WorkflowTrace)
	a = appendStr(a, "acp.session_id", s.acpSession)
	return a
}

func (s *Session) emitLog(ts time.Time, sev int, event, body, span string, attrs []Attr) {
	s.seq++
	all := append(s.baseAttrs(), strAttr("gc.event.id", stableID(12, s.traceID, s.rootSpan, strconv.FormatInt(s.seq, 10), event)),
		intAttr("gc.event.seq", s.seq))
	if s.current != nil {
		all = append(all, intAttr("gc.turn.index", s.turnCount))
	}
	all = append(all, attrs...)
	s.exp.EmitLog(LogRecord{Time: ts, Severity: sev, Event: event, Body: body, TraceID: s.traceID, SpanID: span, Attrs: all})
}
