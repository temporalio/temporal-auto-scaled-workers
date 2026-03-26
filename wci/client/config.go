package client

import "go.temporal.io/server/common/dynamicconfig"

type (
	AWSIAMRoleRequest struct {
		RoleARN         string
		RoleSessionName string
		ExternalID      *string
	}

	GCPIAMServiceAccountRequest struct {
		ServiceAccountEmail string
	}
)

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
	WorkerControllerAWSIntermediaryRoles = dynamicconfig.NewGlobalTypedSetting(
		"workercontroller.compute_providers.aws.intermediary_roles",
		[][]AWSIAMRoleRequest(nil),
		`WorkerControllerAWSIntermediaryRoles defines the role chain to assume before assuming the per-provider role.`,
	)
	WorkerControllerGCPIntermediaryServiceAccounts = dynamicconfig.NewGlobalTypedSetting(
		"workercontroller.compute_providers.gcp.intermediary_service_accounts",
		[][]GCPIAMServiceAccountRequest(nil),
		`WorkerControllerGCPIntermediaryServiceAccounts defines the service account chaining to assume before assuming the per-provider role.`,
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
	WorkerControllerAWSRequireRoleAndExternalID = dynamicconfig.NewGlobalBoolSetting(
		"workercontroller.compute_providers.aws.require_role_and_external_id",
		true,
		`WorkerControllerAWSRequireRoleAndExternalID controls whether AWS compute providers require a role ARN and external ID. Defaults to true. Set to false to allow configurations without these fields and accepting the associated security risks.`,
	)
	WorkerControllerMinSignalIntervalNoSyncMatchMilliseconds = dynamicconfig.NewNamespaceIntSetting(
		"workercontroller.hook.min_signal_interval_no_sync_match",
		500,
		`WorkerControllerMinSignalIntervalNoSyncMatchMilliseconds controls the batching interval of no-sync matches grouped by WCI in milliseconds (per namespace). Each batch triggers a signal. `,
	)
	WorkerControllerMinSignalIntervalSyncMatchMilliseconds = dynamicconfig.NewNamespaceIntSetting(
		"workercontroller.hook.min_signal_interval_sync_match",
		60_000, // 1 minute
		`WorkerControllerMinSignalIntervalSyncMatchMilliseconds controls the batching interval of sync matches grouped by WCI in milliseconds (per namespace). Each batch triggers a signal.`,
	)
)
