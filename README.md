# Temporal Auto-Scaled Workers

Automatically scale Temporal workers in response to workload. This project implements a **Worker Controller Instance (WCI)** — a long-running Temporal workflow that monitors task queue metrics and scales workers across cloud compute providers.

## Overview

Each WCI manages a single deployment version (deployment name + build ID). It:

1. Receives task-add signals from the Temporal Matching Service
2. Periodically polls task queue backlog and dispatch metrics
3. Applies a configurable scaling algorithm to decide when to act
4. Invokes workers on the configured compute provider

Multiple **scaling groups** can be defined per WCI, each mapping a set of task queue types (workflow, activity, nexus), and optionally regions, to a compute provider and scaling algorithm. A group without task types acts as a catch-all, and a group scoped to regions is only used by a worker controller hosted in one of them.

## Supported Compute Providers

| Provider | Type string | Launch strategy |
|---|---|---|
| AWS Lambda | `aws-lambda` | Invoke (one-off) |
| AWS AgentCore | `aws-agentcore` | Invoke (one-off) |
| AWS ECS | `aws-ecs` | Worker set (managed scaling) |
| GCP Cloud Run | `gcp-cloud-run` | Worker set |
| Kubernetes | `k8s` | Worker set |
| Subprocess | `subprocess` | Invoke (dev/test only) |

**Invoke** providers are called once per scaling event to start a short-lived worker.
**Worker-set** providers manage a persistent pool whose size is adjusted up or down.

The development-only subprocess provider accepts an optional positive Go duration string as `config.timeout` (for example, `"30m"` or `"1h"`). It defaults to `"1m"`.

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

## Spec Format

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
    "workflows-west": {
      "task_types": ["WORKFLOW"],
      "region_ids": ["aws-us-west-2"],
      "compute": {
        "provider_type": "aws-lambda",
        "config": {
          "arn": "arn:aws:lambda:us-west-2:123456789012:function:my-worker",
          "role": "arn:aws:iam::123456789012:role/temporal-wci",
          "role_external_id": "my-external-id"
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

Each group covers a set of `task_types`, or none to act as a catch-all, and optionally `region_ids`.
A group with `region_ids` is only used by a worker controller hosted in one of those regions, which in a replicated namespace is where the namespace is active; groups without them apply in any region.
In the example above, workflow tasks use the `us-west-2` Lambda while the namespace is active in `aws-us-west-2`, and the `us-east-1` Lambda anywhere else.
The hosting binary supplies its region by providing a `*hostconfig.Config` through fx; without a well-formed one, specs that set `region_ids` are rejected.
Region IDs use the form `<provider>-<region>` in lowercase letters, digits and single hyphens, e.g. `aws-us-east-1`.
The OSS Temporal server doesn't supply a region, so region-scoped groups are only available where the hosting binary does, such as Temporal Cloud.

For each task type, the serving group is the first match in this order:

1. A group whose `region_ids` include this host's region and that lists the task type
2. The catch-all group whose `region_ids` include this host's region
3. A group without `region_ids` that lists the task type
4. The catch-all group without `region_ids`

Within each region, and among groups without `region_ids` (including the catch-all group without any region set), a task type may appear in at most one group and at most one group may be a catch-all. A catch-all must serve at least one task type where it applies, so a spec whose other groups list workflow, activity and nexus everywhere the catch-all applies is rejected. The `scaling` block is optional; omitting it leaves the group with the default scaling configuration for the given compute provider.

### `no-sync` algorithm config

| Key | Default | Description |
|---|---|---|
| `scale_up_backlog_threshold` | `0` | Scale up when backlog exceeds this value |
| `scale_up_cooloff_ms` | `100` | Minimum milliseconds between scale-up actions |
| `max_worker_lifetime_ms` | `600000` | Re-invoke workers at least this often (10 min) |
| `scale_up_dispatch_rate_epsilon` | `0` | Relative band (a fraction of the dispatch rate, capped at `0.10`); `0` disables it. When set, a queue type's scale-up is suppressed on both the metrics-poll and task-add paths while its dispatch rate holds within this band under a backlog above the scale-up threshold. Worker-lifetime refresh is not suppressed. |
| `scale_up_dispatch_rate_epsilon_confirm_ms` | `90000` | How long the dispatch rate must stay within the band before scale-up is suppressed. Must be greater than `metrics_poll_interval_ms` when `scale_up_dispatch_rate_epsilon > 0`. |
| `suppress_scale_up_ms` | `120000` | How long each confirming poll suppresses scale-up for. Must be greater than `suppress_poll_interval_ms` when `scale_up_dispatch_rate_epsilon > 0`. |
| `suppress_poll_interval_ms` | `90000` | Metrics poll interval while any queue type is suppressed. |
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

## Building

```bash
# Build
make bins

# Run tests
make test
```

## Run Together with a Local Temporal Server

1. Check out the [Temporal Server](https://github.com/temporalio/temporal) alongside this repository.
2. Link the two repositories using either a [Go Workspace](https://go.dev/doc/tutorial/workspaces) or a [`replace` directive](https://go.dev/ref/mod#go-mod-file-replace) in the server's `go.mod`.
3. Compile the Temporal Server: `make bins` (or `make all`).
4. Start the server: `make start` — this uses the SQLite in-memory backend and runs `temporal-auto-scaled-workers` as part of the system workers.

## Scaling Algorithm Simulators

Interactive simulators for the scaling algorithms are available in [`docs/simulators/`](docs/simulators/).

## License

[MIT](LICENSE)
