// Package logutil holds small logging helpers.
package logutil

import "log/slog"

// WarnIfErr logs non-nil err at warn level. For best-effort calls whose
// failure should not abort the caller but must not vanish silently.
func WarnIfErr(err error, msg string, args ...any) {
	if err != nil {
		slog.Warn(msg, append(args, "error", err)...)
	}
}
