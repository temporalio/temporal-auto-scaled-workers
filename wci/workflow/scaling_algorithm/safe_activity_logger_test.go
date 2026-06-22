package scalingalgorithm

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdklog "go.temporal.io/sdk/log"
)

func TestFormatLoggerKeyvals(t *testing.T) {
	t.Run("empty keyvals returns empty string", func(t *testing.T) {
		assert.Equal(t, "", formatLoggerKeyvals(nil))
		assert.Equal(t, "", formatLoggerKeyvals([]any{}))
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

func TestNormalizeSDKLogger(t *testing.T) {
	t.Run("panic value installs fallback and surfaces the recovered value", func(t *testing.T) {
		var buf bytes.Buffer
		original := fallbackStdlibLogger.Writer()
		fallbackStdlibLogger.SetOutput(&buf)
		t.Cleanup(func() { fallbackStdlibLogger.SetOutput(original) })

		result := normalizeSDKLogger(nil, "boom-distinctive-marker")
		assert.IsType(t, fallbackLogger{}, result)
		out := buf.String()
		assert.Contains(t, out, "panicked", "format string must mention the panic")
		assert.Contains(t, out, "boom-distinctive-marker", "recovered value must appear via %v so future drift in the format string fails this test")
	})

	t.Run("nil logger with no panic installs fallback", func(t *testing.T) {
		var buf bytes.Buffer
		original := fallbackStdlibLogger.Writer()
		fallbackStdlibLogger.SetOutput(&buf)
		t.Cleanup(func() { fallbackStdlibLogger.SetOutput(original) })

		var nilLogger sdklog.Logger
		result := normalizeSDKLogger(nilLogger, nil)
		assert.IsType(t, fallbackLogger{}, result)
		assert.Contains(t, buf.String(), "returned nil")
	})

	t.Run("non-nil logger with no panic passes through silently", func(t *testing.T) {
		var buf bytes.Buffer
		original := fallbackStdlibLogger.Writer()
		fallbackStdlibLogger.SetOutput(&buf)
		t.Cleanup(func() { fallbackStdlibLogger.SetOutput(original) })

		input := fallbackLogger{}
		result := normalizeSDKLogger(input, nil)
		assert.Equal(t, input, result)
		assert.Empty(t, buf.String(), "happy path must not write to the fallback")
	})
}

func TestSafeActivityLoggerFallsBackOutsideActivityContext(t *testing.T) {
	var buf bytes.Buffer
	original := fallbackStdlibLogger.Writer()
	fallbackStdlibLogger.SetOutput(&buf)
	t.Cleanup(func() { fallbackStdlibLogger.SetOutput(original) })

	logger := safeActivityLogger(context.Background())
	require.NotNil(t, logger)
	assert.Contains(t, buf.String(), "panicked")

	buf.Reset()
	logger.Warn("downstream", "k", "v")
	downstream := buf.String()
	assert.Contains(t, downstream, "WARN")
	assert.Contains(t, downstream, "downstream")
	assert.Contains(t, downstream, "k=v")
}
