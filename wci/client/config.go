package client

import "go.temporal.io/server/common/dynamicconfig"

var (
	WorkerControllerEnabled = dynamicconfig.NewNamespaceBoolSetting(
		"workercontroller.enabled",
		false,
		`WorkerControllerEnabled is a "feature enable" flag. When enabled it allows clients to configure compute providers.`,
	)
	WorkerControllerMaxInstances = dynamicconfig.NewNamespaceIntSetting(
		"workercontroller.maxInstances",
		100,
		`WorkerControllerMaxInstances represents the maximum number of worker controller instances that can be registered in a single namespace`,
	)
	WorkerControllerInstanceWorkflowVersion = dynamicconfig.NewNamespaceIntSetting(
		"workercontroller.instanceWorkflowVersion",
		0,
		`WorkerControllerInstanceWorkflowVersion controls what version of the logic should the manager workflows use.`,
	)
	WorkerControllerEnabledComputeProviders = dynamicconfig.NewGlobalTypedSetting(
		"workercontroller.compute_providers.enabled",
		[]string(nil),
		`WorkerControllerEnabledComputeProviders defines the list of compute providers enabled for use.`,
	)
	WorkerControllerEnabledScalingAlgorithms = dynamicconfig.NewGlobalTypedSetting(
		"workercontroller.scaling_algorithms.enabled",
		[]string(nil),
		`WorkerControllerEnabledScalingAlgorithm defines the list of scaling algorithms enabled for use.`,
	)
)
