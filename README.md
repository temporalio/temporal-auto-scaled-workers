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
| AWS AgentCore | `aws-agentcore` | Invoke (one-off) |
| AWS ECS | `aws-ecs` | Worker set (managed scaling) |
| GCP Cloud Run | `gcp-cloud-run` | Worker set |
| Modal Functions | `modal-function` | Invoke (one-off) |
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
| `WorkerControllerModalCredentialReloadLimit` | `2` | Secret re-reads allowed for a grant Modal keeps rejecting |
| `WorkerControllerModalCredentialReloadCooldownSeconds` | `900` | How long to stop reading secret storage for a grant after that |

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

### `modal-function` compute provider config

| Key | Required | Description |
|---|---|---|
| `app_name` | yes | Deployed Modal App name |
| `function_name` | yes | Function within the App to spawn |
| `environment` | yes | Modal environment the App is deployed in |
| `workspace_id` | only with OAuth | Modal workspace. Must be one the namespace's account has connected |
| `version` | no | Pins the lookup to a deployed App version; omitted uses the current deployment |

Modal Functions are invoke-based, so it pairs with the `no-sync` algorithm and takes the same scaling options as AWS Lambda.

#### Credentials

By default WCI leaves credentials to the Modal SDK, which reads `MODAL_TOKEN_ID` and `MODAL_TOKEN_SECRET` from the environment or `~/.modal.toml` — the same way the AWS and GCP providers leave their SDKs to find theirs. Nothing else is needed, and `workspace_id` is not required because the key already names the workspace.

A deployment using Modal OAuth configures a data plane config bucket (see below). WCI then authenticates as the Modal grant belonging to the account that owns the namespace, derived server-side. In that mode `workspace_id` selects which of that account's connected workspaces to use, and is required. Either way the config cannot carry a credential or name another account: no credential is ever read from it, and the account half of the lookup comes from the namespace. The signing key and refresh token are read on a grant's first use and cached in process memory; they never enter workflow or activity history.

A steady-state invocation costs one `Spawn`: the Modal connection is kept per grant and the resolved Function handle per `(app, function, version)`, so neither the connection setup nor the `FunctionGet` lookup repeats. Validation shares none of this — it re-reads the credentials, builds its own connection and resolves the Function itself, so a check reflects what the bucket and Modal hold now rather than what a previous spawn cached. A grant the control plane has removed fails the check immediately, instead of passing on a copy that would survive only until the pod restarts. A handle is dropped if a spawn fails against it, so the next invocation resolves a fresh one.

#### Sealed material in the data plane config store (optional)

Configuring a cloud provider switches the provider from one process-wide API key to per-account grants. WCI then authenticates to Modal with an OAuth client assertion (RFC 7523), built from two objects in its own region's data plane config bucket:

| What | Object |
|---|---|
| RSA signing key | `<prefix>/signing-key.json` |
| Refresh token | `<prefix>/accounts/<account id>/<workspace id>.json` |

There is no Vault client here, deliberately. A Vault client is billed per client and a cell would need its own; the material a cell needs is already projected to a bucket the cell can read. The control plane remains the system of record and the only writer.

Both objects are **sealed**, so bucket access alone yields nothing. The control plane encrypts a fresh AES-GCM data key to the public half of a cloud KMS key — public, so it travels as ordinary control plane config and the control plane makes no KMS call and cannot read back what it writes — and WCI asks that cloud's KMS to unwrap it, authorized by the identity the pod already has. The AEAD's additional data binds a grant's ciphertext to its account and workspace, so an object moved into another grant's path fails to open rather than yielding the wrong customer's token, and the decrypted payload repeats both ids so a reader that got the path and the AAD wrong the same way still notices. `SealingKeyID` is checked against the key the object names, by prefix so GCP key rotation keeps working.

The signing key is WCI-owned and shared by every grant in the region, so it is read once for the life of the process; rotating it takes a restart. That is the trade for never having to guess which half Modal rejected: the key was good when it was loaded, so a rejection is the refresh token's, and only the token is re-read. The OAuth client ID is not secret, so it is deployment config rather than something read from the bucket; the sealed grant records the client it was issued to and is refused if the two disagree.

Which bucket a cell reads is **deployment configuration, not dynamic config** — a cell reads its own region and nobody else's, so it changes when the cell does. The deployment installs a `computeprovider.ModalStoreConfig` through fx at startup (`SetModalStoreConfig`); leaving the cloud provider unset keeps the Modal API key default. If one is set but the source cannot be built, the provider fails closed rather than falling back to ambient credentials, which would serve every account from one key.

| Field | Example |
|---|---|
| `CloudProvider` | `aws` or `gcp` |
| `Region` | `us-west-2` |
| `Bucket` | `data-plane-config-test-us-west-2` |
| `AssumeRoleARN` | `arn:aws:iam::730335288748:role/data-plane-config-test-us-west-2-reader` (AWS only) |
| `ObjectPrefix` | `serverless-modal-grants` (the default) |
| `OAuthClientID` | `oc-xxxxxxxx` |
| `SealingKeyID` | the KMS key ARN, or the GCP cryptoKey without its version |

Azure is absent on purpose: the control plane holds no sealing key for it and skips those regions, so a cell there has nothing to read.

To check a region is wired up without deploying anything, `cmd/modal-secret-probe` resolves a grant through exactly this code path and reports what it found without printing any of it:

```
go run ./cmd/modal-secret-probe \
  -targets aws/us-east-1,aws/us-west-2,gcp/us-central1,gcp/us-west1 \
  -account <account id> -workspace <workspace id> -client-id oc-xxxxxxxx
```

Only the runtime tuning knobs in the table above stay in dynamic config.

A group with no `task_types` acts as a catch-all for any task type not claimed by another group. At most one catch-all group is allowed. The `scaling` block is optional; omitting it leaves the group with the default scaling configuration for the given compute provider.

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
