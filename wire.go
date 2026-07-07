package acpagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

type wireLoggingWriter struct {
	writer io.Writer
	buffer *wireLogBuffer
}

func newWireLoggingWriter(writer io.Writer, logger logger) io.Writer {
	return &wireLoggingWriter{writer: writer, buffer: newWireLogBuffer("send", logger, nil)}
}

func (w *wireLoggingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		w.buffer.append(p[:n])
	}
	if err != nil {
		w.buffer.logger.Warn().Err(err).Msg("failed to write acp stream")
	}
	return n, err
}

type wireLoggingReader struct {
	reader io.Reader
	buffer *wireLogBuffer
}

func newWireLoggingReader(reader io.Reader, logger logger, onSessionUpdate func(ExtendedSessionNotification)) io.Reader {
	return &wireLoggingReader{reader: reader, buffer: newWireLogBuffer("recv", logger, onSessionUpdate)}
}

func newConnectionStartReader(reader io.Reader) (io.Reader, func()) {
	gated := &connectionStartReader{reader: reader, ready: make(chan struct{})}
	return gated, gated.release
}

// connectionStartReader prevents the ACP SDK receive goroutine from reading
// connection state before NewClient finishes installing the connection logger.
type connectionStartReader struct {
	reader io.Reader
	ready  chan struct{}
	once   sync.Once
}

func (r *connectionStartReader) Read(p []byte) (int, error) {
	<-r.ready
	return r.reader.Read(p)
}

func (r *connectionStartReader) release() {
	r.once.Do(func() {
		close(r.ready)
	})
}

func (r *wireLoggingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.buffer.append(p[:n])
	}
	if err != nil && !errors.Is(err, io.EOF) {
		r.buffer.logger.Warn().Err(err).Msg("failed to read acp stream")
	}
	return n, err
}

type wireLogBuffer struct {
	direction string
	logger    logger
	onUpdate  func(ExtendedSessionNotification)

	mu  sync.Mutex
	buf []byte
}

func newWireLogBuffer(direction string, logger logger, onUpdate func(ExtendedSessionNotification)) *wireLogBuffer {
	return &wireLogBuffer{direction: direction, logger: logger, onUpdate: onUpdate}
}

func (b *wireLogBuffer) append(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf = append(b.buf, chunk...)
	for {
		idx := bytes.IndexByte(b.buf, '\n')
		if idx < 0 {
			return
		}
		line := bytes.TrimSpace(b.buf[:idx])
		b.buf = b.buf[idx+1:]
		if len(line) == 0 {
			continue
		}
		b.logLine(line)
	}
}

func (b *wireLogBuffer) logLine(line []byte) {
	type wireEnvelope struct {
		Method string          `json:"method,omitempty"`
		ID     json.RawMessage `json:"id,omitempty"`
		Params json.RawMessage `json:"params,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}

	var env wireEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		b.logger.Warn().
			Str("direction", b.direction).
			Err(err).
			Msg("failed to decode acp wire payload")
		return
	}

	if b.direction == "recv" && b.onUpdate != nil && len(env.Params) > 0 {
		switch env.Method {
		case acp.ClientMethodSessionUpdate:
			var note acp.SessionNotification
			if err := json.Unmarshal(env.Params, &note); err == nil {
				b.onUpdate(ExtendedSessionNotification{
					SessionNotification: note,
					Raw:                 env.Params,
					Method:              env.Method,
				})
			} else {
				b.logger.Warn().Err(err).Msg("failed to decode ordered session update")
			}
		default:
			if sessionID, ok := extractPromptScopedSessionID(env.Params); ok {
				b.onUpdate(ExtendedSessionNotification{
					SessionNotification: acp.SessionNotification{SessionId: acp.SessionId(sessionID)},
					Raw:                 env.Params,
					Method:              env.Method,
				})
			}
		}
	}

	kind := unknownValue
	switch {
	case env.Method != "" && len(env.ID) > 0:
		kind = "request"
	case env.Method != "":
		kind = "notification"
	case len(env.ID) > 0:
		kind = "response"
	}

	evt := b.logger.Trace().
		Str("direction", b.direction).
		Str("rpc_kind", kind)
	if env.Method != "" {
		evt = evt.Str("method", env.Method)
	}
	if len(env.ID) > 0 {
		evt = evt.Str("id", strings.TrimSpace(string(env.ID)))
	}
	if len(env.Params) > 0 {
		evt = evt.RawJSON("params", env.Params)
	}
	if len(env.Result) > 0 {
		evt = evt.RawJSON("result", env.Result)
	}
	if env.Error != nil {
		evt = evt.Int("error_code", env.Error.Code).Str("error_message", env.Error.Message)
	}
	evt.Msg("acp wire")
}

func extractPromptScopedSessionID(raw json.RawMessage) (string, bool) {
	var params struct {
		SessionID string `json:"sessionId"`
		ThreadID  string `json:"threadId"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return "", false
	}
	switch {
	case strings.TrimSpace(params.SessionID) != "":
		return strings.TrimSpace(params.SessionID), true
	case strings.TrimSpace(params.ThreadID) != "":
		return strings.TrimSpace(params.ThreadID), true
	default:
		return "", false
	}
}

func mustMarshalJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}
