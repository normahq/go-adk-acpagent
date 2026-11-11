# Troubleshooting

## ACP Process Fails To Start

Check `Config.Command` and `Config.WorkingDir`. The command is executed exactly
as an argv array; shell parsing is not applied.

For local debugging, route provider stderr to the terminal:

```go
Stderr: os.Stderr,
```

For production services, route stderr to your application log pipeline or use
`io.Discard` if provider stderr is too noisy.

## JSON-RPC Stream Looks Corrupted

ACP agents must write protocol messages to stdout and logs to stderr. If a
provider writes logs to stdout, it can corrupt the JSON-RPC stream. Use the
provider's quiet/stdio mode or wrap it with a script that keeps logs on stderr.

## Session Config Selection Fails

`Config.SessionConfig` uses ACP `session/set_config_option`, not legacy
provider-specific setters.

If session config selection fails:

- confirm the ACP agent returns matching config options from `session/new` or
  `session/resume`
- use the exact ACP config option ID exposed by the agent
- inspect logs with `Config.Logger`
- forward stderr while debugging provider startup and capability negotiation

## Legacy Mode Selection Fails

Mode should normally be configured through `Config.SessionConfig`. For legacy
ACP agents that expose only `session/set_mode`, use
`SessionConfigValue{ID: "mode", Value: "<mode-id>"}` and confirm the agent
returns legacy mode state from `session/new` or `session/resume`.

## Permission Requests Do Nothing

Provide `Config.PermissionHandler`. Without a handler, permission requests are
rejected by the adapter.

```go
PermissionHandler: func(ctx context.Context, req acpagent.PermissionRequest) (acpagent.PermissionDecision, error) {
	return acpagent.PermissionDecision{Canceled: true}, nil
},
```

Applications with an interactive UI should present `req.Options` to the user
and return the selected outcome.

## Provider Errors

ACP provider failures are mapped to ADK event errors when possible. Use
`acperror.FromADKMetadata` to extract structured provider failure metadata:

```go
providerErr, ok := acperror.FromADKMetadata(event.CustomMetadata)
```

Known kinds include quota, authentication, payment, rate limit, unavailable,
session exhaustion, invalid request, and unknown. Use `Kind`, `Scope`, and
`RetryAt` for policy; the legacy `Retryable` field is compatibility data only.

## Inspecting ACP Agents

Before wiring a provider command into ADK, verify that it speaks ACP over
stdin/stdout and writes logs to stderr. Provider-specific ACP inspection tools
can help confirm initialization, session creation, config options, and prompt
updates.

## Trace Logs Contain Structural Metadata

Debug and trace logging omit prompt text, encoded media, resource URIs, and raw
JSON-RPC payloads. Trace adds protocol metadata and payload byte lengths.
Provider stderr is independent and may contain provider-owned diagnostics, so
capture it only in a controlled environment.

## Attachment Outcome Is Unknown

`acp_session_attachment_indeterminate` means protected conversation creation
cannot safely be repeated. Use the application's supported explicit reset or
new-conversation action before continuing; sending another ordinary message
cannot repair the retained fence. The message does not prove remote rejection,
creation, or history deletion. Caller/lifecycle cancellation remains a context
error rather than this handled failed final.

Structural `acp setup phase completed` records distinguish initialization gate
wait, process launch, initialize, attachment preflight/pending intent,
`session/new`, `session/resume` and post-attachment configuration. Fields include
`phase`, `method`, `elapsed_ms`, `configured_budget_ms`, `effective_budget_ms`,
`cause_class` and `outcome`. Effective budget is the remaining original deadline
at phase entry (`-1` when unbounded); phase logging does not restart the budget.
Caller cancellation/deadlines and lifecycle shutdown take precedence over an
internal setup deadline when classifying the current result.

`attempted_unknown` records call entry, not proof of sent bytes or peer
acceptance. `pending_cleared` describes the adapter's recorded protection state,
not proven remote rejection. The SDK converts several transport and peer errors
to the same request-error form; diagnostics do not infer provenance from error
strings. Raw causes remain in error chains, not phase log fields. Startup argv,
environment, cwd, prompt, metadata and capability URLs are omitted from these
records. Provider stderr remains separately controlled by the host.

These records improve evidence for a future incident; they do not identify the
cause of an earlier timeout. Keep initialize and attachment budgets distinct,
with earlier callers winning; increasing a timeout or replaying uncertain
creation is not an automatic recovery policy.
