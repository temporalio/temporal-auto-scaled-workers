# No-Sync Scaling Algorithm Spec

This document specifies the behavior of `scalingAlgorithmNoSync`, the `no-sync`
scaling algorithm used for invoke-based compute providers.

## Purpose

The algorithm invokes one new worker when the system observes evidence that
existing workers are not keeping up. It has two scale-up paths:

- Task-add signals from Matching, including direct no-sync matches and batched
  no-sync matches.
- Periodic task-queue metric polls, based on backlog and worker lifetime
  refresh, with an optional per-queue flat-dispatch-rate detector that gates
  scale-up at a downstream/rate-limit ceiling.

The algorithm does not scale down and does not set a target worker-set size.

## Configuration

The algorithm accepts only these config keys. Unknown keys are invalid.

| Key | Type | Default | Validation | Meaning |
| --- | --- | ---: | --- | --- |
| `scale_up_cooloff_ms` | integer | `100` | `>= 0` | Minimum elapsed time between scale-up actions on the backlog-threshold path and task-add path. `0` disables cooloff. |
| `scale_up_backlog_threshold` | integer | `0` | `>= 0` | Periodic metric polls request scale-up only when backlog is strictly greater than this threshold. |
| `max_worker_lifetime_ms` | integer | `600000` | `>= 0` | Periodic metric polls request worker refresh when backlog is present and this much time has elapsed since the last scale-up. `0` disables this path. |
| `scale_up_dispatch_rate_epsilon` | number | `0` | `>= 0`, and `<= 0.10` when `> 0` | Enables the flat-dispatch-rate detector. `0` disables it. When `> 0` it is a **relative band** (a fraction of the dispatch rate): a queue's dispatch rate must stay within `epsilon * reference_rate` to count as flat. |
| `metrics_poll_interval_ms` | integer | `60000` | `>= 10000` | Requested interval before the next periodic metric poll. |
| `flat_dispatch_rate_confirm_ms` | integer | `45000` | `> 0` when `epsilon > 0` | A queue's dispatch rate must stay flat under a material backlog at least this long before suppression engages (≥ one dispatch-rate averaging window). |
| `suppress_scale_up_ms` | integer | `120000` | `> suppress_poll_interval_ms` when `epsilon > 0` | Lifetime (TTL) of the suppression lease. Each flat poll renews it; if polling stops it self-expires after this. |
| `suppress_poll_interval_ms` | integer | `90000` | `> 0` when `epsilon > 0` | Poll cadence while actively suppressing. Deliberately longer than `metrics_poll_interval_ms`. |

Cross-field validation:

- If `scale_up_cooloff_ms > 0`, `metrics_poll_interval_ms` must be greater than
  or equal to `scale_up_cooloff_ms`.
- If `scale_up_cooloff_ms == 0`, the cross-field check is skipped.

When `scale_up_dispatch_rate_epsilon > 0`:

- `scale_up_dispatch_rate_epsilon` must be `<= 0.10` (a relative fraction).
- `flat_dispatch_rate_confirm_ms` must be `> 0`.
- `suppress_poll_interval_ms` must be `> 0`.
- `suppress_scale_up_ms` must be `> suppress_poll_interval_ms`, so the lease
  never lapses between polls.

## State

The algorithm persists these state keys:

| Key | Type | Meaning |
| --- | --- | --- |
| `last_scale_up_time_ms` | integer | Unix epoch milliseconds for the last emitted scale-up action. Shared by both decision paths. |
| `<queue>_dispatch_flat_since_ms` | integer | When that queue's dispatch rate first went flat under a material backlog (`0` = not flat). |
| `<queue>_dispatch_ref_rate` | number | Dispatch rate anchored when flat began (`-1` = none). |
| `<queue>_suppress_scale_up_until_ms` | integer | Suppression lease: that queue's scale-up is gated while `now_ms < ` this value. Read by both scaling paths. |

`<queue>` is the task queue type (`workflow`, `activity`, `nexus`); activity is the only rate-limited type today, but the detector runs per type. These keys are written only when the detector is enabled
(`epsilon > 0`); a poll with the detector disabled deletes them. Unknown state
keys are removed from the returned state.

