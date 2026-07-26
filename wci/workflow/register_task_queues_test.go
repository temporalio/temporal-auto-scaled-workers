package workflow

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	enumspb "go.temporal.io/api/enums/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
)

func TestTaskTypesAllRegistered(t *testing.T) {
	registered := map[enumspb.TaskQueueType]struct{}{
		enumspb.TASK_QUEUE_TYPE_WORKFLOW: {},
		enumspb.TASK_QUEUE_TYPE_ACTIVITY: {},
	}

	assert.True(t, taskTypesAllRegistered([]enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW}, registered))
	assert.True(t, taskTypesAllRegistered([]enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW, enumspb.TASK_QUEUE_TYPE_ACTIVITY}, registered))
	assert.False(t, taskTypesAllRegistered([]enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW, enumspb.TASK_QUEUE_TYPE_NEXUS}, registered),
		"a single unregistered type must force a worker invocation")
	assert.False(t, taskTypesAllRegistered(nil, registered),
		"empty want means nothing is known to exist yet, so it must not be treated as satisfied")
}

func TestRegisteredTaskQueueTypes(t *testing.T) {
	t.Run("parses registered types", func(t *testing.T) {
		fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
			return describeResponseWithTypes(enumspb.TASK_QUEUE_TYPE_WORKFLOW, enumspb.TASK_QUEUE_TYPE_ACTIVITY), nil
		}}
		got, err := NewActivities(nil, nil, fake).registeredTaskQueueTypes(t.Context(), "ns", "dep", "build")
		require.NoError(t, err)
		assert.Len(t, got, 2)
		assert.Contains(t, got, enumspb.TASK_QUEUE_TYPE_WORKFLOW)
		assert.Contains(t, got, enumspb.TASK_QUEUE_TYPE_ACTIVITY)
	})

	t.Run("propagates describe error", func(t *testing.T) {
		wantErr := errors.New("boom")
		fake := &fakeWorkflowServiceClient{describeFn: func(*workflowservice.DescribeWorkerDeploymentVersionRequest) (*workflowservice.DescribeWorkerDeploymentVersionResponse, error) {
			return nil, wantErr
		}}
		_, err := NewActivities(nil, nil, fake).registeredTaskQueueTypes(t.Context(), "ns", "dep", "build")
		assert.ErrorIs(t, err, wantErr)
	})
}
