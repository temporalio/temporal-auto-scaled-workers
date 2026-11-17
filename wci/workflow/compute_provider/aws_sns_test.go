package computeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/client"
)

const testSNSTopicARN = "arn:aws:sns:us-east-1:123456789012:worker-activation"

type mockSNSClient struct {
	publishFn  func(context.Context, *sns.PublishInput, ...func(*sns.Options)) (*sns.PublishOutput, error)
	getTopicFn func(context.Context, *sns.GetTopicAttributesInput, ...func(*sns.Options)) (*sns.GetTopicAttributesOutput, error)
}

func (m *mockSNSClient) Publish(ctx context.Context, in *sns.PublishInput, opts ...func(*sns.Options)) (*sns.PublishOutput, error) {
	return m.publishFn(ctx, in, opts...)
}

func (m *mockSNSClient) GetTopicAttributes(ctx context.Context, in *sns.GetTopicAttributesInput, opts ...func(*sns.Options)) (*sns.GetTopicAttributesOutput, error) {
	return m.getTopicFn(ctx, in, opts...)
}

func newSNSProvider() *awsSNSComputeProvider {
	return &awsSNSComputeProvider{intermediaryRoles: [][]client.AWSIAMRoleRequest{}}
}

func stubSNSClient(t *testing.T, c snsAPI) {
	old := newSNSClientFn
	newSNSClientFn = func(context.Context, string, string, *string, [][]client.AWSIAMRoleRequest) (snsAPI, error) {
		return c, nil
	}
	t.Cleanup(func() { newSNSClientFn = old })
}

func TestAWSSNSInvokeWorker_PublishesActivationMessage(t *testing.T) {
	var topic, body string
	stubSNSClient(t, &mockSNSClient{
		publishFn: func(_ context.Context, in *sns.PublishInput, _ ...func(*sns.Options)) (*sns.PublishOutput, error) {
			topic, body = aws.ToString(in.TopicArn), aws.ToString(in.Message)
			return &sns.PublishOutput{}, nil
		},
	})

	rc := RequestContext{NamespaceName: "ns", DeploymentName: "deployment", DeploymentBuildID: "build-123"}
	require.NoError(t, newSNSProvider().InvokeWorker(t.Context(), rc, ComputeProviderConfig{configAWSSNSTopicARN: testSNSTopicARN}))
	assert.Equal(t, testSNSTopicARN, topic)

	var msg AWSSNSWorkerActivationMessage
	require.NoError(t, json.Unmarshal([]byte(body), &msg))
	assert.Equal(t, AWSSNSWorkerActivationEvent, msg.Event)
	assert.Equal(t, "ns", msg.NamespaceName)
	assert.Equal(t, "deployment", msg.DeploymentName)
	assert.Equal(t, "build-123", msg.BuildID)
}

func TestAWSSNSInvokeWorker_PublishError(t *testing.T) {
	sentinel := errors.New("publish failed")
	stubSNSClient(t, &mockSNSClient{
		publishFn: func(context.Context, *sns.PublishInput, ...func(*sns.Options)) (*sns.PublishOutput, error) {
			return nil, sentinel
		},
	})
	err := newSNSProvider().InvokeWorker(t.Context(), RequestContext{}, ComputeProviderConfig{configAWSSNSTopicARN: testSNSTopicARN})
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

func TestAWSSNSInvokeWorker_InvalidTopicARN(t *testing.T) {
	err := newSNSProvider().InvokeWorker(t.Context(), RequestContext{}, ComputeProviderConfig{configAWSSNSTopicARN: "not-an-arn"})
	require.Error(t, err)
}

func TestAWSSNSValidateConfig_ValidatesTopic(t *testing.T) {
	var topic string
	stubSNSClient(t, &mockSNSClient{
		getTopicFn: func(_ context.Context, in *sns.GetTopicAttributesInput, _ ...func(*sns.Options)) (*sns.GetTopicAttributesOutput, error) {
			topic = aws.ToString(in.TopicArn)
			return &sns.GetTopicAttributesOutput{}, nil
		},
	})
	require.NoError(t, newSNSProvider().ValidateConfig(t.Context(), RequestContext{}, ComputeProviderConfig{configAWSSNSTopicARN: testSNSTopicARN}))
	assert.Equal(t, testSNSTopicARN, topic)
}

func TestAWSSNSValidateConfig_RoleAndExternalIDAreOptional(t *testing.T) {
	var topic string
	stubSNSClient(t, &mockSNSClient{
		getTopicFn: func(_ context.Context, in *sns.GetTopicAttributesInput, _ ...func(*sns.Options)) (*sns.GetTopicAttributesOutput, error) {
			topic = aws.ToString(in.TopicArn)
			return &sns.GetTopicAttributesOutput{}, nil
		},
	})
	// The default AWS credential chain provides the subscriber's local credentials; role overrides are optional.
	p := newSNSProvider()
	require.NoError(t, p.ValidateConfig(t.Context(), RequestContext{}, ComputeProviderConfig{configAWSSNSTopicARN: testSNSTopicARN}))
	assert.Equal(t, testSNSTopicARN, topic)
}

func TestAWSSNSUsesLocalCredentialsWhenRoleOmitted(t *testing.T) {
	var gotRole string
	var gotExternalID *string
	var gotIntermediaryRoles [][]client.AWSIAMRoleRequest
	old := newSNSClientFn
	newSNSClientFn = func(_ context.Context, _ string, role string, externalID *string, roles [][]client.AWSIAMRoleRequest) (snsAPI, error) {
		gotRole, gotExternalID, gotIntermediaryRoles = role, externalID, roles
		return &mockSNSClient{
			publishFn: func(context.Context, *sns.PublishInput, ...func(*sns.Options)) (*sns.PublishOutput, error) {
				return &sns.PublishOutput{}, nil
			},
		}, nil
	}
	t.Cleanup(func() { newSNSClientFn = old })

	p := &awsSNSComputeProvider{
		intermediaryRoles: [][]client.AWSIAMRoleRequest{{
			{RoleARN: "arn:aws:iam::123456789012:role/intermediary", RoleSessionName: "test"},
		}},
	}
	require.NoError(t, p.InvokeWorker(t.Context(), RequestContext{}, ComputeProviderConfig{configAWSSNSTopicARN: testSNSTopicARN}))
	assert.Empty(t, gotRole)
	assert.Nil(t, gotExternalID)
	assert.Nil(t, gotIntermediaryRoles)
}
