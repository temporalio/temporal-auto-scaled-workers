# Temporal Auto-Scaled Workers

Automatically scale Temporal workers in response to workload. This project implements a **Worker Controller Instance (WCI)** — a long-running Temporal workflow that monitors task queue metrics and scales workers across cloud compute providers.

## Overview

Each WCI manages a single deployment version (deployment name + build ID). It:

1. Receives task-add signals from the Temporal Matching Service
2. Periodically polls task queue backlog and dispatch metrics
3. Applies a configurable scaling algorithm to decide when to act
4. Invokes workers on the configured compute provider

Multiple **scaling groups** can be defined per WCI, each mapping a set of task queue types (workflow, activity, nexus) to a compute provider and scaling algorithm. One group can act as a catch-all for task types not claimed by other groups.

## Supported Compute Providers

| Provider | Type string | Launch strategy |
|---|---|---|
| AWS Lambda | `aws-lambda` | Invoke (one-off) |
| AWS ECS | `aws-ecs` | Worker set (managed scaling) |
| GCP Cloud Run | `gcp-cloud-run` | Worker set |
| Kubernetes | `k8s` | Worker set |
| Subprocess | `subprocess` | Invoke (dev/test only) |

**Invoke** providers are called once per scaling event to start a short-lived worker.
**Worker-set** providers manage a persistent pool whose size is adjusted up or down.

## Supported Scaling Algorithms

| Algorithm | Type string | Description |
|---|---|---|
| No-sync | `no-sync` | Scales up when backlog or arrival rate exceeds thresholds; cools down between invocations |

## Configuration

### Dynamic config

| Setting | Default | Description |
|---|---|---|
| `WorkerControllerEnabled` | `false` | Enable WCI per namespace |
| `WorkerControllerMaxInstances` | `100` | Max WCIs per namespace |
| `WorkerControllerEnabledComputeProviders` | all | Allowed compute provider types |
| `WorkerControllerEnabledScalingAlgorithms` | all | Allowed scaling algorithm types |
| `WorkerControllerAWSIntermediaryRoles` | `[]` | IAM role chain for AWS STS |
| `WorkerControllerGCPIntermediaryServiceAccounts` | `[]` | Service account chain for GCP |
| `WorkerControllerAWSRequireRoleAndExternalID` | `true` | Enforce role + external ID on AWS configs |

## Spec format

A WCI spec is a map of named scaling groups:

```json
{
  "scaling_group_specs": {
    "workflows": {
      "task_types": ["WORKFLOW"],
      "compute": {
        "provider_type": "aws-lambda",
        "config": {
          "arn": "arn:aws:lambda:us-east-1:123456789012:function:my-worker",
          "role": "arn:aws:iam::123456789012:role/temporal-wci",
          "role_external_id": "my-external-id"
        }
      },
      "scaling": {
        "scaling_algorithm": "no-sync",
        "config": {
          "scale_up_backlog_threshold": "5",
          "scale_up_cooloff_ms": "500",
          "max_worker_lifetime_ms": "300000"
        }
      }
    },
    "activities": {
      "task_types": ["ACTIVITY", "NEXUS"],
      "compute": {
        "provider_type": "aws-ecs",
        "config": {
          "cluster": "my-cluster",
          "service": "my-worker-service",
          "region": "us-east-1",
          "role": "arn:aws:iam::123456789012:role/temporal-wci"
        }
      }
    }
  }
}
```

A group with no `task_types` acts as a catch-all for any task type not claimed by another group. At most one catch-all group is allowed.

### `no-sync` algorithm config

| Key | Default | Description |
|---|---|---|
| `scale_up_backlog_threshold` | `0` | Scale up when backlog exceeds this value |
| `scale_up_cooloff_ms` | `100` | Minimum milliseconds between scale-up actions |
| `max_worker_lifetime_ms` | `600000` | Re-invoke workers at least this often (10 min) |
| `scale_up_dispatch_rate_epsilon` | `0` | Suppress scale-up if dispatch rate is stable within this margin |
| `metrics_poll_interval_ms` | `60000` | How often to poll task queue metrics |

## Client API

```go
import "go.temporal.io/auto-scaled-workers/wci/client"

// Register the Fx module in your server
fx.Provide(client.ClientProvider)

// Use the Client interface
type MyComponent struct {
    wciClient client.Client
}

// Create or update a WCI
err := wciClient.UpdateWorkerControllerInstance(ctx, ns, deploymentVersion, &client.UpdateWorkerControllerInstanceRequest{
    Spec: &client.Spec{
        ScalingGroupSpecs: map[string]client.ScalingGroupSpec{
            "default": {
                Compute: client.ComputeProviderSpec{
                    ProviderType: "aws-lambda",
                    Config:       map[string]string{"arn": "..."},
                },
            },
        },
    },
})

// List all WCIs
resp, err := wciClient.ListWorkerControllerInstances(ctx, ns, pageSize, nextPageToken)

// Delete a WCI
err := wciClient.DeleteWorkerControllerInstance(ctx, ns, deploymentVersion, conflictToken)
```

All mutating operations accept a `ConflictToken` for optimistic concurrency control. Obtain it from `DescribeWorkerControllerInstance` and pass it with updates to detect concurrent modifications.

## Integration with Temporal Server

The `TaskHookFactory` returned by `ClientProvider` must be registered with the Temporal Matching Service. It intercepts task-add events and signals the appropriate WCI workflow.

Enable per namespace via the `WorkerControllerEnabled` dynamic config setting.

## Building and running

```bash
# Build
make bins

# Run with SQLite (development)
make start

# Run tests
make test
```

The binary accepts a `--config-file` flag:

```bash
./temporal-auto-scaled-workers --config-file config/development-sqlite.yaml start
```

## Scaling algorithm simulators

Interactive simulators for the scaling algorithms are available in [`docs/simulators/`](docs/simulators/).

## License

[MIT](LICENSE)
