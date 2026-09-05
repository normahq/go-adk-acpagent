# Changelog

## Unreleased

- Let explicit session config replace conflicting persisted values on resume.
- Preserve ordered ADK text, inline media, and file data in ACP prompts.
- Keep first-turn instructions separate from structured user content.
- Remove prompt and update payload content from trace and wire logs.
- Move the v2 module to the repository root.
- Add the module path for current Google ADK releases.
- Rename ACP provider error helpers from `providererror` to `acperror`.
- Project ACP provider failures onto ADK `ErrorCode` and `ErrorMessage`.
- Add OSS readiness docs and README status badges.

## v0.1.3

- Isolate lint tool dependencies from the runtime module.
