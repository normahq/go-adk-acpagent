package acpagent

import (
	"context"
	"encoding/json"
	"log/slog"
)

const levelTrace = slog.LevelDebug - 4

type loggerContextKey struct{}

type logger struct {
	inner *slog.Logger
	ctx   context.Context
}

func newLogger(base *slog.Logger, subcomponent string) logger {
	if base == nil {
		base = slog.New(slog.DiscardHandler)
	}
	if subcomponent != "" {
		base = base.With("subcomponent", subcomponent)
	}
	return logger{inner: base}
}

func loggerFromContext(ctx context.Context, fallback logger, subcomponent string) logger {
	if ctx == nil {
		return fallback
	}
	if ctxLogger, ok := ctx.Value(loggerContextKey{}).(logger); ok && ctxLogger.inner != nil {
		if subcomponent != "" {
			return logger{inner: ctxLogger.inner.With("subcomponent", subcomponent), ctx: ctx}
		}
		return ctxLogger.withContext(ctx)
	}
	return fallback.withContext(ctx)
}

func contextWithLogger(ctx context.Context, l logger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, loggerContextKey{}, l)
}

func (l logger) slog() *slog.Logger {
	if l.inner == nil {
		return slog.New(slog.DiscardHandler)
	}
	return l.inner
}

func (l logger) with(attrs ...any) logger {
	return logger{inner: l.slog().With(attrs...), ctx: l.ctx}
}

func (l logger) withContext(ctx context.Context) logger {
	if ctx != nil {
		l.ctx = ctx
	}
	return l
}

func (l logger) enabled(level slog.Level) bool {
	ctx := l.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return l.slog().Enabled(ctx, level)
}

func (l logger) Debug() *logEvent {
	return l.event(slog.LevelDebug)
}

func (l logger) Error() *logEvent {
	return l.event(slog.LevelError)
}

func (l logger) Trace() *logEvent {
	return l.event(levelTrace)
}

func (l logger) Warn() *logEvent {
	return l.event(slog.LevelWarn)
}

func (l logger) event(level slog.Level) *logEvent {
	return &logEvent{logger: l.slog(), ctx: l.ctx, level: level, enabled: l.enabled(level)}
}

type logEvent struct {
	logger  *slog.Logger
	ctx     context.Context
	level   slog.Level
	enabled bool
	attrs   []slog.Attr
}

func (e *logEvent) Bool(key string, value bool) *logEvent {
	if !e.enabled {
		return e
	}
	e.attrs = append(e.attrs, slog.Bool(key, value))
	return e
}

func (e *logEvent) Err(err error) *logEvent {
	if e.enabled && err != nil {
		e.attrs = append(e.attrs, slog.Any("error", err))
	}
	return e
}

func (e *logEvent) Int(key string, value int) *logEvent {
	if !e.enabled {
		return e
	}
	e.attrs = append(e.attrs, slog.Int(key, value))
	return e
}

func (e *logEvent) Interface(key string, value any) *logEvent {
	if !e.enabled {
		return e
	}
	e.attrs = append(e.attrs, slog.Any(key, value))
	return e
}

func (e *logEvent) RawJSON(key string, value json.RawMessage) *logEvent {
	if e.enabled && len(value) > 0 {
		e.attrs = append(e.attrs, slog.String(key, string(value)))
	}
	return e
}

func (e *logEvent) Str(key string, value string) *logEvent {
	if !e.enabled {
		return e
	}
	e.attrs = append(e.attrs, slog.String(key, value))
	return e
}

func (e *logEvent) Strs(key string, value []string) *logEvent {
	if !e.enabled {
		return e
	}
	e.attrs = append(e.attrs, slog.Any(key, value))
	return e
}

func (e *logEvent) Msg(msg string) {
	if e.logger == nil || !e.enabled {
		return
	}
	ctx := e.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	e.logger.LogAttrs(ctx, e.level, msg, e.attrs...)
}
