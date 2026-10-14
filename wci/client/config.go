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

	// ModalVaultConfig locates the Vault instance holding Modal OAuth material and
	// names the paths and fields it is stored under. Every field is settable so a
	// cell can point at its own Vault layout without a code change.
	ModalVaultConfig struct {
		// Address is the Vault API base URL, e.g. https://vault.test.tmprl-test.cloud.
		// An empty Address disables the Modal compute provider: with no secret
		// source there is no grant to resolve, and the provider fails closed.
		Address string
		// Namespace is the Vault Enterprise/HCP namespace (e.g. "admin"), if any.
		Namespace string
		// CACertPath is an optional PEM bundle used to verify the Vault server.
		CACertPath string
		// KVMount is the KV v2 mount the Modal secrets live under.
		KVMount string
		// AuthMount is the Kubernetes auth method mount path, which is per-cell.
		AuthMount string
		// AuthRole is the Vault role the cell's Kubernetes service account logs in as.
		AuthRole string
		// ServiceAccountTokenPath is the projected service account token the
		// Kubernetes auth login presents. The kubelet rotates this file, so it is
		// re-read on every login rather than cached.
		ServiceAccountTokenPath string
		// SigningKeyPath is the KV v2 secret holding WCI's own Modal OAuth client
		// credentials, relative to KVMount.
		SigningKeyPath string
		// SigningKeyField is the field of SigningKeyPath holding the PEM-encoded RSA
		// private key used to sign client assertions.
		SigningKeyField string
		// OAuthClientIDField is the field of SigningKeyPath holding the Modal OAuth
		// client ID. When absent, WorkerControllerModalOAuthClientID is used.
		OAuthClientIDField string
		// GrantPathPrefix is the KV v2 prefix under which per-grant refresh tokens
		// live, relative to KVMount. The full path is
		// <GrantPathPrefix>/<account id>/<workspace id>.
		GrantPathPrefix string
		// RefreshTokenField is the field of a grant secret holding the refresh token.
		RefreshTokenField string
		// RequestTimeoutSeconds bounds a single Vault API call.
		RequestTimeoutSeconds int
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
	WorkerControllerModalVaultConfig = dynamicconfig.NewGlobalTypedSetting(
		"workercontroller.compute_providers.modal.vault",
		ModalVaultConfig{
			KVMount:                 "global",
			ServiceAccountTokenPath: "/var/run/secrets/kubernetes.io/serviceaccount/token",
			SigningKeyPath:          "compute/modal-oauth/test",
			SigningKeyField:         "jwtKey",
			OAuthClientIDField:      "clientId",
			GrantPathPrefix:         "compute/modal-oauth/grants",
			RefreshTokenField:       "refreshToken",
			RequestTimeoutSeconds:   10,
		},
		`WorkerControllerModalVaultConfig locates the Vault instance holding Modal OAuth material and names the paths and fields it is stored under. Address must be set for the Modal compute provider to function; without it the provider fails closed.`,
	)
	WorkerControllerModalOAuthClientID = dynamicconfig.NewGlobalStringSetting(
		"workercontroller.compute_providers.modal.oauth_client_id",
		"",
		`WorkerControllerModalOAuthClientID is the Modal-issued OAuth client ID ("oc-" prefix) WCI authenticates as. It is only consulted when the Vault signing-key secret does not carry the client ID itself.`,
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
