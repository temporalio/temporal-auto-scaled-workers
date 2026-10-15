package scalingalgorithm

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	sdklog "go.temporal.io/sdk/log"
)

type (
	loggerKey      struct{}
	clockKey       struct{}
	fallbackLogger struct{}
)

var fallbackStdlibLogger = log.New(os.Stderr, "scalingalgorithm: ", log.LstdFlags)

// WithLogger sets the algorithms' logger; without one they log to stderr.
func WithLogger(ctx context.Context, logger sdklog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

// WithClock sets the algorithms' clock; without one they use time.Now.
func WithClock(ctx context.Context, now func() time.Time) context.Context {
	return context.WithValue(ctx, clockKey{}, now)
}

func loggerFrom(ctx context.Context) sdklog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(sdklog.Logger); ok && logger != nil {
		return logger
	}
	return fallbackLogger{}
}

func nowFrom(ctx context.Context) time.Time {
	if now, ok := ctx.Value(clockKey{}).(func() time.Time); ok && now != nil {
		return now()
	}
	return time.Now()
}

func (fallbackLogger) Debug(msg string, keyvals ...any) {
	fallbackStdlibLogger.Printf("DEBUG %s%s", msg, formatLoggerKeyvals(keyvals))
}

func (fallbackLogger) Info(msg string, keyvals ...any) {
	fallbackStdlibLogger.Printf("INFO  %s%s", msg, formatLoggerKeyvals(keyvals))
}

func (fallbackLogger) Warn(msg string, keyvals ...any) {
	fallbackStdlibLogger.Printf("WARN  %s%s", msg, formatLoggerKeyvals(keyvals))
}

func (fallbackLogger) Error(msg string, keyvals ...any) {
	fallbackStdlibLogger.Printf("ERROR %s%s", msg, formatLoggerKeyvals(keyvals))
}

func formatLoggerKeyvals(keyvals []any) string {
	if len(keyvals) == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(keyvals); i += 2 {
		b.WriteString(" ")
		if i+1 < len(keyvals) {
			fmt.Fprintf(&b, "%v=%v", keyvals[i], keyvals[i+1])
		} else {
			// Caller passed an odd number of keyvals — the sdklog.Logger
			// convention is pairs.
			fmt.Fprintf(&b, "%v=!BADKV", keyvals[i])
		}
	}
	return b.String()
}
