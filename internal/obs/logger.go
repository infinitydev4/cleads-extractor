package obs

import (
	"io"
	"log/slog"
	"strings"
)

// NewLogger builds a process logger: JSON (or text) records on w with
// secrets masked, teed to the database through the returned Tee.
func NewLogger(w io.Writer, format string, level slog.Level, opts TeeOptions) (*slog.Logger, *Tee) {
	opts.defaults()
	hopts := &slog.HandlerOptions{Level: level, ReplaceAttr: ReplaceAttr(opts.Redactor)}
	var inner slog.Handler
	if strings.EqualFold(format, "text") {
		inner = slog.NewTextHandler(w, hopts)
	} else {
		inner = slog.NewJSONHandler(w, hopts)
	}
	tee := NewTee(inner, opts)
	return slog.New(tee), tee
}

// ReplaceAttr masks secrets in string and error attributes written by a
// standard slog handler.
func ReplaceAttr(red *Redactor) func(groups []string, a slog.Attr) slog.Attr {
	return func(_ []string, a slog.Attr) slog.Attr {
		if secretKey(a.Key) {
			return slog.String(a.Key, masked)
		}
		switch a.Value.Kind() {
		case slog.KindString:
			if s := a.Value.String(); s != "" {
				if r := red.String(s); r != s {
					return slog.String(a.Key, r)
				}
			}
		case slog.KindAny:
			if err, ok := a.Value.Any().(error); ok {
				return slog.String(a.Key, red.String(err.Error()))
			}
		}
		return a
	}
}
