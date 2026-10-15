package scalingalgorithm

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sdklog "go.temporal.io/sdk/log"
)

func TestFormatLoggerKeyvals(t *testing.T) {
	t.Run("empty keyvals returns empty string", func(t *testing.T) {
		assert.Empty(t, formatLoggerKeyvals(nil))
		assert.Empty(t, formatLoggerKeyvals([]any{}))
	})

	t.Run("single pair", func(t *testing.T) {
		assert.Equal(t, " k=v", formatLoggerKeyvals([]any{"k", "v"}))
	})

	t.Run("multiple pairs preserve order and use %v formatting", func(t *testing.T) {
		assert.Equal(t, " a=1 b=2.5", formatLoggerKeyvals([]any{"a", 1, "b", 2.5}))
	})

	t.Run("odd trailing key surfaces as !BADKV", func(t *testing.T) {
		// Documented contract: callers passing an odd-length keyvals slice get
		// the orphan key marked with the !BADKV token so log-parsing systems
		// can alert on malformed call sites instead of seeing a silent `key=`.
		assert.Equal(t, " k1=v1 orphan=!BADKV", formatLoggerKeyvals([]any{"k1", "v1", "orphan"}))
	})
}

func TestFallbackLoggerLevelsRouteToStderrLogger(t *testing.T) {
	// Redirect the package-level fallback writer to capture output; restoring
	// at cleanup prevents cross-test pollution.
	var buf bytes.Buffer
	original := fallbackStdlibLogger.Writer()
	fallbackStdlibLogger.SetOutput(&buf)
	t.Cleanup(func() { fallbackStdlibLogger.SetOutput(original) })

	l := fallbackLogger{}
	cases := []struct {
		name     string
		levelTag string
		emit     func()
	}{
		{"Debug", "DEBUG", func() { l.Debug("hello", "k", "v") }},
		{"Info", "INFO", func() { l.Info("hello", "k", "v") }},
		{"Warn", "WARN", func() { l.Warn("hello", "k", "v") }},
		{"Error", "ERROR", func() { l.Error("hello", "k", "v") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf.Reset()
			c.emit()
			out := buf.String()
			assert.Contains(t, out, c.levelTag)
			assert.Contains(t, out, "hello")
			assert.Contains(t, out, "k=v")
		})
	}
}

func TestLoggerFrom(t *testing.T) {
	t.Run("without a logger falls back to stderr", func(t *testing.T) {
		var buf bytes.Buffer
		original := fallbackStdlibLogger.Writer()
		fallbackStdlibLogger.SetOutput(&buf)
		t.Cleanup(func() { fallbackStdlibLogger.SetOutput(original) })

		logger := loggerFrom(t.Context())
		assert.IsType(t, fallbackLogger{}, logger)
		logger.Warn("downstream", "k", "v")
		assert.Contains(t, buf.String(), "WARN  downstream k=v")
	})

	t.Run("nil logger falls back", func(t *testing.T) {
		var nilLogger sdklog.Logger
		assert.IsType(t, fallbackLogger{}, loggerFrom(WithLogger(t.Context(), nilLogger)))
	})

	t.Run("returns the logger set on the context", func(t *testing.T) {
		var buf bytes.Buffer
		logger := sdklog.NewStructuredLogger(slog.New(slog.NewTextHandler(&buf, nil)))
		loggerFrom(WithLogger(t.Context(), logger)).Info("hello")
		assert.Contains(t, buf.String(), "hello")
	})
}

func TestNowFrom(t *testing.T) {
	t.Run("without a clock uses wall time", func(t *testing.T) {
		before := time.Now()
		now := nowFrom(t.Context())
		require.False(t, now.Before(before))
	})

	t.Run("uses the clock set on the context", func(t *testing.T) {
		fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		assert.Equal(t, fixed, nowFrom(WithClock(t.Context(), func() time.Time { return fixed })))
	})
}
