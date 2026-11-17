package logger

import (
	"context"
	"github.com/snowflakedb/gosnowflake/v2/sflog"
	"log/slog"
)

// snowflakeHandler wraps slog.Handler and adds context field extraction
type snowflakeHandler struct {
	inner    slog.Handler
	levelVar *slog.LevelVar
}

func newSnowflakeHandler(inner slog.Handler, level sflog.Level) *snowflakeHandler {
	levelVar := &slog.LevelVar{}
	levelVar.Set(slog.Level(level))
	return &snowflakeHandler{
		inner:    inner,
		levelVar: levelVar,
	}
}

// Enabled checks if the handler is enabled for the given level
func (h *snowflakeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle processes a log record
func (h *snowflakeHandler) Handle(ctx context.Context, r slog.Record) error {
	// NOTE: Context field extraction is NOT done here because:
	// - If WithContext() was used, fields are already added to the logger via .With()
	// - If WithContext() was not used, the context passed here is typically context.Background()
	//   and wouldn't have any fields anyway

	// Secret masking is already done in secretMaskingLogger wrapper
	return h.inner.Handle(ctx, r)
}

// WithAttrs creates a new handler with additional attributes
func (h *snowflakeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &snowflakeHandler{
		inner:    h.inner.WithAttrs(attrs),
		levelVar: h.levelVar,
	}
}

// WithGroup creates a new handler with a group
func (h *snowflakeHandler) WithGroup(name string) slog.Handler {
	return &snowflakeHandler{
		inner:    h.inner.WithGroup(name),
		levelVar: h.levelVar,
	}
}
