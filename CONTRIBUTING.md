# Contributing

## Development

Use the Go version declared in `go.mod`.

Before submitting changes, run:

```sh
go mod tidy
go test ./...
go test -race ./...
GOBIN="$PWD/.bin" go -C tools install github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOBIN="$PWD/.bin" go -C tools install golang.org/x/vuln/cmd/govulncheck
./.bin/golangci-lint run ./...
./.bin/govulncheck ./...
```

## Compatibility

Keep public APIs backward compatible within a major version. Breaking changes
belong in a new major module path.
