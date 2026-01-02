package core

import "log/slog"

// attrsToAny converts []slog.Attr to []any so it can be appended into slog's variadic args.
func attrsToAny(attrs []slog.Attr) []any {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]any, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, a)
	}
	return out
}
