# Conversation telemetry (local, opt-in)

Gas City can stream ACP session content (prompts, assistant replies, provider-exposed reasoning, tool calls and results, permission requests and lifecycle) to a local OTel collector as OTLP logs and traces. It is off by default and separate from `GC_OTEL_*` metrics/logs.

## What is exported

The bridge observes the JSON-RPC lines the ACP provider already reads and writes (`internal/runtime/acp/conn.go`). It does not read, rewrite or replace the session capture files, and it does not poll transcripts.

Traces (one trace per agent start):

| Span | Parent | Notes |
| --- | --- | --- |
| `gc.agent` | none | Lifetime of one ACP process |
| `gc.turn` | `gc.agent` | One `session/prompt` request to its response. `gc.content.prompt` holds the prompt text |
| `gc.tool.<kind>` | `gc.turn` | One `tool_call` id from announce to final status |

Logs (each carries `traceId` and `spanId` of the turn or tool it belongs to, and a stable `gc.event.id`):

`conversation.user_message`, `conversation.assistant_message`, `conversation.reasoning`, `conversation.tool_call`, `conversation.tool_result`, `conversation.permission.request`, `conversation.permission.reply`, `conversation.context`, `conversation.agent_request`, `conversation.update`, `conversation.turn.end`.

Content lives in the log body and in `gc.content.*` attributes (`gc.content.prompt`, `gc.content.tool_input`, `gc.content.tool_output`). These names are distinct from the operational `args`, `stdout` and `tool_input` attributes, so collector rules that drop those do not drop conversation content. A collector allowlist must keep `gc.content.*` explicitly.

Attribution attributes appear only when the launch env provides them: `gc.session.id`, `gc.session.name`, `gc.city`, `gc.rig`, `gc.agent`, `gc.template`, `gc.provider`, `gen_ai.request.model`, `gc.bead_id.launch`, `gc.workflow_trace`, `gc.continuation_epoch`, `gc.runtime_epoch`, `acp.session_id`, `process.pid`. Missing values are omitted, never guessed.

Reasoning: only `agent_thought_chunk` blocks the provider sends over ACP are exported, tagged `gc.reasoning.source=provider_exposed`. Hidden reasoning is not visible to Gas City and is never claimed.

## Security

- Destination must be loopback (`127.0.0.1`, `::1`, `localhost`). Other hosts and URLs with credentials are rejected and export stays disabled.
- Redaction runs before any record or span is queued: secret-named JSON keys, values of secret-named env vars in the session's launch env, and credential patterns (PEM private keys, bearer/authorization headers, JWTs, `sk-`/`ghp_`/`github_pat_`/`glpat-`/`xox?-`/AWS/Google keys, URL userinfo, `password=`-style assignments). Surrounding text is kept.
- The process environment is never exported. Credential files are never read.
- Each text field is capped at `GC_CONVERSATION_MAX_FIELD_BYTES` with an explicit `…[gc truncated N bytes]` marker.

## Delivery semantics

- Enqueue never blocks the ACP read loop or stdin writer. Queues are bounded (`GC_CONVERSATION_QUEUE`, plus a per-session wire queue). When full, items are dropped and counted.
- Batches are posted as OTLP/HTTP JSON with 2 retries and exponential backoff. A batch that still fails is abandoned and counted.
- At-least-once is not guaranteed and exactly-once is not claimed. A batch the collector stored but whose response was lost is resent, so duplicates are possible. Deduplicate on `gc.event.id` (logs) and `spanId` (spans).
- On `gc` exit the exporter flushes for up to 5 s. Nothing is persisted or replayed after a crash. The original session capture files are the record of truth.
- Export failures never fail a session or task.

Counters (`convtel.Stats`): enqueued, dropped, exported, failed, retries, session drops, truncated, redactions, rejected batches.

## Configuration

| Env | Default | Meaning |
| --- | --- | --- |
| `GC_CONVERSATION_EXPORT` | unset (off) | `true`/`1` enables content export |
| `GC_CONVERSATION_OTLP_URL` | `http://127.0.0.1:14318` | OTLP/HTTP base URL. `/v1/logs` and `/v1/traces` are appended |
| `GC_CONVERSATION_MAX_FIELD_BYTES` | `32768` | Per-field cap |
| `GC_CONVERSATION_QUEUE` | `8192` | Export queue size |

`OTEL_SDK_DISABLED=true` also disables it.

Sample opt-in, set in the environment of the process that runs the ACP provider (the city controller):

```sh
GC_CONVERSATION_EXPORT=true
GC_CONVERSATION_OTLP_URL=http://127.0.0.1:14318
GC_CONVERSATION_MAX_FIELD_BYTES=32768
```

The collector's logs and traces pipelines need an OTLP/HTTP receiver on that port and must keep `gc.content.*` attributes.

## Query examples

VictoriaLogs (LogsQL):

```
gc.signal:conversation event.name:conversation.tool_call | sort by (_time) | fields _time, gc.agent, gc.tool.title, gc.content.tool_input
gc.signal:conversation trace_id:"<trace id>" | sort by (_time)
gc.signal:conversation gc.session.name:"<session>" event.name:conversation.reasoning
```

VictoriaTraces / Tempo (TraceQL):

```
{ resource.service.name = "gascity" && name = "gc.turn" }
{ resource.gc.signal = "conversation" && span.gc.tool.status = "failed" }
```

Join a log to its trace with the log's `trace_id` field and open it in the Tempo datasource.

## Disable

Unset `GC_CONVERSATION_EXPORT` and restart the controller. Nothing else changes.
