// Command modalproto is a throwaway prototype client that creates a Worker
// Deployment Version whose compute config selects the "modal" provider, by
// calling the CreateWorkerDeploymentVersion gRPC method directly. It exists
// because there is no CLI/UI yet for authoring a Modal compute config.
package main

import (
	"context"
	"flag"
	"log"
	"time"

	"github.com/google/uuid"
	computepb "go.temporal.io/api/compute/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/server/common/sdk"
)

func main() {
	var (
		address     = flag.String("address", "localhost:7233", "Temporal frontend gRPC address")
		namespace   = flag.String("namespace", "default", "namespace")
		deployment  = flag.String("deployment", "modal-demo", "worker deployment name (== Modal app name)")
		buildID     = flag.String("build", "v1", "build ID (== Modal function name)")
		environment = flag.String("environment", "", "Modal environment (optional)")
	)
	flag.Parse()

	// The provider derives the Modal app (deployment name) and function (build ID) from
	// the WDV, so only optional overrides go in the details payload.
	details := map[string]string{}
	if *environment != "" {
		details["environment"] = *environment
	}
	detailsPayload, err := sdk.PreferProtoDataConverter.ToPayload(details)
	if err != nil {
		log.Fatalf("failed to encode provider details: %v", err)
	}

	c, err := sdkclient.Dial(sdkclient.Options{HostPort: *address, Namespace: *namespace})
	if err != nil {
		log.Fatalf("failed to dial %s: %v", *address, err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = c.WorkflowService().CreateWorkerDeploymentVersion(ctx, &workflowservice.CreateWorkerDeploymentVersionRequest{
		Namespace:         *namespace,
		DeploymentVersion: &deploymentpb.WorkerDeploymentVersion{DeploymentName: *deployment, BuildId: *buildID},
		Identity:          "modalproto",
		RequestId:         uuid.NewString(),
		ComputeConfig: &computepb.ComputeConfig{
			ScalingGroups: map[string]*computepb.ComputeConfigScalingGroup{
				"default": {
					Provider: &computepb.ComputeProvider{Type: "modal", Details: detailsPayload},
					Scaler:   &computepb.ComputeScaler{Type: "no-sync"},
				},
			},
		},
	})
	if err != nil {
		log.Fatalf("CreateWorkerDeploymentVersion failed: %v", err)
	}

	log.Printf("created worker deployment version %s.%s with modal provider (modal app=%s function=%s)",
		*deployment, *buildID, *deployment, *buildID)
}
