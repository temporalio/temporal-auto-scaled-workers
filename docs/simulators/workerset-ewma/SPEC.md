# Worker Set Scaling Algorithm Spec

This document specifies a worker set scaling algorithm using exponential weighted-moving-average (EWMA) on the arrival/dispatch rates reported by matching service. It determines the size of the worker set (a.k.a. the replica count) with a combination of a steady state, queueing theory-based formula and a fast-path handler for no-sync matches (that quickly triggers upscaling).

The fast path is driven by the clarity of the signal: when a no-sync match happens, it directly means that there is not sufficient compute for processing all work and so a scale-up should be strongly considered. For simplicity this is only smoothed via a cooldown.

The steady state path leverages the rate information as well as the backlog size (to allow for burndown if needed). This is based on queueing theory: it combines an estimated per-worker dispatch capacity with two staffing rules — a utilization-target rule that dominates at low load and a Halfin-Whitt square-root staffing rule that dominates under heavy load — and takes the larger of the two as desired capacity. As the raw rates might be very noisy (e.g. arrival rate can be very spiky) they are smoothed using EWMA (see https://en.wikipedia.org/wiki/EWMA_chart).

## Inputs

The scaler receives these values from its caller:

- `Backlog length`: number of tasks in the backlog of the task queue (waiting to be processed).
- `Arrival rate (/s)`: rolling task-arrival rate measured by the caller.
- `Dispatch rate (/s)`: rolling task-dispatch rate measured by the caller.
- `No-sync match`: event signal that an arrival could not start immediately.

The caller owns measurement configuration:

- `Rate window (s)`: rolling window used to measure arrival and dispatch rates before passing them to the scaler.

The scaler is configured with:

- `Initial worker count`: default starting value for planned worker count when no prior planned value is available.
- `Min workers`: lower bound for scale-down.
- `Max workers`: upper bound for planned workers.
- `Cadence (s)`: interval for regular scaler decisions.
- `EWMA alpha (%)`: smoothing factor for EWMA arrival rate, EWMA dispatch rate, and per-worker capacity estimate.
- `Initial per-worker capacity (/s)`: per-worker capacity used until the first saturation sample arrives (see Capacity Estimate).
- `Target backlog drain rate (/s)`: additional required dispatch rate included while backlog length is greater than zero.
- `Material backlog threshold`: backlog length at or above which cadence may sample per-worker capacity.
- `Utilization target (%)`: target mean utilization used by the utilization-target staffing candidate. Lower values reserve more headroom and dominate at low load.
- `Halfin-Whitt beta`: coefficient β in the square-root staffing formula `N = ρ + β·√ρ`. Larger values reserve more spare capacity and lower the probability that an arrival has to wait under heavy load.
- `Max scale-up step`: maximum planned-worker-count increase a cadence decision can make at once.
- `Scale-up cooldown (s)`: minimum time between planned-count increases.
- `Scale-down cooldown (s)`: minimum time between planned-count decreases.
- `No-sync quiet (s)`: minimum time since the last no-sync match before scale-down is allowed.

## Planned vs Running Workers

The scaler's only worker-count concept is the *planned worker count*: a target maintained by the scaler and updated by its decisions. The actual *running worker count* — how many worker processes are currently up and accepting work — is not available and so cannot be used by the algorithm.

From the caller's perspective a worker is in one of three lifecycle states:

- **Starting** — a planned-count increase has reserved this slot, but the underlying worker is still spinning up and not yet accepting tasks.
- **Active** — the worker has finished spin-up and is dispatching tasks.
- **Stopping** — a planned-count decrease has marked this worker for removal; the caller drains and shuts it down, or cancels its startup if it was still starting.

Each scaler decision updates the planned count immediately; the caller reconciles the running set to match. Because spin-up takes time, `planned ≥ running + starting` does not hold instantaneously: planned can lead running during scale-up (workers reserved but not yet active) and lag running briefly during scale-down (workers marked for removal but still draining in-flight work).

This split has three implications worth calling out, because the rest of the spec quietly relies on them:

- **Cooldowns gate planned-count changes, not running-count changes.** `Scale-up cooldown (s)` starts when the scaler increments planned, not when the new worker becomes active. Two scale-ups separated by less than the cooldown are blocked even if neither new worker is running yet. This is what prevents bursts of no-sync matches from issuing redundant scale-ups while the first one is still spinning up.
- **The per-worker capacity estimate divides by *planned* workers, not running.** During spin-up the dispatch rate is being served by fewer workers than planned, so the observed `dispatch rate / planned workers` is temporarily biased downward. The bias resolves as workers finish starting and as later saturation samples are taken; using planned (rather than running) keeps the scaler self-consistent — it estimates the capacity of the fleet *it has asked for*, which is what its next decision will compare against.
- **Scale-down prefers cancelling starts over stopping active workers.** When planned drops, the caller drops a still-starting worker first (no in-flight work to drain); only when no starts are pending does it stop an active worker. The scaler emits only the new planned count; the choice of which worker to drop is the caller's.

The scaler uses planned (not running) as its decision basis so that each decision is reflected in subsequent decisions immediately, without the scaler having to model spin-up latency or risk issuing duplicate scale-ups while a previous one is still in flight.

## State

The scaler tracks:

- Planned worker count: target worker count maintained by the scaler and counted against scale limits.
- EWMA arrival rate.
- EWMA dispatch rate.
- EWMA per-worker processing capacity.
- Last planned-count increase time.
- Last planned-count decrease time.
- Last no-sync match time.

The caller owns the measured backlog length and measured rates. The caller may restore a prior planned worker count; otherwise the scaler initializes planned worker count from `Initial worker count`.

## Capacity Estimate

The scaler updates its per-worker capacity estimate when a saturation signal provides a usable sample:

```text
observed per-worker capacity = dispatch rate / planned workers
estimated per-worker capacity =
  EWMA alpha * observed per-worker capacity +
  (1 - EWMA alpha) * previous estimated per-worker capacity
```

Saturation signals are:

- A no-sync match.
- A cadence tick where backlog length is greater than or equal to `Material backlog threshold`.

The estimate is updated only when planned worker count is greater than zero and dispatch rate is positive. Otherwise, the existing estimate is retained. Before the first usable sample, `Initial per-worker capacity (/s)` is used.

## No-Sync Decision

A no-sync match is an immediate scale-up signal.

On each no-sync match, the scaler increments planned worker count by one if all of the following are true:

- Planned worker count is below `Max workers`.
- `Scale-up cooldown (s)` has elapsed since the last planned-count increase.

The no-sync scale-up decision does not use backlog length, EWMA rates, cadence spare-capacity terms, or execution-capacity details.

## Cadence Decision

On every cadence tick:

1. The scaler updates EWMA arrival and dispatch rates using `EWMA alpha (%)`.
2. If the system is fully idle, the scaler snaps EWMA arrival and dispatch rates to zero.
3. If backlog length is at least `Material backlog threshold`, the scaler updates its per-worker capacity estimate from the raw dispatch rate.
4. The scaler computes one desired planned-worker count.
5. The scaler compares desired planned workers to current planned workers.

If desired is greater than current, the cadence path evaluates scale-up. If desired is less than current, it evaluates scale-down. Otherwise, it takes no action.

## Idle Snap-To-Zero

The cadence path removes residual EWMA rate memory once the caller reports a fully idle system:

- Backlog length is zero.
- Raw arrival rate is zero.
- Raw dispatch rate is zero.

When all conditions are true, EWMA arrival and dispatch rates are set to zero before computing desired workers. This allows desired workers to reach zero instead of staying at one because of a tiny smoothed arrival rate.

## Cadence Desired Capacity

Cadence computes desired workers from the smoothed arrival load, an optional backlog catch-up term, and the estimated per-worker capacity. The result is the larger of two staffing candidates, each capturing a different "how much spare capacity do we need" rule.

First, the load:

```text
backlog catch-up rate = Target backlog drain rate if backlog length > 0, otherwise 0
required rate         = EWMA arrival rate + backlog catch-up rate
offered load (ρ)      = required rate / estimated per-worker capacity
```

`offered load` (denoted ρ, in Erlangs) is the dimensionless ratio of demand to single-worker throughput — equivalently, the minimum number of workers needed to keep up *on average*. At exactly ρ workers, mean utilization is 100% and queue length grows without bound under any variability; any sustainable system needs strictly more than ρ. The two candidates below each pick how much more.

If `required rate` is zero, the formulas are skipped and `desired workers` is zero (then clamped to `Min workers`).

```text
utilization desired  = ceil(ρ / Utilization target)
Halfin-Whitt desired = ceil(ρ + Halfin-Whitt beta · √ρ)
desired workers      = max(utilization desired, Halfin-Whitt desired)
```

- **Utilization-target candidate** sizes the worker count so that mean utilization sits at `Utilization target`. The spare capacity is *proportional* to load (`ρ · (1/target − 1)`), so this candidate dominates at low load — e.g. at ρ = 1 with an 80% target it asks for 2 workers, leaving a full extra worker of headroom for a single unit of demand.
- **Halfin-Whitt candidate** applies the square-root staffing rule from heavy-traffic queueing theory: with `N = ρ + β·√ρ` servers, the probability that an arrival has to wait converges to a constant determined by `β` as ρ grows. Spare capacity grows with `√ρ` rather than with `ρ`, so the *absolute* spare grows with load but the *relative* spare shrinks. This candidate dominates at high load, where the proportional headroom of the utilization rule would be wastefully large.

Taking the max of the two gives the more conservative rule for the current load: utilization at low ρ, Halfin-Whitt at high ρ, with a smooth crossover. `desired workers` is then clamped to `[Min workers, Max workers]`.

## Cadence Scale-Up

When desired workers is greater than planned worker count, the scaler increases planned worker count by:

```text
min(Max scale-up step, desired - planned, Max workers - planned)
```

subject to `Scale-up cooldown (s)`. workers created by a planned-count increase may start accepting work later.

## Scale-Down

Scale-down runs only from cadence decisions.

Scale-down is allowed when all of the following are true:

- Desired worker count is less than planned worker count.
- Time since the last no-sync match is at least `No-sync quiet (s)`.
- Time since the last scale-down is at least `Scale-down cooldown (s)`.

When scale-down occurs, the scaler decrements planned worker count by one. Unlike scale-up, scale-down has no `Max scale-down step` parameter — it is intentionally single-step per cadence tick. Combined with `Scale-down cooldown (s)`, this rate-limits the descent and biases the algorithm toward retaining warm capacity through transient dips: under-provisioning is a reliability problem worth correcting fast, while over-provisioning is only a cost problem and recovers naturally over multiple cadence ticks. If the excess capacity is still starting, that startup can be canceled instead of stopping an active worker.

## Outputs

Each scaling decision emits one of:

- No action.
- Updated planned worker count.

The caller is responsible for reconciling running worker processes to the updated planned worker count and restoring that count on later decisions. If no prior planned worker count is available, the caller uses `Initial worker count` as the default.

## Failure Modes

The algorithm has a small number of slow-feedback paths where its internal state can drift from reality. None require external intervention to recover — the recovery dynamic noted with each item always applies — but each is worth understanding when tuning configured parameters or interpreting decisions.

- **Stale per-worker capacity estimate.** The estimate updates only when a saturation signal fires (no-sync match, or cadence tick with backlog ≥ `Material backlog threshold`). Under sustained light load with no backlog, the estimate is frozen. If actual per-worker capacity has *decreased* in that interval (heavier tasks, slower downstream, etc.), the next demand spike will be under-provisioned by cadence; the resulting no-sync matches both scale up directly and refresh the estimate. If actual capacity has *increased*, cadence over-provisions until the next saturation sample arrives, which can take indefinitely long under light load — accepted as a cost-side rather than reliability-side error.

- **Capacity estimate bias during spin-up.** `dispatch rate / planned workers` underestimates per-worker capacity while planned > running, because the running fleet is producing all of the dispatch rate. Cadence may briefly over-provision after a scale-up driven by saturation. The bias resolves naturally as workers finish starting and later saturation samples are taken.

- **Capacity estimate noise at small planned counts.** When planned workers is small (especially 1–2), the divisor is small and the estimate is dominated by short-term dispatch-rate noise. EWMA smoothing absorbs most of this, but the estimate can swing meaningfully between samples until the worker count grows. Operators of systems that run persistently at small worker counts should expect larger swings here than in the rate EWMAs.

- **EWMA hysteresis after a burst.** Smoothed rates decay exponentially at rate `1 − alpha` per cadence tick. After a burst clears, EWMA arrival rate remains elevated for several ticks, which delays scale-down. Lower `EWMA alpha (%)` exchanges responsiveness for stability. The idle snap-to-zero rescues the fully-idle case but not the "trickling at low rate" case.

- **Step-change in arrival rate.** A sudden rate *increase* is absorbed first by no-sync scale-ups (one worker per no-sync, bounded by `Scale-up cooldown (s)`), then by cadence catching up over several ticks as EWMA arrivals climb (bounded per tick by `Max scale-up step`). A sudden rate *decrease* is conservatively absorbed via the EWMA decay and the `No-sync quiet (s)` / `Scale-down cooldown (s)` gates — capacity is retained longer than strictly needed to avoid thrashing.

- **Worker failures invisible to the scaler.** If running workers die outside the scaler's decisions, planned stays ahead of running until the caller reconciles. Subsequent no-sync matches will scale up planned further; the planned count itself does not detect or correct for the loss. This is by design: worker liveness is the caller's domain, not the scaler's (see [Planned vs Running Workers](#planned-vs-running-workers)).

## Non-Goals

The algorithm intentionally does not use, and does not attempt to model, the following.

**Signals not available today.** The matching-service surface the scaler runs against does not expose these, so the algorithm cannot consume them. If/when they become available, several of these would be worth reconsidering as inputs.

- Execution-capacity internals beyond the no-sync-sampled per-worker capacity estimate: only the information matching service publishes on the server is available; richer slot- or queue-internal metrics are not.
- Work-duration estimates as scaling inputs: end-of-processing for a specific task is not surfaced, and neither are reliable average processing times.
- Worker idle state as a scaling input: idle/free-slot signals are not published to the server.
- Lifecycle state of starting workers as separate scaling inputs: worker insights exposes some of this to the server, but it is not integrated into the scaling algorithm today.

**Decisions delegated to the caller.** These are *available* in principle but are out of scope for this scaler by design — the scaler emits only a planned worker count.

- Cross-task-queue coordination: the scaler operates per task queue with no awareness of other queues that may share underlying capacity. Global budgets, fair-share allocation, or priority arbitration belong above this layer.
- Reconciliation of running worker processes to the planned count, including draining, deadline enforcement, and which specific worker to stop on scale-down (beyond the "prefer cancelling starts" guideline).

**Out of scope by design.** These are deliberate omissions from the algorithm itself, not just missing data.

- Backlog length or rates in the no-sync scale-up decision: a no-sync match alone already signals insufficient workers, so taking action should not be delayed for additional inputs. Any over-correction is bounded by `Scale-up cooldown (s)` and recovered (if needed) by the next cadence tick, which is not far off.
- Direct SLO targets (P99 wait, max queue depth, max age in backlog): the algorithm exposes `Utilization target (%)` and `Halfin-Whitt beta` as indirect proxies and does not adapt them to observed SLO breaches. Closing the loop on SLOs is left to a higher-level controller.
- Predictive or forecasting models: the algorithm is purely reactive on smoothed current rates. It does not project future arrivals from historical patterns, calendars, or learned seasonality.
- Adaptive tuning of its own parameters: `EWMA alpha`, `Utilization target`, `Halfin-Whitt beta`, and the cooldowns are static configuration inputs, not self-tuned at runtime.
