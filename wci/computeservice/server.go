// Package computeservice implements WorkerControllerComputeService over the compute provider
// registry. History runs it in process; the WCI process serves it to history for providers that
// need the WCI process's cloud identity.
package computeservice

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/auto-scaled-workers/wci/compute"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/api/workercontrollerservice/v1"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/sdk"
)

type Server struct {
	workercontrollerservice.UnimplementedWorkerControllerComputeServiceServer
	dc *dynamicconfig.Collection
}

var _ workercontrollerservice.WorkerControllerComputeServiceServer = (*Server)(nil)

func NewServer(dc *dynamicconfig.Collection) *Server {
	return &Server{dc: dc}
}

func (s *Server) ValidateConfig(ctx context.Context, req *workercontrollerservice.ValidateConfigRequest) (*workercontrollerservice.ValidateConfigResponse, error) {
	rc := RequestContextFromProto(req.GetContext())
	failure := s.call(ctx, rc, req.GetProvider(), func(p compute.ComputeProvider, cfg compute.ComputeProviderConfig) error {
		return p.ValidateConfig(ctx, rc, cfg)
	})
	return &workercontrollerservice.ValidateConfigResponse{Failure: failure}, nil
}

// InvokeWorker ignores request_id until a provider can de-duplicate on it.
func (s *Server) InvokeWorker(ctx context.Context, req *workercontrollerservice.InvokeWorkerRequest) (*workercontrollerservice.InvokeWorkerResponse, error) {
	rc := RequestContextFromProto(req.GetContext())
	failure := s.call(ctx, rc, req.GetProvider(), func(p compute.ComputeProvider, cfg compute.ComputeProviderConfig) error {
		return p.InvokeWorker(ctx, rc, cfg)
	})
	return &workercontrollerservice.InvokeWorkerResponse{Failure: failure}, nil
}

func (s *Server) UpdateWorkerSetSize(ctx context.Context, req *workercontrollerservice.UpdateWorkerSetSizeRequest) (*workercontrollerservice.UpdateWorkerSetSizeResponse, error) {
	rc := RequestContextFromProto(req.GetContext())
	failure := s.call(ctx, rc, req.GetProvider(), func(p compute.ComputeProvider, cfg compute.ComputeProviderConfig) error {
		return p.UpdateWorkerSetSize(ctx, rc, cfg, req.GetWorkerSetSize())
	})
	return &workercontrollerservice.UpdateWorkerSetSizeResponse{Failure: failure}, nil
}

// call returns nil when the provider accepted; every provider outcome is a failure, never an RPC error.
func (s *Server) call(
	ctx context.Context,
	rc compute.RequestContext,
	provider *workercontrollerservice.ComputeProviderConfig,
	fn func(compute.ComputeProvider, compute.ComputeProviderConfig) error,
) *workercontrollerservice.ComputeFailure {
	providerType := iface.ComputeProviderType(provider.GetProviderType())
	p, err := computeprovider.GetComputeProvider(ctx, providerType, rc.NamespaceName, s.dc)
	if err != nil {
		return FailureToProto(err)
	}
	if p == nil {
		return &workercontrollerservice.ComputeFailure{
			FailureClass: workercontrollerservice.FAILURE_CLASS_UNSUPPORTED,
			Message:      fmt.Sprintf("compute provider %q is not enabled for namespace %q", providerType, rc.NamespaceName),
		}
	}
	cfg := compute.ComputeProviderConfig{}
	if err := sdk.PreferProtoDataConverter.FromPayload(provider.GetConfig(), &cfg); err != nil {
		return FailureToProto(compute.NewProviderError(compute.FailureRejected, fmt.Errorf("invalid compute config: %w", err)))
	}
	return FailureToProto(fn(p, cfg))
}

// FailureToProto classifies a provider error; nil stays nil.
func FailureToProto(err error) *workercontrollerservice.ComputeFailure {
	if err == nil {
		return nil
	}
	class := compute.FailureUnclassified
	var pErr *compute.ProviderError
	if errors.As(err, &pErr) {
		class = pErr.Class
	}
	return &workercontrollerservice.ComputeFailure{FailureClass: classToProto[class], Message: err.Error()}
}

// FailureFromProto rebuilds the classified provider error a ComputeFailure stands for.
// An UNSUPPORTED failure has no compute.FailureClass, so the caller maps it before this.
func FailureFromProto(f *workercontrollerservice.ComputeFailure) error {
	if f == nil {
		return nil
	}
	return compute.NewProviderError(classFromProto[f.GetFailureClass()], errors.New(f.GetMessage()))
}

func RequestContextToProto(rc compute.RequestContext) *workercontrollerservice.ComputeRequestContext {
	return &workercontrollerservice.ComputeRequestContext{
		NamespaceId:       rc.NamespaceID,
		NamespaceName:     rc.NamespaceName,
		DeploymentName:    rc.DeploymentName,
		DeploymentBuildId: rc.DeploymentBuildID,
	}
}

func RequestContextFromProto(rc *workercontrollerservice.ComputeRequestContext) compute.RequestContext {
	return compute.RequestContext{
		NamespaceID:       rc.GetNamespaceId(),
		NamespaceName:     rc.GetNamespaceName(),
		DeploymentName:    rc.GetDeploymentName(),
		DeploymentBuildID: rc.GetDeploymentBuildId(),
	}
}

var classToProto = map[compute.FailureClass]workercontrollerservice.FailureClass{
	compute.FailureUnclassified: workercontrollerservice.FAILURE_CLASS_UNSPECIFIED,
	compute.FailureRejected:     workercontrollerservice.FAILURE_CLASS_REJECTED,
	compute.FailureNotFound:     workercontrollerservice.FAILURE_CLASS_NOT_FOUND,
	compute.FailureAccessDenied: workercontrollerservice.FAILURE_CLASS_ACCESS_DENIED,
	compute.FailureUnavailable:  workercontrollerservice.FAILURE_CLASS_UNAVAILABLE,
	compute.FailureThrottled:    workercontrollerservice.FAILURE_CLASS_THROTTLED,
	compute.FailureInternal:     workercontrollerservice.FAILURE_CLASS_INTERNAL,
	compute.FailureConflict:     workercontrollerservice.FAILURE_CLASS_CONFLICT,
}

var classFromProto = func() map[workercontrollerservice.FailureClass]compute.FailureClass {
	m := make(map[workercontrollerservice.FailureClass]compute.FailureClass, len(classToProto))
	for k, v := range classToProto {
		m[v] = k
	}
	return m
}()
