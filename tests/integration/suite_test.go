//go:build test_dep

package integration

import (
	"testing"

	"go.temporal.io/server/tests/testcore"
)

// TestWCISuite runs every WCI integration scenario as a subtest sharing one
// suite-scoped cluster (testcore.UseSuiteScopedCluster), instead of each
// scenario booting its own dedicated in-process Temporal server. Scenario
// bodies live in the other test files of this package, as unexported
// testWCI* functions,  so `go test` only discovers this single entry point.
func TestWCISuite(t *testing.T) {
	testcore.UseSuiteScopedCluster(t) //nolint:staticcheck // SA1019: suite reuses one worker-service cluster to avoid per-test cluster churn.

	t.Run("CanDeleteDrainingVersionWithOverride", testWCICanDeleteDrainingVersionWithOverride)
	t.Run("CannotDeleteCurrentVersion", testWCICannotDeleteCurrentVersion)
	t.Run("CannotDeleteDrainingVersion", testWCICannotDeleteDrainingVersion)
	t.Run("CannotDeleteDrainingVersionWithOverrideDueToActivePollers", testWCICannotDeleteDrainingVersionWithOverrideDueToActivePollers)
	t.Run("CannotDeleteRampingVersion", testWCICannotDeleteRampingVersion)
	t.Run("CannotDeleteWorkerDeploymentWithVersions", testWCICannotDeleteWorkerDeploymentWithVersions)
	t.Run("CreateVersionInvalidComputeConfig", testWCICreateVersionInvalidComputeConfig)
	t.Run("CreateWorkerDeploymentAlreadyExists", testWCICreateWorkerDeploymentAlreadyExists)
	t.Run("CreateWorkerDeploymentEmptyName", testWCICreateWorkerDeploymentEmptyName)
	t.Run("CreateWorkerDeploymentIdempotent", testWCICreateWorkerDeploymentIdempotent)
	t.Run("CreateWorkerDeploymentSuccess", testWCICreateWorkerDeploymentSuccess)
	t.Run("DeleteEmptyWorkerDeployment", testWCIDeleteEmptyWorkerDeployment)
	t.Run("DeleteNonexistentWorkerDeployment", testWCIDeleteNonexistentWorkerDeployment)
	t.Run("DescribeVersionReportsTaskQueueStats", testWCIDescribeVersionReportsTaskQueueStats)
	t.Run("CreateDeploymentVersionIdempotent", testWCICreateDeploymentVersionIdempotent)
	t.Run("CreateDeploymentVersionDeploymentNotFound", testWCICreateDeploymentVersionDeploymentNotFound)
	t.Run("DeleteDrainedVersion", testWCIDeleteDrainedVersion)
	t.Run("DescribeVersionReturnsCorrectComputeConfig", testWCIDescribeVersionReturnsCorrectComputeConfig)
	t.Run("DescribeWorkerDeploymentNotFound", testWCIDescribeWorkerDeploymentNotFound)
	t.Run("DescribeWorkerDeploymentVersionSummaries", testWCIDescribeWorkerDeploymentVersionSummaries)
	t.Run("DuplicateDeploymentVersionAlreadyExists", testWCIDuplicateDeploymentVersionAlreadyExists)
	t.Run("InstanceLifecycle", testWCIInstanceLifecycle)
	t.Run("InvokeIncompatibleWithRateBased", testWCIInvokeIncompatibleWithRateBased)
	t.Run("ListWorkerDeployments", testWCIListWorkerDeployments)
	t.Run("ListWorkerDeploymentsEmpty", testWCIListWorkerDeploymentsEmpty)
	t.Run("ListWorkerDeploymentsPagination", testWCIListWorkerDeploymentsPagination)
	t.Run("ManagerIdentityEnforcedOnSetCurrent", testWCIManagerIdentityEnforcedOnSetCurrent)
	t.Run("MetricsPollInvokesOnBacklog", testWCIMetricsPollInvokesOnBacklog)
	t.Run("ModalCreateVersionFunctionNotFound", testWCIModalCreateVersionFunctionNotFound)
	t.Run("ModalCreateVersionInvalidConfig", testWCIModalCreateVersionInvalidConfig)
	t.Run("ModalCreateVersionWithoutGrant", testWCIModalCreateVersionWithoutGrant)
	t.Run("ModalScaleUp", testWCIModalScaleUp)
	t.Run("MultipleVersionsInvokeWithPinnedWorkflows", testWCIMultipleVersionsInvokeWithPinnedWorkflows)
	t.Run("NoInvokeWhenWorkerPolling", testWCINoInvokeWhenWorkerPolling)
	t.Run("ScaleUp", testWCIScaleUp)
	t.Run("ScaleUpAfterComputeConfigUpdate", testWCIScaleUpAfterComputeConfigUpdate)
	t.Run("SetCurrentVersionAllowNoPollers", testWCISetCurrentVersionAllowNoPollers)
	t.Run("SetCurrentVersionHappyPath", testWCISetCurrentVersionHappyPath)
	t.Run("SetCurrentVersionMissingTaskQueuesAndOverride", testWCISetCurrentVersionMissingTaskQueuesAndOverride)
	t.Run("SetCurrentVersionStaleConflictToken", testWCISetCurrentVersionStaleConflictToken)
	t.Run("SetCurrentVersionToUnversioned", testWCISetCurrentVersionToUnversioned)
	t.Run("SetManagerHappyPath", testWCISetManagerHappyPath)
	t.Run("SetManagerOverride", testWCISetManagerOverride)
	t.Run("SetManagerStaleConflictToken", testWCISetManagerStaleConflictToken)
	t.Run("SetRampingVersionClear", testWCISetRampingVersionClear)
	t.Run("SetRampingVersionHappyPath", testWCISetRampingVersionHappyPath)
	t.Run("SetRampingVersionInvalidPercentage", testWCISetRampingVersionInvalidPercentage)
	t.Run("SetRampingVersionSameAsCurrent", testWCISetRampingVersionSameAsCurrent)
	t.Run("SetRampingVersionToUnversioned", testWCISetRampingVersionToUnversioned)
	t.Run("UpdateAndRemoveVersionComputeConfig", testWCIUpdateAndRemoveVersionComputeConfig)
	t.Run("UpdateVersionComputeConfigNoChange", testWCIUpdateVersionComputeConfigNoChange)

	t.Run("UpdateVersionInvalidComputeConfig", testWCIUpdateVersionInvalidComputeConfig)
	t.Run("VersionInactiveAfterInvoke", testWCIVersionInactiveAfterInvoke)
	t.Run("WorkerSetCreateVersionInvalidComputeConfig", testWCIWorkerSetCreateVersionInvalidComputeConfig)
	t.Run("WorkerSetIncompatibleWithNoSync", testWCIWorkerSetIncompatibleWithNoSync)
	t.Run("WorkerSetMultipleVersionsScaleIndependently", testWCIWorkerSetMultipleVersionsScaleIndependently)
	t.Run("WorkerSetRegistrationHonorsInitialCount", testWCIWorkerSetRegistrationHonorsInitialCount)
	t.Run("WorkerSetScaleDownToZero", testWCIWorkerSetScaleDownToZero)
	t.Run("WorkerSetScaleUp", testWCIWorkerSetScaleUp)
	t.Run("WorkerSetScaleUpPastOne", testWCIWorkerSetScaleUpPastOne)
	t.Run("WorkerSetUpdateDoesNotShrinkLiveSet", testWCIWorkerSetUpdateDoesNotShrinkLiveSet)
	t.Run("WorkerSetUpdateOnRegisteredVersionSkipsResize", testWCIWorkerSetUpdateOnRegisteredVersionSkipsResize)
}
