# No-Sync Scaling Algorithm Spec

This document specifies the behavior of `scalingAlgorithmNoSync`, the `no-sync`
scaling algorithm used for invoke-based compute providers.

## Purpose

The algorithm invokes one new worker when the system observes evidence that
existing workers are not keeping up. It has two scale-up paths:

- Task-add signals from Matching, including direct no-sync matches and batched
  no-sync matches.
- Periodic task-queue metric polls, based on backlog, worker lifetime refresh,
  and dispatch-rate stability.

The algorithm does not scale down and does not set a target worker-set size.

## Configuration

The algorithm accepts only these config keys. Unknown keys are invalid.

| Key | Type | Default | Validation | Meaning |
| --- | --- | ---: | --- | --- |
| `scale_up_cooloff_ms` | integer | `100` | `>= 0` | Minimum elapsed time between scale-up actions on the backlog-threshold path and task-add path. `0` disables cooloff. |
| `scale_up_backlog_threshold` | integer | `0` | `>= 0` | Periodic metric polls request scale-up only when backlog is strictly greater than this threshold. |
| `max_worker_lifetime_ms` | integer | `600000` | `>= 0` | Periodic metric polls request worker refresh when backlog is present and this much time has elapsed since the last scale-up. `0` disables this path. |
| `scale_up_dispatch_rate_epsilon` | number | `0` | `>= 0` | Periodic metric polls suppress otherwise eligible scale-up when dispatch rate is unchanged within this epsilon. `0` disables suppression. |
| `metrics_poll_interval_ms` | integer | `60000` | `>= 10000` | Requested interval before the next periodic metric poll. |

Cross-field validation:

- If `scale_up_cooloff_ms > 0`, `metrics_poll_interval_ms` must be greater than
  or equal to `scale_up_cooloff_ms`.
- If `scale_up_cooloff_ms == 0`, the cross-field check is skipped.

## State

The algorithm persists these state keys:

| Key | Type | Meaning |
| --- | --- | --- |
| `last_scale_up_time_ms` | integer | Unix epoch milliseconds for the last emitted scale-up action. Shared by both decision paths. |
| `last_dispatch_rate` | number | Last observed dispatch rate from a periodic poll. |

Unknown state keys are removed from the returned state.

Nil config is treated as an empty config. Nil prior state is treated as an empty
state.

## Task-Add Signal Inputs

`ProcessTaskAdd` receives one `SignalTaskAddRequest` with:

- `IsSyncMatch`
- `NoSyncMatchSignalsSinceLast`

## Task-Add Signal Decision

A task-add signal is eligible for scale-up when either condition is true:

- `IsSyncMatch == false`
- `NoSyncMatchSignalsSinceLast > 0`

If neither condition is true, the algorithm returns no action.

For an eligible signal:

```text
elapsed = now_ms - prior_state.last_scale_up_time_ms

if elapsed >= scale_up_cooloff_ms:
  emit one invoke-worker action
  set last_scale_up_time_ms = now_ms
else:
  emit no action
  set throttled_count = NoSyncMatchSignalsSinceLast
```

When `scale_up_cooloff_ms == 0`, every eligible signal may emit a scale-up.

## Metrics Poll Inputs

`ProcessMetricsPoll` receives a metrics snapshot for the queue with:

- `LastBacklogCount`
- `LastArrivalRate`
- `LastProcessingRate`

The no-sync algorithm does not use `LastArrivalRate`.

If queue metrics are absent, the poll returns no scale-up action and still
returns the configured next-poll interval.

## Metrics Poll Decision

On every metrics poll, the algorithm:

1. Sets `NextPoll` to `metrics_poll_interval_ms`.
2. Computes `elapsed_since_scale_up` from the prior `last_scale_up_time_ms`.
3. Evaluates the queue metrics.
4. Emits at most one `invoke-worker` action for the poll if the queue remains
   eligible after all guards.
5. Updates `last_scale_up_time_ms` only if it emits a scale-up.
6. Stores the current dispatch rate, even when no scale-up occurs or scale-up is
   suppressed.

Queue decision:

```text
candidate = false

if LastBacklogCount > scale_up_backlog_threshold
   and elapsed_since_scale_up >= scale_up_cooloff_ms:
  candidate = true

if candidate == false
   and max_worker_lifetime_ms > 0
   and LastBacklogCount > 0
   and elapsed_since_scale_up >= max_worker_lifetime_ms:
  candidate = true

if candidate == true
   and scale_up_dispatch_rate_epsilon > 0
   and prior last dispatch rate exists
   and abs(LastProcessingRate - prior_last_dispatch_rate) <= scale_up_dispatch_rate_epsilon:
  candidate = false

store current dispatch rate in state
```

If `candidate == true`, the poll emits one `invoke-worker` action and sets
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

## Dispatch-Rate Epsilon Suppression

Dispatch-rate epsilon suppression applies only after the queue has become a
scale-up candidate through the backlog threshold path or the worker lifetime
refresh path.

Suppression is active only when:

```text
scale_up_dispatch_rate_epsilon > 0
prior dispatch rate exists
abs(current dispatch rate - prior dispatch rate) <= scale_up_dispatch_rate_epsilon
```

The comparison is inclusive. A difference exactly equal to epsilon suppresses
the candidate.

Suppression is skipped on the first poll because no prior dispatch rate exists.

The current dispatch rate is written to state whether the candidate is emitted,
suppressed, or never created.

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
- `NextPoll`: `metrics_poll_interval_ms`.

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
