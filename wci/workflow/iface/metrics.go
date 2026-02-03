package iface

import "go.temporal.io/server/common/metrics"

var (
	WorkerControllerInstanceCreated              = metrics.NewCounterDef("worker_controller_instance_created")
	WorkerControllerInstanceVisibilityQueryCount = metrics.NewCounterDef("worker_controller_instance_visibility_query_count")
)