Nil config is treated as an empty config. Nil prior state is treated as an empty
state.

## Task-Add Signal Inputs

`ProcessTaskAdd` receives one `SignalTaskAddRequest` with:

- `IsSyncMatch`
- `NoSyncMatchSignalsSinceLast`
- `TaskQueueType` (workflow, activity, or nexus)

## Task-Add Signal Decision

A task-add signal is eligible for scale-up when either condition is true:

- `IsSyncMatch == false`
- `NoSyncMatchSignalsSinceLast > 0`

If neither condition is true, the algorithm returns no action.

For an eligible signal:

```text
elapsed = now_ms - prior_state.last_scale_up_time_ms

if now_ms < prior_state.<TaskQueueType>_suppress_scale_up_until_ms:
  # obey the poll's per-queue lease (only rate-limited types ever get one)
  emit no action
  set throttled_count = NoSyncMatchSignalsSinceLast
elif elapsed >= scale_up_cooloff_ms:
  emit one invoke-worker action
  set last_scale_up_time_ms = now_ms
else:
  emit no action
  set throttled_count = NoSyncMatchSignalsSinceLast
```

When `scale_up_cooloff_ms == 0`, every eligible signal may emit a scale-up
unless that queue's suppression lease gates it.

## Metrics Poll Inputs

`ProcessMetricsPoll` receives a metrics snapshot for the queue with:

- `LastBacklogCount`
- `LastArrivalRate`
- `LastProcessingRate`

The no-sync algorithm does not use `LastArrivalRate`.

If queue metrics are absent, the poll returns no scale-up action and still
returns the configured next-poll interval.

## Metrics Poll Decision

The poll receives metrics for each configured queue type (workflow, activity,
nexus). On every metrics poll, the algorithm:

1. Runs the flat-dispatch-rate detector (below) for each queue type, persisting
   each queue's verdict and returning whether that queue's scale-up is suppressed.
2. Sets `NextPoll` to `suppress_poll_interval_ms` when suppressing, else
   `metrics_poll_interval_ms`.
3. Computes `elapsed_since_scale_up` from the prior `last_scale_up_time_ms`.
4. Evaluates each queue and emits at most one `invoke-worker` action for the
   whole poll (OR-ed across queue types).
5. Updates `last_scale_up_time_ms` only if it emits a scale-up.

Per-queue decision (OR-ed across queue types):

```text
scale_up = false

for q in [workflow, activity, nexus] with metrics present:
  candidate = false

  if q.LastBacklogCount > scale_up_backlog_threshold
     and elapsed_since_scale_up >= scale_up_cooloff_ms:
    candidate = true            # growth

  if candidate and q_suppressed:
    candidate = false           # gate this queue's growth at its ceiling

  if candidate == false
     and max_worker_lifetime_ms > 0
     and q.LastBacklogCount > 0
     and elapsed_since_scale_up >= max_worker_lifetime_ms:
    candidate = true            # lifetime maintenance -- never gated

  scale_up = scale_up or candidate
```

If `scale_up == true`, the poll emits one `invoke-worker` action and sets
`last_scale_up_time_ms = now_ms`.

## Backlog Threshold Path

The backlog threshold path is guarded by both backlog and cooloff:

```text
LastBacklogCount > scale_up_backlog_threshold
elapsed_since_scale_up >= scale_up_cooloff_ms
```

The threshold comparison is strict. A backlog exactly equal to
`scale_up_backlog_threshold` does not trigger this path.

With the default threshold of `0`, any positive backlog is sufficient after the
cooloff has elapsed.

## Worker Lifetime Refresh Path

The lifetime refresh path can fire only when the backlog-threshold path did not
already select the queue for scale-up in the current poll.

It is guarded by:

```text
max_worker_lifetime_ms > 0
LastBacklogCount > 0
elapsed_since_scale_up >= max_worker_lifetime_ms
```

