package scalingalgorithm

import (
	"context"

	"go.temporal.io/sdk/activity"
	sdklog "go.temporal.io/sdk/log"
)

type (
	noopLogger struct{}
)

func safeActivityLogger(ctx context.Context) (logger sdklog.Logger) {
	logger = noopLogger{}
	defer func() {
		if recover() != nil {
			logger = noopLogger{}
		}
	}()
	return activity.GetLogger(ctx)
}

func (noopLogger) Debug(string, ...interface{}) {}
func (noopLogger) Info(string, ...interface{})  {}
func (noopLogger) Warn(string, ...interface{})  {}
func (noopLogger) Error(string, ...interface{}) {}
