# WCI on CHASM: rollout and reversal

A WCI runs on one of two backends: the WCI workflow (this repo) or the `wci/instance` CHASM archetype
in the server (`chasm/lib/wci`). The client routes between them; WCIs move between backends without
losing scale-ups or spec.

Only in-process providers (`workercontroller.chasm.in_process_compute_providers`, default
`test-invoke`, `test-worker-set`) run on CHASM. A WCI whose spec names any other provider is created
as, or stays, a workflow.

## Gates

All namespace-scoped unless noted; all default off.

| Key | Effect |
|---|---|
| `workercontroller.chasm.routing_enabled` | Calls try CHASM first, then the workflow. Required by every gate below |
| `workercontroller.chasm.creation_enabled` + `creation_rollout_percent` | New WCIs are created on CHASM. Percent is keyed on deployment name + build ID |
| `workercontroller.chasm.migration_enabled` + `migration_rollout_percent` | Workflow WCIs move to CHASM at their next continue-as-new. Also needs `workercontroller.instanceWorkflowVersion` ≥ 5 |
| `workercontroller.chasm.workflow_task_queue` (global) | Queue a WCI migrated back to the workflow starts on. Must match the WCI client's queue: cloud `temporal-sys-worker-controller-per-ns-tq`, OSS `temporal-sys-per-ns-tq` |

## Rollout

Per namespace, in order; watch the [metrics](#metrics) between steps.

1. `routing_enabled: true`. No behaviour change: every WCI is still a workflow, so the router falls
   through. Expect `worker_controller_client_requests{backend="workflow"}` only.
2. `creation_enabled: true`, `creation_rollout_percent` 1 → 10 → 100. New WCIs appear with
   `backend="chasm"`.
3. `instanceWorkflowVersion: 5`, wait until running WCIs have continued-as-new onto it.
4. `migration_enabled: true`, `migration_rollout_percent` 1 → 10 → 100. Each WCI moves at its next
   continue-as-new (any spec update forces one). Watch
   `worker_controller_instance_chasm_migration_count{migration_outcome}`.

## Reversal

Per WCI, CHASM → workflow:

1. Close the migration gate for the WCI (`migration_enabled: false` or lower the percent),
   otherwise the new workflow migrates back at its first continue-as-new.
2. Call `MigrateToWorkflow` on history for the WCI (operator command: see SRV `tdbg`, step 6d).
   The CHASM execution stops accepting work, starts the workflow from its state, and closes as
   MIGRATED; the router falls through to the workflow.
3. Check `worker_controller_instance_chasm_migrate_to_workflow_count{migration_outcome}` and that the
   WCI workflow is running.

Whole namespace: close creation and migration gates, migrate every CHASM WCI back, then turn
`routing_enabled` off. Turning routing off first strands CHASM WCIs: they keep running but
nothing reaches them, and the next spec update recreates the WCI as a workflow.

## Metrics

CHASM emits the workflow's metric names and tags plus `backend="chasm"`; workflow emissions carry no
`backend` tag (`backend=""` in PromQL). Names below are as defined; the exported form depends on the
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

- **Instance limit:** `workercontroller.maxInstances` counts workflow WCIs only.
- **Task-adds during workflow → CHASM:** signals that arrive after the CHASM create and before the
  workflow completes are dropped; the next metrics poll covers them.
- **Task-adds during CHASM → workflow:** rejected with `ErrMigrationPending` while migrating and
  dropped by the hook; same recovery.
- **Provider change on a CHASM WCI:** updating an existing CHASM WCI to a provider CHASM doesn't
  support fails; migrate it back first.
- **Region IDs** are not carried across migration.
