# go-adk-acpagent Documentation

Start with the README for install and quick usage. Use these pages when wiring
the adapter into a real ADK application.

## Guides

- [Concepts](concepts.md): what the adapter does, why it exists, and how ACP
  sessions map to ADK sessions.
- [Provider recipes](provider-recipes.md): OpenCode, Codex, Claude, PI, and
  generic ACP command examples.
- [Session state](session-state.md): cwd overrides, ACP session identity,
  metadata, config values, plan snapshots, and output state.
- [Troubleshooting](troubleshooting.md): process startup, stderr, session
  config, permissions, provider errors, and ACP inspection.
- [Migration from Norma](migration-from-norma.md): import path and config
  mapping from the deprecated Norma wrapper.

## Design Boundary

This repository documents the ACP-to-ADK adapter. It does not document Norma
PDCA, swarm, Beads, profile loading, or pool-agent orchestration except where
those old Norma concepts help explain migration to the standalone adapter.
