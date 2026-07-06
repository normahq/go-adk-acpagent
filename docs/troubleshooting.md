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
PermissionHandler: func(ctx context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeCancelled(),
	}, nil
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
invalid request, and unknown.

## Inspecting ACP Agents

Before wiring a provider command into ADK, verify that it speaks ACP over
stdin/stdout and writes logs to stderr. Provider-specific ACP inspection tools
can help confirm initialization, session creation, config options, and prompt
updates.
