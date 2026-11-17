package iface

// CapacityAdmission records the provider's response to one capacity decision.
// It does not describe later placement or automatically refreshed capacity.
type CapacityAdmission struct {
	WorkerRuntimeID   string   `json:"worker_runtime_id"`
	Sequence          int64    `json:"sequence"`
	RequestedReplicas int32    `json:"requested_replicas"`
	AdmittedReplicas  int32    `json:"admitted_replicas"`
	State             string   `json:"state"`
	LimitingReasons   []string `json:"limiting_reasons,omitempty"`
}
