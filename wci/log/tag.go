package log

import "go.temporal.io/server/common/log/tag"

var ComponentWorkerController = componentTag("worker-controller")

// Component returns tag for Component
func componentTag(component string) tag.ZapTag {
	return tag.NewStringTag("component", component)
}
