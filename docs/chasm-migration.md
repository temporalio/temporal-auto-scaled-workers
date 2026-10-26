# WCI on CHASM: rollout and reversal

A WCI runs on one of two backends: the WCI workflow (this repo) or the `wci.instance` CHASM archetype
in the server (`chasm/lib/wci`). The client routes between them; WCIs move between backends without
losing scale-ups or spec.

A CHASM WCI calls its provider either inside history (`workercontroller.chasm.in_process_compute_providers`,
default `test-invoke`, `test-worker-set`) or on the WCI process through `WorkerControllerComputeService`
(`workercontroller.chasm.remote_compute_providers`, default empty). Both are namespace-scoped; in-process takes
precedence. A WCI whose spec names a provider in neither list is created as, or stays, a workflow.

## Gates

All namespace-scoped unless noted; the gates default off.

| Key | Effect |
|---|---|
| `workercontroller.chasm.routing_enabled` | Calls try CHASM first, then the workflow. Required by every gate below |
| `workercontroller.chasm.creation_enabled` + `creation_rollout_percent` | New WCIs are created on CHASM. Percent is keyed on deployment name + build ID |
| `workercontroller.chasm.migration_enabled` + `migration_rollout_percent` | Workflow WCIs move to CHASM at their next continue-as-new. Also needs `workercontroller.instanceWorkflowVersion` ≥ 5 |
| `workercontroller.chasm.migration_manual_only` | Turns automatic migration off whatever `migration_enabled` / `migration_rollout_percent` say; `migrate-to-chasm` still works. Guards a namespace against cell-wide migration settings |
| `workercontroller.chasm.migration_pending_ttl` (global, 10m) | How long a pending migration target waits for activation before aborting itself. Must exceed the source's activation retries |
| `workercontroller.chasm.workflow_task_queue` (global) | Queue a WCI migrated back to the workflow starts on. Must match the WCI client's queue: cloud `temporal-sys-worker-controller-per-ns-tq`, OSS `temporal-sys-per-ns-tq` |

## Rollout

Per namespace, in order; watch the [metrics](#metrics) between steps.

1. `routing_enabled: true`. No behaviour change: every WCI is still a workflow, so the router falls
   through. Expect `worker_controller_client_requests{backend="workflow"}` only.
2. `creation_enabled: true`, `creation_rollout_percent` 1 → 10 → 100. New WCIs appear with
   `backend="chasm"`.
3. `instanceWorkflowVersion: 5`, wait until running WCIs have continued-as-new onto it.
4. `migration_enabled: true`, `migration_rollout_percent` 1 → 10 → 100. Each WCI moves at its next
   continue-as-new (any spec update forces one; task-adds and polls don't). Watch
   `worker_controller_instance_chasm_migration_count{migration_outcome}`.

### High-traffic namespaces

Move WCIs one at a time instead of by percent:

1. `routing_enabled: true` and `migration_manual_only: true` for the namespace (set manual-only
   before any cell-wide `migration_enabled` / percent reaches it). `instanceWorkflowVersion: 5` as above.
2. Per WCI: `tdbg -n <namespace> worker-controller migrate-to-chasm --deployment-name <name> --build-id <id>`.
   It returns once the workflow accepts the request; the handover runs at the continue-as-new it
   forces. It is rejected if routing is off or the run predates version 5, and fails `NotFound` if no
   workflow runs the WCI (already on CHASM). A failed handover isn't retried; run it again.
3. Check `worker_controller_instance_chasm_migration_count{migration_outcome}` and that the WCI
   answers with `backend="chasm"` before moving the next one.

## Reversal

Per WCI, CHASM → workflow:

1. Close the migration gate for the WCI (`migration_enabled: false`, lower the percent, or
   `migration_manual_only: true`), otherwise the new workflow migrates back at its first continue-as-new.
2. Run `tdbg -n <namespace> worker-controller migrate-to-workflow --deployment-name <name> --build-id <id>`
   (AdminService `MigrateWorkerControllerInstance`). The CHASM execution stops accepting work,
   starts the workflow from its state, and closes as MIGRATED; the router falls through to the
   workflow. Repeating it while the migration is in progress is a no-op; on an already-migrated
   WCI it fails as closed.
3. Check `worker_controller_instance_chasm_migrate_to_workflow_count{migration_outcome}` and that the
   WCI workflow is running.

Whole namespace: close creation and migration gates, migrate every CHASM WCI back, then turn
`routing_enabled` off. Turning routing off first strands CHASM WCIs: they keep scaling on their own
poll timer but no call reaches them, and the next spec update creates a workflow WCI alongside, so
two controllers scale the same worker set.

## Metrics

CHASM emits the workflow's metric names and tags plus `backend="chasm"`; workflow instance emissions
carry no `backend` tag (`backend=""` in PromQL). `worker_controller_client_requests` is tagged by the
router with `backend="chasm"` or `backend="workflow"`. Names below are as defined; the exported form depends on the
metrics reporter.

| What | Query |
|---|---|
| Who serves client calls | `sum by (backend, operation) (rate(worker_controller_client_requests[5m]))` |
| Scale-ups per backend | `sum by (backend, compute_provider) (rate(worker_controller_instance_scale_up_count[5m]))` |
| Task-adds reaching a WCI | `sum by (backend, skip_reason) (rate(worker_controller_instance_signals{signal_type="task_add"}[5m]))` |
| Provider failures | `sum by (backend, operation, error_type) (rate(worker_controller_instance_operations{error_type!="none"}[5m]))` |
| Backlog | `max by (backend, worker_deployment_name) (worker_controller_instance_backlog_count)` |
| Workflow → CHASM | `sum by (migration_outcome) (rate(worker_controller_instance_chasm_migration_count[5m]))` |
| CHASM → workflow | `sum by (migration_outcome) (rate(worker_controller_instance_chasm_migrate_to_workflow_count[5m]))` |
| Hook failures | `sum (rate(worker_controller_instance_task_match_error_count[5m]))` |

Not emitted on CHASM: `scaling_action_processing_latency`, `activities`, and the registration
invoke/resize done synchronously on update.

## Known gaps

- **Instance limit:** `workercontroller.maxInstances` is enforced only on workflow creation and counts
  workflow WCIs only; CHASM creation is unlimited.
- **Task-adds during workflow → CHASM:** buffered signals are carried with each activate attempt; only
  those reaching the workflow after activation and before the run closes are dropped. The next
  metrics poll covers them.
- **Task-adds during CHASM → workflow:** rejected with `ErrMigrationPending` while migrating and
  dropped by the hook; same recovery.
- **Provider change on a CHASM WCI:** updating an existing CHASM WCI to a provider CHASM doesn't
  support fails; migrate it back first.
- **Region IDs** are not carried across migration.