This path uses `max_worker_lifetime_ms` as its elapsed-time threshold, not
`scale_up_cooloff_ms`. It can therefore emit a scale-up while the cooloff window
would still suppress the backlog-threshold path.

Any scale-up from either task-add signals or metrics polls resets the shared
lifetime timer by updating `last_scale_up_time_ms`.

## Flat-Dispatch-Rate Detector

When `scale_up_dispatch_rate_epsilon > 0`, each metrics poll runs this detector
**per queue type**. It confirms a queue's dispatch rate staying flat under a
material backlog (a downstream or rate-limit ceiling that adding workers cannot
lift), then persists a per-queue suppression lease that gates that queue's growth
on both the poll path and the task-add fast path. When `epsilon <= 0` the detector
is disabled: the per-queue keys are deleted and the poll reverts to baseline.

Activity queues are the only ones that can be rate-limited today, but the detector
works for any queue type -- a non-rate-limited queue's dispatch rises as workers
are added, so it moves out of the band and never confirms.

Per queue `q`, inputs from its metrics: `rate = LastProcessingRate`,
`backlog = LastBacklogCount`. Let `ref` be `<q>_dispatch_ref_rate`.

```text
material = backlog > scale_up_backlog_threshold
band     = scale_up_dispatch_rate_epsilon * ref     # relative band
moved    = ref >= 0 and abs(rate - ref) > band

if not material or moved or rate <= 0:
  # no material backlog, dispatch moved off the reference, or zero throughput
  # (a stall, not a flat plateau) -> resume: clear the verdict
  <q>_dispatch_flat_since_ms     = 0
  <q>_dispatch_ref_rate          = -1
  <q>_suppress_scale_up_until_ms = 0
elif <q>_dispatch_flat_since_ms == 0:
  # flat + material backlog -> start confirming; anchor the reference rate
  <q>_dispatch_flat_since_ms = now_ms
  <q>_dispatch_ref_rate      = rate
elif now_ms - <q>_dispatch_flat_since_ms >= flat_dispatch_rate_confirm_ms:
  # confirmed flat under backlog -> (re)new the suppression lease
  <q>_suppress_scale_up_until_ms = now_ms + suppress_scale_up_ms

q_suppressed = <q>_suppress_scale_up_until_ms > now_ms
```

Notes:

- The reference rate is anchored once when flatness begins and is compared
  against on every subsequent poll; a move beyond `band` in either direction
  resumes scale-up.
- Zero throughput under a backlog is treated as a stall (recover), never as a
  flat plateau to suppress.
- Only a suppressed queue's growth is gated. Lifetime maintenance on every queue
  is never gated.
- The lease is consulted by `ProcessTaskAdd` for that queue's task-adds, so the
  metrics-blind fast path obeys the same ceiling.

## Shared Cooloff and State

`last_scale_up_time_ms` is shared:

- Across task-add signal decisions and metrics-poll decisions.
- Across backlog-threshold scale-ups and worker lifetime refresh scale-ups.

Consequences:

- A task-add scale-up can suppress a later metrics-poll backlog-threshold
  scale-up until cooloff elapses.
- A metrics-poll scale-up can suppress a later task-add scale-up until cooloff
  elapses.
- A metrics poll emits at most one action.

## Outputs

`ProcessTaskAdd` returns:

- `Actions`: empty or one `invoke-worker` action.
- `Status`: updated algorithm state.
- `ThrottledCount`: batched no-sync count suppressed by cooloff.

`ProcessMetricsPoll` returns:

- `Actions`: empty or one `invoke-worker` action.
- `Status`: updated algorithm state.
- `NextPoll`: `metrics_poll_interval_ms`, or `suppress_poll_interval_ms` while
  any queue's suppression lease is active.

The caller is responsible for adding the scaling group key to emitted actions,
executing the invoke request, persisting returned status, and applying any
global poll interval clamping.

## Non-Goals

The algorithm intentionally does not:

- Scale down.
- Compute desired worker count.
- Emit `update-worker-set-size`.
- Use arrival rate.
- Use per-worker capacity or slot utilization.
