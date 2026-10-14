package client

import (
	"time"

	"go.temporal.io/server/common/dynamicconfig"
)

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

// Namespace-scoped settings.
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
		4,
		`WorkerControllerInstanceWorkflowVersion controls what version of the logic should the manager workflows use.`,
	)
	WorkerControllerValidateWorkflowVersion = dynamicconfig.NewNamespaceIntSetting(
		"workercontroller.validateWorkflowVersion",
		1,
		`WorkerControllerValidateWorkflowVersion controls what version of the logic should the validate workflows use.`,
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
	WorkerControllerEnabledComputeProviders = dynamicconfig.NewNamespaceTypedSetting(
		"workercontroller.compute_providers.enabled",
		[]string(nil),
		`WorkerControllerEnabledComputeProviders defines the list of compute providers enabled for use. Namespace-scoped: a namespace-level value overrides the cell-wide (global) value, and namespaces without an entry inherit the cell value.`,
	)
)

// Global settings.
var (
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
	WorkerControllerGCPFirstDelegateAsBase = dynamicconfig.NewGlobalBoolSetting(
		"workercontroller.compute_providers.gcp.first_delegate_as_base",
		false,
		`WorkerControllerGCPFirstDelegateAsBase controls how the first entry of the resolved GCP impersonation chain is treated. `+
			`When true, the first delegate is the identity the pod's ambient Workload Identity can *directly* impersonate `+
			`(roles/iam.workloadIdentityUser -> getAccessToken): it is used as the base credential of the chain, and the remaining `+
			`delegates are passed as token-creator delegates to the target. When false, the entire chain is passed as token-creator `+
			`delegates starting from the ambient ADC (default), which requires the ambient identity to hold implicitDelegation on the first delegate.`,
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
	WorkerControllerModalCredentialReloadLimit = dynamicconfig.NewGlobalIntSetting(
		"workercontroller.compute_providers.modal.credential_reload_limit",
		2,
		`WorkerControllerModalCredentialReloadLimit caps how many times WCI re-reads Vault for a grant whose credentials Modal rejected, before giving up for the cooldown window. Vault is not provisioned for data-plane request rates, so this is deliberately small.`,
	)
	WorkerControllerModalCredentialReloadCooldownSeconds = dynamicconfig.NewGlobalIntSetting(
		"workercontroller.compute_providers.modal.credential_reload_cooldown_s",
		int((15 * time.Minute).Seconds()), // 900
		`WorkerControllerModalCredentialReloadCooldownSeconds is how long WCI stops reading Vault for a grant after WorkerControllerModalCredentialReloadLimit consecutive reads failed to produce working credentials.`,
	)
	WorkerControllerModalFunctionCacheTTLSeconds = dynamicconfig.NewGlobalIntSetting(
		"workercontroller.compute_providers.modal.function_cache_ttl_s",
		int((10 * time.Minute).Seconds()), // 600
		`WorkerControllerModalFunctionCacheTTLSeconds is how long a resolved Modal Function handle is reused before being looked up again, so a worker invocation costs a spawn rather than a lookup plus a spawn. It bounds how long an App redeploy can go unnoticed in the case where the retired handle keeps working instead of failing; a handle that stops resolving is refreshed immediately. Zero disables expiry.`,
	)
	WorkerControllerModalClientIdleTimeoutSeconds = dynamicconfig.NewGlobalIntSetting(
		"workercontroller.compute_providers.modal.client_idle_timeout_s",
		int((30 * time.Minute).Seconds()), // 1800
		`WorkerControllerModalClientIdleTimeoutSeconds is how long an unused Modal connection is kept before being closed, bounding the connections held open for grants that have stopped scaling. Zero keeps them indefinitely.`,
	)
	WorkerControllerPeriodicValidationIntervalSeconds = dynamicconfig.NewGlobalIntSetting(
		"workercontroller.periodic_validation_interval_s",
		int((6 * time.Hour).Seconds()), // 21600
		`WorkerControllerPeriodicValidationIntervalSeconds controls the interval between periodic spec validation checks in seconds.`,
	)
)
