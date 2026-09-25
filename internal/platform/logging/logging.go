// Package logging builds the JSON slog logger shared by every binary and lets
// request-scoped attributes (request_id, order_id...) travel in the context.
//
// Call sites must use the *Context logging methods (InfoContext, ErrorContext)
// for the context attributes to be attached.
package logging

import (
	"context"
	"io"
	"log/slog"
)

type ctxKey struct{}

// With returns a copy of ctx carrying attrs in addition to any attributes
// already attached. Every record logged with the returned context includes
// them at the top level.
func With(ctx context.Context, attrs ...slog.Attr) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	prev := fromContext(ctx)
	merged := make([]slog.Attr, 0, len(prev)+len(attrs))
	merged = append(merged, prev...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, ctxKey{}, merged)
}

func fromContext(ctx context.Context) []slog.Attr {
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	return attrs
}

// New returns a logger writing JSON to w at the given minimum level.
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(&contextHandler{
		Handler: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}),
	})
}

// contextHandler adds the attributes stored by With to each record.
//
// Attributes are added to the record itself, so if the logger was derived
// with WithGroup they end up inside that group. None of our loggers use
// groups, so request_id always stays at the top level.
type contextHandler struct {
	slog.Handler
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := fromContext(ctx); len(attrs) > 0 {
		r = r.Clone()
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}
