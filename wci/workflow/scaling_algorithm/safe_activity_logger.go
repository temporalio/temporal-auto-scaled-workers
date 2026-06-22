package scalingalgorithm

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"go.temporal.io/sdk/activity"
	sdklog "go.temporal.io/sdk/log"
)

type (
	fallbackLogger struct{}
)

var fallbackStdlibLogger = log.New(os.Stderr, "scalingalgorithm: ", log.LstdFlags)

func safeActivityLogger(ctx context.Context) (logger sdklog.Logger) {
	defer func() {
		logger = normalizeSDKLogger(logger, recover())
	}()
	logger = activity.GetLogger(ctx)
	return
}

// normalizeSDKLogger installs the fallback when activity.GetLogger either
// panicked (typical: called outside an activity context) or returned nil
// (permitted by the ActivityOutboundInterceptor contract — a custom interceptor
// could legitimately return nil). Both paths would otherwise nil-panic callers
// at the next .Info/.Warn call.
func normalizeSDKLogger(logger sdklog.Logger, recovered any) sdklog.Logger {
	if recovered != nil {
		fallbackStdlibLogger.Printf("ERROR safeActivityLogger: activity.GetLogger panicked, using fallback: %v", recovered)
		return fallbackLogger{}
	}
	if logger == nil {
		fallbackStdlibLogger.Printf("ERROR safeActivityLogger: activity.GetLogger returned nil, using fallback")
		return fallbackLogger{}
	}
	return logger
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
