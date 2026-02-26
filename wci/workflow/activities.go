package workflow

import (
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/namespace"
)

type (
	Activities struct {
		dc                    *dynamicconfig.Collection
		namespace             *namespace.Namespace
		workflowserviceClient workflowservice.WorkflowServiceClient
	}
)

func NewActivities(namespace *namespace.Namespace, dc *dynamicconfig.Collection, workflowserviceClient workflowservice.WorkflowServiceClient) *Activities {
	return &Activities{
		dc:                    dc,
		namespace:             namespace,
		workflowserviceClient: workflowserviceClient,
	}
}
