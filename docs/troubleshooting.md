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

ACP runtimes must write protocol messages to stdout and logs to stderr. If a
provider writes logs to stdout, it can corrupt the JSON-RPC stream. Use the
provider's quiet/stdio mode or wrap it with a script that keeps logs on stderr.

## Model Selection Fails

`Config.Model` uses ACP `session/set_config_option`, not the legacy
`session/set_model` method.

If model selection fails:

- confirm the ACP runtime returns model config options from `session/new` or
  `session/resume`
- set `Config.ModelConfigID` when the runtime uses a nonstandard option ID
- inspect logs with `Config.Logger`
- forward stderr while debugging provider startup and capability negotiation

## Mode Selection Fails

`Config.Mode` uses ACP `session/set_mode`. Use only mode IDs reported by the ACP
runtime. If the runtime does not support modes, leave `Config.Mode` empty.

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

## Inspecting ACP Runtimes

Norma includes useful standalone tools for ACP runtime inspection:

```sh
acp-dump -- opencode acp
acp-repl --model opencode/big-pickle --mode coding -- opencode acp
```

These are not part of `go-adk-acpagent`, but they are useful when verifying a
provider command before wiring it into ADK.
