package computeprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nexus-rpc/sdk-go/nexus"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	wciclient "go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/server/common/dynamicconfig"
)

const WorkerCloudCapacityService = "WorkerRuntimeCapacityService"

type workerCloudComputeProvider struct {
	address     string
	callTimeout time.Duration
	dial        func(context.Context, sdkclient.Options) (sdkclient.Client, error)
}

// These JSON fields match the WC capacity contract without importing its implementation.
type workerCloudBinding struct {
	WorkerRuntimeID string `json:"worker_runtime_id"`
	CapacityOwner   string `json:"capacity_owner"`
	NamespaceID     string `json:"namespace_id"`
	Namespace       string `json:"namespace"`
	DeploymentName  string `json:"deployment_name"`
	BuildID         string `json:"build_id"`
}

type workerCloudResize struct {
	workerCloudBinding
	RequestID         string `json:"request_id"`
	RequestedReplicas int32  `json:"requested_replicas"`
	Sequence          int64  `json:"sequence"`
}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeWorkerCloud, NewWorkerCloudComputeProvider)
}

func NewWorkerCloudComputeProvider(_ context.Context, dc *dynamicconfig.Collection) (ComputeProvider, error) {
	return &workerCloudComputeProvider{address: wciclient.WorkerControllerWorkerCloudAddress.Get(dc)(), callTimeout: wciclient.WorkerControllerWorkerCloudCallTimeout.Get(dc)(), dial: sdkclient.DialContext}, nil
}

func (p *workerCloudComputeProvider) LaunchStrategy() LaunchStrategy { return LaunchStrategyWorkerSet }

func (p *workerCloudComputeProvider) InvokeWorker(context.Context, RequestContext, ComputeProviderConfig) error {
	return errors.ErrUnsupported
}

func WorkerCloudCapacityOwner(rc RequestContext) string {
	return "wci/" + rc.NamespaceID + "/" + rc.DeploymentName + "/" + rc.DeploymentBuildID + "/" + rc.ScalingGroupID
}

func workerCloudRequestID(binding workerCloudBinding, sequence int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d", binding.CapacityOwner, binding.WorkerRuntimeID, sequence)))
	return "wci-" + hex.EncodeToString(sum[:])
}

func workerCloudConfig(rc RequestContext, config ComputeProviderConfig) (workerCloudBinding, error) {
	id, _ := config["worker_runtime_id"].(string)
	if id == "" || rc.NamespaceID == "" || rc.NamespaceName == "" || rc.DeploymentName == "" || rc.DeploymentBuildID == "" || rc.ScalingGroupID == "" || rc.NexusEndpoint == "" {
		return workerCloudBinding{}, NewProviderError(FailureRejected, errors.New("worker cloud requires a Runtime, namespace identity, deployment, Build ID, scaling group, and Nexus endpoint"))
	}
	return workerCloudBinding{WorkerRuntimeID: id, CapacityOwner: WorkerCloudCapacityOwner(rc), NamespaceID: rc.NamespaceID, Namespace: rc.NamespaceName, DeploymentName: rc.DeploymentName, BuildID: rc.DeploymentBuildID}, nil
}

func (p *workerCloudComputeProvider) ValidateConfig(ctx context.Context, rc RequestContext, config ComputeProviderConfig) error {
	binding, err := workerCloudConfig(rc, config)
	if err != nil {
		return err
	}
	return p.execute(ctx, rc, "ValidateRuntimeCapacity", binding, "wc-validate-"+uuid.NewString())
}

func (p *workerCloudComputeProvider) UpdateWorkerSetSize(ctx context.Context, rc RequestContext, config ComputeProviderConfig, size int32) error {
	binding, err := workerCloudConfig(rc, config)
	if err != nil {
		return err
	}
	if size < 0 || rc.CapacitySequence <= 0 {
		return NewProviderError(FailureRejected, errors.New("worker cloud requires a nonnegative count and a positive decision sequence"))
	}
	id := workerCloudRequestID(binding, rc.CapacitySequence)
	return p.execute(ctx, rc, "SetWorkerRuntimeSize", workerCloudResize{workerCloudBinding: binding, RequestID: id, RequestedReplicas: size, Sequence: rc.CapacitySequence}, id)
}

func (p *workerCloudComputeProvider) execute(ctx context.Context, rc RequestContext, operation string, request any, id string) error {
	if p.callTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.callTimeout)
		defer cancel()
	}
	c, err := p.dial(ctx, sdkclient.Options{HostPort: p.address, Namespace: rc.NamespaceName})
	if err != nil {
		return NewProviderError(FailureUnavailable, err)
	}
	defer c.Close()
	nexus, err := c.NewNexusClient(sdkclient.NexusClientOptions{Endpoint: rc.NexusEndpoint, Service: WorkerCloudCapacityService})
	if err != nil {
		return NewProviderError(FailureRejected, err)
	}
	h, err := nexus.ExecuteOperation(ctx, operation, request, sdkclient.StartNexusOperationOptions{
		ID:               id,
		IDConflictPolicy: enumspb.NEXUS_OPERATION_ID_CONFLICT_POLICY_USE_EXISTING,
		IDReusePolicy:    enumspb.NEXUS_OPERATION_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	})
	var exists *serviceerror.NexusOperationExecutionAlreadyStarted
	if errors.As(err, &exists) {
		h = c.GetNexusOperationHandle(sdkclient.GetNexusOperationHandleOptions{OperationID: id})
		err = nil
	}
	if err == nil {
		err = h.Get(ctx, nil)
	}
	return workerCloudFailure(err)
}

func workerCloudFailure(err error) error {
	if err == nil {
		return nil
	}
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		switch app.Type() {
		case "InvalidArgument", "FailedPrecondition", "AlreadyExists", "NotFound", "BadRequest":
			return NewProviderError(FailureRejected, err)
		}
	}
	var handler *nexus.HandlerError
	if errors.As(err, &handler) && !handler.Retryable() {
		return NewProviderError(FailureRejected, err)
	}
	var invalid *serviceerror.InvalidArgument
	var missing *serviceerror.NotFound
	var denied *serviceerror.PermissionDenied
	if errors.As(err, &invalid) || errors.As(err, &missing) || errors.As(err, &denied) {
		return NewProviderError(FailureRejected, err)
	}
	return NewProviderError(FailureUnavailable, err)
}
