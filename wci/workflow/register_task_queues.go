package workflow

import (
	"context"
	"fmt"

	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
)

// registeredTaskQueueTypes returns the set of task queue types already registered for the
// given worker deployment version. A type is present once a worker has polled it under this
// version, and the association is durable for the version's lifetime. Stats are not requested;
// only the queue identities are needed.
func (a *Activities) registeredTaskQueueTypes(ctx context.Context, namespaceName, deploymentName, deploymentBuildID string) (map[enumspb.TaskQueueType]struct{}, error) {
	resp, err := a.workflowserviceClient.DescribeWorkerDeploymentVersion(ctx, &workflowservice.DescribeWorkerDeploymentVersionRequest{
		Namespace: namespaceName,
		DeploymentVersion: &deploymentpb.WorkerDeploymentVersion{
			DeploymentName: deploymentName,
			BuildId:        deploymentBuildID,
		},
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("did not receive details in the describe response")
	}

	registered := map[enumspb.TaskQueueType]struct{}{}
	for _, taskQueue := range resp.VersionTaskQueues {
		if taskQueue == nil {
			continue
		}
		registered[taskQueue.Type] = struct{}{}
	}
	return registered, nil
}

// taskTypesAllRegistered reports whether every task type in want is present in registered.
// An empty want means no queue is yet known to exist, so it returns false to force a bootstrap invocation.
func taskTypesAllRegistered(want []enumspb.TaskQueueType, registered map[enumspb.TaskQueueType]struct{}) bool {
	if len(want) == 0 {
		return false
	}
	for _, taskQueue := range want {
		if _, ok := registered[taskQueue]; !ok {
			return false
		}
	}
	return true
}
