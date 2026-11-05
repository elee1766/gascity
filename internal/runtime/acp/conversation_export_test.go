//go:build integration

package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/convtel"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestACPConversationExportEndToEnd runs fakeacp through the real provider
// with conversation export on, and checks that the prompt, reasoning, tool
// call and reply reach an OTLP sink joined by trace ID, with the launch env's
// credential redacted.
func TestACPConversationExportEndToEnd(t *testing.T) {
	var fixture acpConformanceFixture
	if err := prepareACPConformanceFixture(t, &fixture); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		// GC_TEST_CONVTEL_LIVE_URL forwards to a real local collector for a
		// manual end-to-end check; unset in CI.
		if live := os.Getenv("GC_TEST_CONVTEL_LIVE_URL"); live != "" {
			if resp, err := http.Post(live+r.URL.Path, "application/json", strings.NewReader(string(b))); err == nil {
				_ = resp.Body.Close()
				t.Logf("forwarded %s: %s", r.URL.Path, resp.Status)
			}
		}
		mu.Lock()
		bodies = append(bodies, r.URL.Path+" "+string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	exp := convtel.NewExporter(convtel.Config{BaseURL: srv.URL, FlushInterval: 10 * time.Millisecond}, nil)

	p := NewProviderWithDir(fixture.dir, Config{
		HandshakeTimeout: protocolWait, NudgeBusyTimeout: protocolWait, OutputBufferLines: 100,
		ConversationExporter: exp,
	})
	const secret = "e2e-secret-token-value-42"
	name := fmt.Sprintf("gc-acp-conv-%d-%d", os.Getpid(), time.Now().UnixNano())
	command := "exec " + shellQuote(fixture.command) + " --log-dir " + shellQuote(t.TempDir()) +
		" --thought " + shellQuote("thinking aloud") + " --tool-call"
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: command, WorkDir: t.TempDir(),
		Env: map[string]string{"GC_SESSION_ID": "gc-conv-e2e", "GC_CONTINUATION_EPOCH": "1", "GC_RIG": "gascity", "GH_TOKEN": secret},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Nudge(name, runtime.TextContent("say hi; my token is "+secret)); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	p.mu.Lock()
	sc := p.conns[name]
	p.mu.Unlock()
	if !sc.waitIdle(protocolWait) {
		t.Fatal("turn did not finish")
	}
	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	<-sc.done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exp.Close(ctx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	all := strings.Join(bodies, "\n")
	mu.Unlock()
	if strings.Contains(all, secret) {
		t.Fatal("launch-env credential reached the exporter")
	}
	for _, want := range []string{
		"conversation.prompt", "say hi; my token is <redacted>", "conversation.reasoning", "thinking aloud",
		"conversation.tool_call", "fixture contents", "conversation.assistant_message", `"gc.turn"`, `"gc.agent"`,
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("export lacks %q:\n%s", want, all)
		}
	}
	var traces []string
	for _, b := range bodies {
		_, js, _ := strings.Cut(b, " ")
		var v map[string]any
		_ = json.Unmarshal([]byte(js), &v)
		for _, k := range []string{"traceId"} {
			traces = append(traces, collectStrings(v, k)...)
		}
	}
	first := ""
	for _, tr := range traces {
		if first == "" {
			first = tr
		}
		if tr != first {
			t.Fatalf("records span traces %q and %q, want one session trace", first, tr)
		}
	}
	if first == "" {
		t.Fatal("no trace IDs exported")
	}
	t.Logf("trace %s", first)
	if testing.Verbose() {
		for _, b := range bodies {
			t.Logf("OTLP %s", b)
		}
	}
}

func collectStrings(v any, key string) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			if s, ok := c.(string); ok && k == key {
				out = append(out, s)
			} else {
				out = append(out, collectStrings(c, key)...)
			}
		}
	case []any:
		for _, c := range t {
			out = append(out, collectStrings(c, key)...)
		}
	}
	return out
}
