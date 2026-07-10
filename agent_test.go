package acpagent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

const (
	testACPCallID     = "call-1"
	testACPToolID     = "tool-1"
	testACPPlanPrompt = "planning"

	testSessionOneID    = "session-1"
	testSessionOneHello = "session-1:hello"
	testSessionOneOne   = "session-1:one"
	testSessionOneTwo   = "session-1:two"
	testSessionTwoTwo   = "session-2:two"
)

func instructionPrompt(instruction string, prompt string) string {
	return strings.TrimSpace(instruction) + "\n\nUser message:\n" + prompt
}

func expectedPromptsJSON(t *testing.T, prompts ...string) string {
	t.Helper()
	raw, err := json.Marshal(prompts)
	if err != nil {
		t.Fatalf("json.Marshal(expected prompts) error = %v", err)
	}
	return string(raw)
}

func closeTestCloser(t *testing.T, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

func testSlogLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

func testLogger(w io.Writer, level slog.Level) logger {
	return newLogger(testSlogLogger(w, level), "")
}

type testLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type contextCaptureHandler struct {
	slog.Handler
	key  any
	seen chan any
}

func (h *contextCaptureHandler) Handle(ctx context.Context, record slog.Record) error {
	select {
	case h.seen <- ctx.Value(h.key):
	default:
	}
	return h.Handler.Handle(ctx, record)
}

func (h *contextCaptureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextCaptureHandler{Handler: h.Handler.WithAttrs(attrs), key: h.key, seen: h.seen}
}

func (h *contextCaptureHandler) WithGroup(name string) slog.Handler {
	return &contextCaptureHandler{Handler: h.Handler.WithGroup(name), key: h.key, seen: h.seen}
}

func (b *testLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *testLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *testLogBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
