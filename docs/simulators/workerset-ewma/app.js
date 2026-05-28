(function () {
  'use strict';

  const MAX_HISTORY = 300;
  const MAX_CHART_POINTS = 240;
  const CHART_SAMPLE_MS = 1000;
  const EPSILON_MS = 0.0001;
  const IDLE_RATE_EPSILON = 1e-9;

  const state = {
    simTimeMs: 0,
    targetTimeMs: 0,
    lastFrameAt: null,
    paused: true,
    eventQueue: [],
    nextEventId: 1,
    nextTaskId: 1,
    nextConsumerId: 1,
    consumers: [],
    pendingConsumers: [],
    backlog: [],
    arrivalTimes: [],
    dispatchTimes: [],
    waitSamplesMs: [],
    history: [],
    chartPoints: [],
    lastChartSampleMs: -Infinity,
    chartVersion: 0,
    renderedChartVersion: -1,
    stats: {
      syncMatches: 0,
      noSyncMatches: 0,
      completed: 0,
      totalWaitMs: 0,
      waits: 0
    },
    scaler: {
      lastScaleUpMs: -Infinity,
      lastScaleDownMs: -Infinity,
      lastNoSyncMs: -Infinity,
      ewmaInitialized: false,
      ewmaArrivalRate: 0,
      ewmaDispatchRate: 0,
      ewmaPerConsumerCapacity: 0,
      plannedConsumers: null,
      lastDecisionText: 'No decisions yet'
    }
  };

  const els = {};
  let capacityChart = null;
  let rateChart = null;
  let utilizationChart = null;

  function cacheElements() {
    for (const id of [
      'runPauseBtn', 'stepBtn', 'resetBtn', 'addOneBtn', 'burstBtn', 'showMatchEvents',
      'metricTime', 'metricPlannedConsumers', 'metricConsumers', 'metricStartingConsumers',
      'metricBacklog', 'metricBusySlots', 'metricUtilization',
      'metricOldestBacklog', 'metricArrivalRate', 'metricDispatchRate',
      'metricEwmaArrivalRate', 'metricEwmaDispatchRate',
      'metricSyncMatches', 'metricNoSyncMatches', 'metricCompleted', 'metricMeanWait',
      'consumersList', 'history', 'slotSummary', 'capacityChart', 'rateChart', 'utilizationChart',
      'capacityChartLegend', 'rateChartLegend', 'utilizationChartLegend'
    ]) {
      els[id] = document.getElementById(id);
    }
  }

  function param(id) {
    const el = document.getElementById(id);
    const value = Number.parseFloat(el.value);
    return Number.isFinite(value) ? value : 0;
  }

  function intParam(id) {
    return Math.max(0, Math.round(param(id)));
  }

  function msParam(id) {
    return Math.max(0, param(id) * 1000);
  }

  function clamp(value, min, max) {
    return Math.min(max, Math.max(min, value));
  }

  function formatTime(ms) {
    if (ms < 1000) return ms.toFixed(0) + 'ms';
    if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
    const minutes = Math.floor(ms / 60000);
    const seconds = Math.floor((ms % 60000) / 1000);
    return minutes + 'm ' + String(seconds).padStart(2, '0') + 's';
  }

  function logEvent(kind, message, className) {
    state.history.unshift({
      at: state.simTimeMs,
      kind,
      message,
      className
    });
    if (state.history.length > MAX_HISTORY) state.history.pop();
  }

  function schedule(type, at, data) {
    const event = {
      id: state.nextEventId++,
      type,
      at,
      data: data || {}
    };
    state.eventQueue.push(event);
    state.eventQueue.sort((a, b) => a.at - b.at || a.id - b.id);
    return event;
  }

  function popNextEvent() {
    return state.eventQueue.shift();
  }

  function peekNextEvent() {
    return state.eventQueue[0] || null;
  }

  function removeFutureEvents(type) {
    state.eventQueue = state.eventQueue.filter(event => event.type !== type);
  }

  function randomNormal() {
    let u = 0;
    let v = 0;
    while (u === 0) u = Math.random();
    while (v === 0) v = Math.random();
    return Math.sqrt(-2 * Math.log(u)) * Math.cos(2 * Math.PI * v);
  }

  function sampleTaskDurationMs() {
    const mean = Math.max(0.1, param('durationMeanSec'));
    const stdDev = Math.max(0, param('durationStdDevSec'));
    const min = Math.max(0.1, param('durationMinSec'));
    const max = Math.max(min, param('durationMaxSec'));
    const sample = stdDev === 0 ? mean : mean + randomNormal() * stdDev;
    return clamp(sample, min, max) * 1000;
  }

  function sampleInterarrivalMs() {
    const rate = Math.max(0, param('arrivalRate'));
    if (rate <= 0) return Infinity;
    let u = 0;
    while (u === 0) u = Math.random();
    return (-Math.log(u) / rate) * 1000;
  }

  function scheduleNextArrival(fromMs) {
    removeFutureEvents('arrival');
    const delay = sampleInterarrivalMs();
    if (Number.isFinite(delay)) schedule('arrival', fromMs + delay);
  }

  function scheduleNextCadence(fromMs) {
    removeFutureEvents('cadence');
    schedule('cadence', fromMs + Math.max(1000, msParam('cadenceSec')));
  }

  function getTotalSlots() {
    return state.consumers.reduce((sum, consumer) => sum + consumer.slots.length, 0);
  }

  function getBusySlots() {
    return state.consumers.reduce((sum, consumer) => {
      return sum + consumer.slots.filter(slot => slot !== null).length;
    }, 0);
  }

  function normalizeConsumerCount(value) {
    const numeric = Number.isFinite(value) ? value : 0;
    return Math.max(0, Math.round(numeric));
  }

  function minConsumerCount() {
    return intParam('minConsumers');
  }

  function maxConsumerCount() {
    return Math.max(1, intParam('maxConsumers'));
  }

  function clampPlannedConsumerCount(value) {
    return clamp(normalizeConsumerCount(value), minConsumerCount(), maxConsumerCount());
  }

  function boundPlannedConsumerUpdate(value, current) {
    const normalized = normalizeConsumerCount(value);
    if (normalized > current) return Math.min(normalized, maxConsumerCount());
    if (normalized < current) return Math.max(normalized, minConsumerCount());
    return current;
  }

  function initialConsumerCount() {
    return clampPlannedConsumerCount(intParam('initialConsumers'));
  }

  function getPlannedConsumerCount() {
    const plannedConsumers = state.scaler.plannedConsumers;
    return Number.isFinite(plannedConsumers) ? plannedConsumers : initialConsumerCount();
  }

  function setPlannedConsumerCount(value) {
    state.scaler.plannedConsumers = normalizeConsumerCount(value);
    return state.scaler.plannedConsumers;
  }

  function getLifecycleConsumerCount() {
    return state.consumers.length + state.pendingConsumers.length;
  }

  function findFreeSlot() {
    for (const consumer of state.consumers) {
      const slotIndex = consumer.slots.findIndex(slot => slot === null);
      if (slotIndex !== -1) return { consumer, slotIndex };
    }
    return null;
  }

  function createActiveConsumer(reason) {
    const consumer = {
      id: state.nextConsumerId++,
      createdAt: state.simTimeMs,
      slots: Array(Math.max(1, intParam('slotsPerConsumer'))).fill(null)
    };
    state.consumers.push(consumer);
    if (reason) logEvent('scale up', 'Consumer #' + consumer.id + ' is active: ' + reason, 'scale-up');
    return consumer;
  }

  function activatePendingConsumer(pending) {
    const consumer = {
      id: pending.id,
      createdAt: state.simTimeMs,
      slots: Array(pending.slotsPerConsumer).fill(null)
    };
    state.pendingConsumers = state.pendingConsumers.filter(item => item.id !== pending.id);
    state.consumers.push(consumer);
    logEvent('scale up', 'Consumer #' + consumer.id + ' is accepting tasks after ' +
      formatTime(state.simTimeMs - pending.plannedAt) + ' spin-up', 'scale-up');
    const dispatched = drainBacklog();
    if (dispatched > 0) {
      logEvent('decision', 'Dispatched ' + dispatched + ' backlog item(s) to newly ready capacity', 'decision');
    }
    return consumer;
  }

  function startConsumerAfterPlannedIncrease(reason) {
    const spinUpMs = msParam('consumerSpinUpSec');
    const pending = {
      id: state.nextConsumerId++,
      plannedAt: state.simTimeMs,
      readyAt: state.simTimeMs + spinUpMs,
      slotsPerConsumer: Math.max(1, intParam('slotsPerConsumer'))
    };
    logEvent('scale up', 'Consumer #' + pending.id + ' will start after planned count update: ' + reason +
      (spinUpMs > 0 ? '; ready in ' + formatTime(spinUpMs) : '; ready now'), 'scale-up');
    if (spinUpMs <= EPSILON_MS) {
      activatePendingConsumer(pending);
    } else {
      state.pendingConsumers.push(pending);
      schedule('consumer-ready', pending.readyAt, { consumerId: pending.id });
    }
    return pending;
  }

  function applyPlannedDecrease(reason) {
    if (getLifecycleConsumerCount() <= getPlannedConsumerCount()) return false;
    const pendingTarget = state.pendingConsumers
      .slice()
      .sort((a, b) => b.plannedAt - a.plannedAt || b.id - a.id)[0];

    if (pendingTarget) {
      state.pendingConsumers = state.pendingConsumers.filter(consumer => consumer.id !== pendingTarget.id);
      state.eventQueue = state.eventQueue.filter(event => {
        return event.type !== 'consumer-ready' || event.data.consumerId !== pendingTarget.id;
      });
      logEvent('scale down', 'Cancelled starting consumer #' + pendingTarget.id + ': ' + reason, 'scale-down');
      return true;
    }

    if (state.consumers.length === 0) {
      return true;
    }

    const target = state.consumers
      .slice()
      .sort((a, b) => b.createdAt - a.createdAt || b.id - a.id)[0];
    const interrupted = target.slots
      .filter(slot => slot !== null)
      .map(slot => ({ id: slot.taskId, arrivedAt: slot.arrivedAt }));
    state.consumers = state.consumers.filter(consumer => consumer.id !== target.id);
    if (interrupted.length > 0) {
      state.backlog = interrupted.concat(state.backlog);
    }
    logEvent('scale down', 'Stopped consumer #' + target.id + ': ' + reason +
      (interrupted.length > 0 ? '; requeued ' + interrupted.length + ' in-flight task(s)' : ''), 'scale-down');
    drainBacklog();
    return true;
  }

  function updatePlannedConsumerCount(nextCount, reason) {
    const previousPlanned = getPlannedConsumerCount();
    const nextPlanned = boundPlannedConsumerUpdate(nextCount, previousPlanned);
    const delta = nextPlanned - previousPlanned;
    if (delta === 0) {
      return {
        changed: false,
        previousPlanned,
        nextPlanned,
        delta
      };
    }

    setPlannedConsumerCount(nextPlanned);
    logEvent(delta > 0 ? 'scale up' : 'scale down',
      'Updated planned consumers from ' + previousPlanned + ' to ' + nextPlanned + ': ' + reason,
      delta > 0 ? 'scale-up' : 'scale-down');

    if (delta > 0) {
      for (let i = 0; i < delta; i += 1) startConsumerAfterPlannedIncrease(reason);
    } else {
      for (let i = 0; i < Math.abs(delta); i += 1) applyPlannedDecrease(reason);
    }

    return {
      changed: true,
      previousPlanned,
      nextPlanned,
      delta
    };
  }

  function assignTaskToSlot(task, consumer, slotIndex, source) {
    const waitMs = state.simTimeMs - task.arrivedAt;
    const durationMs = sampleTaskDurationMs();
    consumer.slots[slotIndex] = {
      taskId: task.id,
      startedAt: state.simTimeMs,
      arrivedAt: task.arrivedAt,
      durationMs,
      source
    };
    state.dispatchTimes.push(state.simTimeMs);
    if (source === 'backlog') {
      state.stats.totalWaitMs += waitMs;
      state.stats.waits += 1;
      state.waitSamplesMs.push(waitMs);
      if (state.waitSamplesMs.length > 500) state.waitSamplesMs.shift();
    }
    schedule('completion', state.simTimeMs + durationMs, {
      consumerId: consumer.id,
      slotIndex,
      taskId: task.id
    });
  }

  function drainBacklog() {
    let dispatched = 0;
    while (state.backlog.length > 0) {
      const free = findFreeSlot();
      if (!free) break;
      const task = state.backlog.shift();
      assignTaskToSlot(task, free.consumer, free.slotIndex, 'backlog');
      dispatched += 1;
    }
    return dispatched;
  }

  function trimMetricWindows() {
    const cutoff = state.simTimeMs - Math.max(1000, msParam('rateWindowSec'));
    state.arrivalTimes = state.arrivalTimes.filter(t => t >= cutoff);
    state.dispatchTimes = state.dispatchTimes.filter(t => t >= cutoff);
  }

  function getRates() {
    trimMetricWindows();
    const windowSec = Math.max(1, param('rateWindowSec'));
    return {
      arrivalRate: state.arrivalTimes.length / windowSec,
      dispatchRate: state.dispatchTimes.length / windowSec
    };
  }

  function getScaleMetrics(rateOverride) {
    const rates = rateOverride || getRates();
    return {
      backlog: state.backlog.length,
      arrivalRate: rates.arrivalRate,
      dispatchRate: rates.dispatchRate
    };
  }

  function ewmaAlpha() {
    return clamp(param('ewmaAlphaPct') / 100, 0.01, 1);
  }

  function updateEwma(rates) {
    const alpha = ewmaAlpha();
    if (!state.scaler.ewmaInitialized) {
      state.scaler.ewmaArrivalRate = rates.arrivalRate;
      state.scaler.ewmaDispatchRate = rates.dispatchRate;
      state.scaler.ewmaInitialized = true;
      return;
    }
    state.scaler.ewmaArrivalRate =
      alpha * rates.arrivalRate + (1 - alpha) * state.scaler.ewmaArrivalRate;
    state.scaler.ewmaDispatchRate =
      alpha * rates.dispatchRate + (1 - alpha) * state.scaler.ewmaDispatchRate;
  }

  function maybeSnapEwmaToZero(rawMetrics) {
    if (rawMetrics.backlog > 0) return false;
    if (rawMetrics.arrivalRate > IDLE_RATE_EPSILON) return false;
    if (rawMetrics.dispatchRate > IDLE_RATE_EPSILON) return false;
    if (state.scaler.ewmaArrivalRate === 0 && state.scaler.ewmaDispatchRate === 0) return false;

    state.scaler.ewmaArrivalRate = 0;
    state.scaler.ewmaDispatchRate = 0;
    logEvent('decision',
      'cadence idle snap-to-zero: no backlog, arrivals, or dispatches; reset EWMA rates to 0.00/s',
      'decision');
    return true;
  }

  function initialPerConsumerCapacity() {
    return Math.max(0.01, param('initialPerConsumerCapacity'));
  }

  function estimatedPerConsumerCapacity() {
    const estimate = state.scaler.ewmaPerConsumerCapacity;
    return Number.isFinite(estimate) && estimate > 0 ? estimate : initialPerConsumerCapacity();
  }

  function materialBacklogThreshold() {
    return Math.max(1, intParam('materialBacklogThreshold'));
  }

  function updatePerConsumerCapacityFromSaturation(reason, rates, context) {
    const plannedConsumers = getPlannedConsumerCount();
    if (plannedConsumers <= 0 || rates.dispatchRate <= 0) return false;

    const observedCapacity = rates.dispatchRate / plannedConsumers;
    if (!Number.isFinite(observedCapacity) || observedCapacity <= 0) return false;

    const previousEstimate = estimatedPerConsumerCapacity();
    const alpha = ewmaAlpha();
    state.scaler.ewmaPerConsumerCapacity =
      alpha * observedCapacity + (1 - alpha) * previousEstimate;

    logEvent('decision', reason + ' capacity sample: observed=' +
      observedCapacity.toFixed(2) + '/s per consumer, estimate=' +
      state.scaler.ewmaPerConsumerCapacity.toFixed(2) + '/s per consumer' +
      (context ? ', ' + context : ''), 'decision');
    return true;
  }

  function targetBacklogDrainRate() {
    return Math.max(0, param('targetBacklogDrainRate'));
  }

  function utilizationTarget() {
    return clamp(param('utilizationTargetPct') / 100, 0.01, 1);
  }

  function halfinWhittBeta() {
    return Math.max(0, param('halfinWhittBeta'));
  }

  function computeScalePlan(metrics) {
    const minConsumers = intParam('minConsumers');
    const maxConsumers = Math.max(1, intParam('maxConsumers'));
    const plannedConsumers = getPlannedConsumerCount();
    const catchUpRate = metrics.backlog > 0 ? targetBacklogDrainRate() : 0;
    const requiredRate = metrics.arrivalRate + catchUpRate;
    const perConsumerCapacity = estimatedPerConsumerCapacity();
    const offeredLoad = requiredRate > 0 ? requiredRate / perConsumerCapacity : 0;
    const baseConsumers = offeredLoad > 0 ? Math.ceil(offeredLoad) : 0;
    const targetUtilization = utilizationTarget();
    const utilizationDesired = offeredLoad > 0 ? Math.ceil(offeredLoad / targetUtilization) : 0;
    const beta = halfinWhittBeta();
    const halfinWhittDesired = offeredLoad > 0 ? Math.ceil(offeredLoad + beta * Math.sqrt(offeredLoad)) : 0;
    const spareConsumers = Math.max(
      0,
      utilizationDesired - baseConsumers,
      halfinWhittDesired - baseConsumers
    );
    const rawDesiredConsumers = baseConsumers + spareConsumers;
    return {
      desiredConsumers: clamp(rawDesiredConsumers, minConsumers, maxConsumers),
      rawDesiredConsumers,
      plannedConsumers,
      catchUpRate,
      requiredRate,
      perConsumerCapacity,
      offeredLoad,
      baseConsumers,
      targetUtilization,
      utilizationDesired,
      halfinWhittBeta: beta,
      halfinWhittDesired,
      spareConsumers
    };
  }

  function formatScaleInputs(metrics, scalePlan) {
    return ', backlog=' + metrics.backlog +
      ', arrivals=' + metrics.arrivalRate.toFixed(2) + '/s' +
      ', dispatches=' + metrics.dispatchRate.toFixed(2) + '/s' +
      ', backlog catch-up=' + scalePlan.catchUpRate.toFixed(2) + '/s' +
      ', required=' + scalePlan.requiredRate.toFixed(2) + '/s' +
      ', capacity=' + scalePlan.perConsumerCapacity.toFixed(2) + '/s per consumer' +
      ', load=' + scalePlan.offeredLoad.toFixed(2) +
      ', base=' + scalePlan.baseConsumers +
      ', util desired=' + scalePlan.utilizationDesired +
      ' @ ' + (scalePlan.targetUtilization * 100).toFixed(0) + '%' +
      ', HW desired=' + scalePlan.halfinWhittDesired +
      ' beta=' + scalePlan.halfinWhittBeta.toFixed(2) +
      ', spare=' + scalePlan.spareConsumers +
      ', raw desired=' + scalePlan.rawDesiredConsumers;
  }

  function maybeScaleUp(trigger, metrics, scalePlan) {
    const maxConsumers = Math.max(1, intParam('maxConsumers'));
    const cooldownMs = msParam('scaleUpCooldownSec');
    const maxStep = Math.max(1, intParam('maxScaleUpStep'));
    const current = scalePlan.plannedConsumers;
    const desired = scalePlan.desiredConsumers;

    if (state.simTimeMs - state.scaler.lastScaleUpMs < cooldownMs) {
      const wait = cooldownMs - (state.simTimeMs - state.scaler.lastScaleUpMs);
      const text = trigger + ': scale-up blocked by cooldown for ' + formatTime(wait) +
        '; desired=' + desired + ', current=' + current +
        formatScaleInputs(metrics, scalePlan);
      state.scaler.lastDecisionText = text;
      logEvent('blocked', text, 'blocked');
      return false;
    }

    const addCount = Math.min(maxStep, desired - current, maxConsumers - current);
    const update = updatePlannedConsumerCount(current + addCount,
      trigger + ' desired=' + desired + formatScaleInputs(metrics, scalePlan));
    if (!update.changed) {
      const text = trigger + ': scale-up blocked; no planned-consumer headroom' +
        '; desired=' + desired + ', current=' + current +
        formatScaleInputs(metrics, scalePlan);
      state.scaler.lastDecisionText = text;
      logEvent('blocked', text, 'blocked');
      return false;
    }
    state.scaler.lastScaleUpMs = state.simTimeMs;
    state.scaler.lastDecisionText = trigger + ': updated planned consumers from ' +
      update.previousPlanned + ' to ' + update.nextPlanned;
    return true;
  }

  function maybeScaleUpFromNoSync() {
    const maxConsumers = Math.max(1, intParam('maxConsumers'));
    const cooldownMs = msParam('scaleUpCooldownSec');
    const current = getPlannedConsumerCount();

    if (current >= maxConsumers) {
      const text = 'no-sync: scale-up blocked; max consumers reached (' + current + ')';
      state.scaler.lastDecisionText = text;
      logEvent('blocked', text, 'blocked');
      return false;
    }

    if (state.simTimeMs - state.scaler.lastScaleUpMs < cooldownMs) {
      const wait = cooldownMs - (state.simTimeMs - state.scaler.lastScaleUpMs);
      const text = 'no-sync: scale-up blocked by cooldown for ' + formatTime(wait);
      state.scaler.lastDecisionText = text;
      logEvent('blocked', text, 'blocked');
      return false;
    }

    const update = updatePlannedConsumerCount(current + 1, 'no-sync match observed; all existing slots were busy');
    if (!update.changed) {
      const text = 'no-sync: scale-up blocked; no planned-consumer headroom';
      state.scaler.lastDecisionText = text;
      logEvent('blocked', text, 'blocked');
      return false;
    }
    state.scaler.lastScaleUpMs = state.simTimeMs;
    state.scaler.lastDecisionText = 'no-sync: updated planned consumers from ' +
      update.previousPlanned + ' to ' + update.nextPlanned;
    return true;
  }

  function maybeScaleDown(trigger, metrics, scalePlan) {
    const cooldownMs = msParam('scaleDownCooldownSec');
    const quietMs = msParam('scaleDownQuietSec');
    const minConsumers = intParam('minConsumers');
    const current = scalePlan.plannedConsumers;
    const desired = scalePlan.desiredConsumers;
    const scaleInputs = formatScaleInputs(metrics, scalePlan);

    if (current <= minConsumers) {
      const text = trigger + ': hold scale-down from ' + current + ' to desired=' + desired +
        '; already at min planned consumers (' + minConsumers + ')' + scaleInputs;
      state.scaler.lastDecisionText = text;
      logEvent('decision', text, 'decision');
      return false;
    }
    if (state.simTimeMs - state.scaler.lastNoSyncMs < quietMs) {
      const wait = quietMs - (state.simTimeMs - state.scaler.lastNoSyncMs);
      const text = trigger + ': hold scale-down from ' + current + ' to desired=' + desired +
        '; no-sync quiet period has ' + formatTime(wait) + ' left' +
        scaleInputs;
      state.scaler.lastDecisionText = text;
      logEvent('decision', text, 'decision');
      return false;
    }
    if (state.simTimeMs - state.scaler.lastScaleDownMs < cooldownMs) {
      const wait = cooldownMs - (state.simTimeMs - state.scaler.lastScaleDownMs);
      const text = trigger + ': hold scale-down from ' + current + ' to desired=' + desired +
        '; cooldown has ' + formatTime(wait) + ' left' +
        scaleInputs;
      state.scaler.lastDecisionText = text;
      logEvent('decision', text, 'decision');
      return false;
    }

    const update = updatePlannedConsumerCount(current - 1,
      'desired scale ' + desired + ' is below current ' + current + scaleInputs);
    if (update.changed) {
      state.scaler.lastScaleDownMs = state.simTimeMs;
      state.scaler.lastDecisionText = trigger + ': updated planned consumers from ' +
        update.previousPlanned + ' to ' + update.nextPlanned + '; desired=' + desired + scaleInputs;
      return true;
    }
    state.scaler.lastDecisionText = trigger + ': hold scale-down from ' + current +
      ' to desired=' + desired + '; planned count cannot be reduced' + scaleInputs;
    logEvent('decision', state.scaler.lastDecisionText, 'decision');
    return false;
  }

  function runScalingDecision(trigger, metrics) {
    const scalePlan = computeScalePlan(metrics);
    const current = scalePlan.plannedConsumers;
    const desired = scalePlan.desiredConsumers;

    if (desired > current) {
      maybeScaleUp(trigger, metrics, scalePlan);
      return;
    }
    if (desired < current) {
      maybeScaleDown(trigger, metrics, scalePlan);
      return;
    }
    const text = trigger + ': hold at ' + current + ' consumers; desired=' + desired +
      formatScaleInputs(metrics, scalePlan);
    state.scaler.lastDecisionText = text;
    logEvent('decision', text, 'decision');
  }

  function processArrival(data) {
    const count = data.count || 1;
    for (let i = 0; i < count; i += 1) {
      const task = {
        id: state.nextTaskId++,
        arrivedAt: state.simTimeMs
      };
      state.arrivalTimes.push(state.simTimeMs);
      const free = findFreeSlot();
      if (free) {
        state.stats.syncMatches += 1;
        assignTaskToSlot(task, free.consumer, free.slotIndex, 'sync');
        if (els.showMatchEvents.checked) {
          logEvent('match', 'Task #' + task.id + ' sync matched to consumer #' + free.consumer.id, 'match');
        }
      } else {
        state.stats.noSyncMatches += 1;
        state.scaler.lastNoSyncMs = state.simTimeMs;
        state.backlog.push(task);
        logEvent('match', 'Task #' + task.id + ' no-sync matched; backlog=' + state.backlog.length, 'match');
        updatePerConsumerCapacityFromSaturation('no-sync', getRates(), 'backlog=' + state.backlog.length);
        maybeScaleUpFromNoSync();
      }
    }
    if (!data.manual) scheduleNextArrival(state.simTimeMs);
  }

  function processCompletion(data) {
    const consumer = state.consumers.find(item => item.id === data.consumerId);
    if (!consumer) return;
    const slot = consumer.slots[data.slotIndex];
    if (!slot || slot.taskId !== data.taskId) return;
    consumer.slots[data.slotIndex] = null;
    state.stats.completed += 1;
    if (state.backlog.length > 0) {
      const task = state.backlog.shift();
      assignTaskToSlot(task, consumer, data.slotIndex, 'backlog');
    }
  }

  function processConsumerReady(data) {
    const pending = state.pendingConsumers.find(item => item.id === data.consumerId);
    if (!pending) return;
    activatePendingConsumer(pending);
  }

  function processCadence() {
    const rawMetrics = getScaleMetrics();
    updateEwma(rawMetrics);
    maybeSnapEwmaToZero(rawMetrics);
    const smoothedMetrics = getScaleMetrics({
      arrivalRate: state.scaler.ewmaArrivalRate,
      dispatchRate: state.scaler.ewmaDispatchRate
    });
    const backlogThreshold = materialBacklogThreshold();
    logEvent('decision', 'cadence input: backlog=' + rawMetrics.backlog +
      ', raw arrivals=' + rawMetrics.arrivalRate.toFixed(2) + '/s' +
      ', raw dispatches=' + rawMetrics.dispatchRate.toFixed(2) + '/s' +
      ', ewma arrivals=' + smoothedMetrics.arrivalRate.toFixed(2) + '/s' +
      ', ewma dispatches=' + smoothedMetrics.dispatchRate.toFixed(2) + '/s', 'decision');
    if (rawMetrics.backlog >= backlogThreshold) {
      updatePerConsumerCapacityFromSaturation('cadence backlog', rawMetrics,
        'backlog=' + rawMetrics.backlog + ', threshold=' + backlogThreshold);
    }
    runScalingDecision('cadence', smoothedMetrics);
    scheduleNextCadence(state.simTimeMs);
  }

  function processEvent(event) {
    state.simTimeMs = event.at;
    if (event.type === 'arrival') processArrival(event.data);
    if (event.type === 'completion') processCompletion(event.data);
    if (event.type === 'consumer-ready') processConsumerReady(event.data);
    if (event.type === 'cadence') processCadence();
    sampleCharts();
  }

  function processUntil(targetTimeMs, maxEvents) {
    let processed = 0;
    while (processed < maxEvents) {
      const next = peekNextEvent();
      if (!next || next.at > targetTimeMs + EPSILON_MS) break;
      processEvent(popNextEvent());
      processed += 1;
    }
    state.simTimeMs = Math.max(state.simTimeMs, targetTimeMs);
    sampleCharts();
    return processed;
  }

  function stepOneEvent() {
    const next = popNextEvent();
    if (!next) return;
    processEvent(next);
    state.targetTimeMs = state.simTimeMs;
    render();
  }

  function initializeConsumers() {
    const initial = getPlannedConsumerCount();
    for (let i = 0; i < initial; i += 1) createActiveConsumer('');
    state.history = [];
  }

  function reset() {
    state.simTimeMs = 0;
    state.targetTimeMs = 0;
    state.lastFrameAt = null;
    state.paused = true;
    state.eventQueue = [];
    state.nextEventId = 1;
    state.nextTaskId = 1;
    state.nextConsumerId = 1;
    state.consumers = [];
    state.pendingConsumers = [];
    state.backlog = [];
    state.arrivalTimes = [];
    state.dispatchTimes = [];
    state.waitSamplesMs = [];
    state.history = [];
    state.chartPoints = [];
    state.lastChartSampleMs = -Infinity;
    state.chartVersion = 0;
    state.renderedChartVersion = -1;
    state.stats = {
      syncMatches: 0,
      noSyncMatches: 0,
      completed: 0,
      totalWaitMs: 0,
      waits: 0
    };
    state.scaler = {
      lastScaleUpMs: -Infinity,
      lastScaleDownMs: -Infinity,
      lastNoSyncMs: -Infinity,
      ewmaInitialized: false,
      ewmaArrivalRate: 0,
      ewmaDispatchRate: 0,
      ewmaPerConsumerCapacity: initialPerConsumerCapacity(),
      plannedConsumers: initialConsumerCount(),
      lastDecisionText: 'No decisions yet'
    };
    initializeConsumers();
    scheduleNextArrival(0);
    scheduleNextCadence(0);
    sampleCharts(true);
    updateRunButton();
    render();
  }

  function sampleCharts(force) {
    if (!force && state.simTimeMs - state.lastChartSampleMs < CHART_SAMPLE_MS) return;
    state.lastChartSampleMs = state.simTimeMs;
    const rates = getRates();
    const plannedConsumers = getPlannedConsumerCount();
    const totalSlots = getTotalSlots();
    const busySlots = getBusySlots();
    state.chartPoints.push({
      timeSec: state.simTimeMs / 1000,
      consumers: state.consumers.length,
      startingConsumers: state.pendingConsumers.length,
      backlog: state.backlog.length,
      busySlots,
      slotUtilizationPct: totalSlots > 0 ? (busySlots / totalSlots) * 100 : 0,
      arrivalRate: rates.arrivalRate,
      dispatchRate: rates.dispatchRate,
      perConsumerDispatchRate: plannedConsumers > 0 ? rates.dispatchRate / plannedConsumers : 0,
      ewmaArrivalRate: state.scaler.ewmaArrivalRate,
      ewmaDispatchRate: state.scaler.ewmaDispatchRate,
      ewmaPerConsumerCapacity: estimatedPerConsumerCapacity()
    });
    if (state.chartPoints.length > MAX_CHART_POINTS) state.chartPoints.shift();
    state.chartVersion += 1;
  }

  function makeDataset(label, color, formatValue) {
    return {
      label,
      data: [],
      borderColor: color,
      backgroundColor: color,
      borderWidth: 2,
      pointRadius: 0,
      pointHoverRadius: 4,
      tension: 0.25,
      fill: false,
      formatValue
    };
  }

  function makeChartOptions(formatTick, yBounds) {
    const tickFormatter = formatTick || (value => value.toFixed(0));
    const bounds = yBounds || {};
    return {
      animation: false,
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: {
          labels: {
            color: '#687076',
            boxWidth: 12,
            padding: 12
          }
        },
        tooltip: {
          mode: 'index',
          intersect: false,
          callbacks: {
            title(items) {
              return items.length > 0 ? 'time: ' + items[0].label : '';
            },
            label(context) {
              const formatter = context.dataset.formatValue || tickFormatter;
              return context.dataset.label + ': ' + formatter(context.parsed.y);
            }
          }
        }
      },
      scales: {
        x: {
          ticks: {
            color: '#687076',
            maxTicksLimit: 8
          },
          grid: { color: '#e9e1d2' }
        },
        y: {
          beginAtZero: true,
          min: bounds.min,
          max: bounds.max,
          ticks: {
            color: '#687076',
            callback: value => tickFormatter(Number(value))
          },
          grid: { color: '#e9e1d2' }
        }
      }
    };
  }

  function initCharts() {
    const ChartCtor = window.Chart;
    if (!ChartCtor) return;

    capacityChart = new ChartCtor(els.capacityChart, {
      type: 'line',
      data: {
        labels: [],
        datasets: [
          makeDataset('consumers', '#0f766e', value => value.toFixed(0)),
          makeDataset('starting', '#2563eb', value => value.toFixed(0)),
          makeDataset('backlog', '#c2410c', value => value.toFixed(0)),
          makeDataset('busy slots', '#b45309', value => value.toFixed(0))
        ]
      },
      options: makeChartOptions(value => value.toFixed(0))
    });

    rateChart = new ChartCtor(els.rateChart, {
      type: 'line',
      data: {
        labels: [],
        datasets: [
          makeDataset('raw arrivals', '#2563eb', value => value.toFixed(2) + '/s'),
          makeDataset('raw dispatches', '#15803d', value => value.toFixed(2) + '/s'),
          makeDataset('raw / consumer', '#0891b2', value => value.toFixed(2) + '/s'),
          makeDataset('ewma arrivals', '#7c3aed', value => value.toFixed(2) + '/s'),
          makeDataset('ewma dispatches', '#b45309', value => value.toFixed(2) + '/s'),
          makeDataset('ewma / consumer', '#be185d', value => value.toFixed(2) + '/s')
        ]
      },
      options: makeChartOptions(value => value.toFixed(1))
    });

    utilizationChart = new ChartCtor(els.utilizationChart, {
      type: 'line',
      data: {
        labels: [],
        datasets: [
          makeDataset('slot utilization', '#7c3aed', value => value.toFixed(1) + '%')
        ]
      },
      options: makeChartOptions(value => value.toFixed(0) + '%', { min: 0, max: 100 })
    });
  }

  function updateChart(chart, extractors) {
    if (!chart) return;
    chart.data.labels = state.chartPoints.map(point => point.timeSec.toFixed(1) + 's');
    chart.data.datasets.forEach((dataset, index) => {
      dataset.data = state.chartPoints.map(extractors[index]);
    });
    chart.update('none');
  }

  function renderCharts() {
    if (state.renderedChartVersion === state.chartVersion) return;
    updateChart(capacityChart, [
      point => point.consumers,
      point => point.startingConsumers,
      point => point.backlog,
      point => point.busySlots
    ]);
    updateChart(rateChart, [
      point => point.arrivalRate,
      point => point.dispatchRate,
      point => point.perConsumerDispatchRate,
      point => point.ewmaArrivalRate,
      point => point.ewmaDispatchRate,
      point => point.ewmaPerConsumerCapacity
    ]);
    updateChart(utilizationChart, [
      point => point.slotUtilizationPct
    ]);
    state.renderedChartVersion = state.chartVersion;
    els.capacityChartLegend.textContent = 'hover for exact values';
    els.rateChartLegend.textContent = 'hover for exact values';
    els.utilizationChartLegend.textContent = 'hover for exact values';
  }

  function renderMetrics() {
    const rates = getRates();
    const totalSlots = getTotalSlots();
    const busySlots = getBusySlots();
    const utilization = totalSlots > 0 ? Math.round((busySlots / totalSlots) * 100) : 0;
    const oldestBacklogMs = state.backlog.length > 0
      ? state.simTimeMs - state.backlog[0].arrivedAt
      : 0;
    const meanWaitMs = state.stats.waits > 0 ? state.stats.totalWaitMs / state.stats.waits : 0;

    els.metricTime.textContent = formatTime(state.simTimeMs);
    els.metricPlannedConsumers.textContent = String(getPlannedConsumerCount());
    els.metricConsumers.textContent = String(state.consumers.length);
    els.metricStartingConsumers.textContent = String(state.pendingConsumers.length);
    els.metricBacklog.textContent = String(state.backlog.length);
    els.metricBusySlots.textContent = busySlots + '/' + totalSlots;
    els.metricUtilization.textContent = utilization + '%';
    els.metricOldestBacklog.textContent = formatTime(oldestBacklogMs);
    els.metricArrivalRate.textContent = rates.arrivalRate.toFixed(2) + '/s';
    els.metricDispatchRate.textContent = rates.dispatchRate.toFixed(2) + '/s';
    els.metricEwmaArrivalRate.textContent = state.scaler.ewmaArrivalRate.toFixed(2) + '/s';
    els.metricEwmaDispatchRate.textContent = state.scaler.ewmaDispatchRate.toFixed(2) + '/s';
    els.metricSyncMatches.textContent = String(state.stats.syncMatches);
    els.metricNoSyncMatches.textContent = String(state.stats.noSyncMatches);
    els.metricCompleted.textContent = String(state.stats.completed);
    els.metricMeanWait.textContent = formatTime(meanWaitMs);
    els.slotSummary.textContent = busySlots + ' busy, ' + Math.max(0, totalSlots - busySlots) +
      ' free, ' + getPlannedConsumerCount() + ' planned, ' +
      state.pendingConsumers.length + ' starting, ' + state.backlog.length + ' backlog';
  }

  function renderConsumers() {
    if (state.consumers.length === 0 && state.pendingConsumers.length === 0) {
      els.consumersList.innerHTML = '<div class="empty-state">No consumers are running.</div>';
      return;
    }
    const activeHtml = state.consumers.map(consumer => {
      const busy = consumer.slots.filter(slot => slot !== null).length;
      const slots = consumer.slots.map(slot => {
        if (!slot) return '<span class="slot free" title="free"></span>';
        const elapsed = state.simTimeMs - slot.startedAt;
        const remaining = Math.max(0, slot.durationMs - elapsed);
        const cls = slot.source === 'backlog' ? 'slot backlog' : 'slot busy';
        return '<span class="' + cls + '" title="task #' + slot.taskId + ', remaining ' + formatTime(remaining) + '"></span>';
      }).join('');
      return '<article class="consumer">' +
        '<div class="consumer-header">' +
        '<span class="consumer-name">Consumer #' + consumer.id + '</span>' +
        '<span class="consumer-meta">' + busy + '/' + consumer.slots.length + ' slots</span>' +
        '</div>' +
        '<div class="slots">' + slots + '</div>' +
        '</article>';
    }).join('');
    const pendingHtml = state.pendingConsumers.map(consumer => {
      const remaining = Math.max(0, consumer.readyAt - state.simTimeMs);
      return '<article class="consumer pending">' +
        '<div class="consumer-header">' +
        '<span class="consumer-name">Consumer #' + consumer.id + '</span>' +
        '<span class="consumer-meta">starting, ready in ' + formatTime(remaining) + '</span>' +
        '</div>' +
        '<div class="slots"><span class="slot pending" title="starting"></span></div>' +
        '</article>';
    }).join('');
    els.consumersList.innerHTML = activeHtml + pendingHtml;
  }

  function renderHistory() {
    const showMatches = els.showMatchEvents.checked;
    const visible = state.history.filter(entry => showMatches || entry.kind !== 'match');
    if (visible.length === 0) {
      els.history.innerHTML = '<div class="empty-state">No scaler events yet.</div>';
      return;
    }
    els.history.innerHTML = visible.slice(0, 150).map(entry => {
      return '<div class="history-entry ' + entry.className + '">' +
        '<span class="time">' + formatTime(entry.at) + '</span>' +
        '<span class="kind">' + entry.kind + '</span>' +
        '<span class="message">' + entry.message + '</span>' +
        '</div>';
    }).join('');
  }

  function render() {
    renderMetrics();
    renderConsumers();
    renderHistory();
    renderCharts();
  }

  function updateRunButton() {
    els.runPauseBtn.textContent = state.paused ? 'Start' : 'Pause';
  }

  function animationLoop(now) {
    if (state.lastFrameAt === null) state.lastFrameAt = now;
    const deltaRealMs = now - state.lastFrameAt;
    state.lastFrameAt = now;

    if (!state.paused) {
      const speed = clamp(param('runSpeed'), 0.1, 500);
      state.targetTimeMs += deltaRealMs * speed;
      processUntil(state.targetTimeMs, 5000);
    }
    render();
    window.requestAnimationFrame(animationLoop);
  }

  function bindEvents() {
    els.runPauseBtn.addEventListener('click', function () {
      state.paused = !state.paused;
      state.targetTimeMs = state.simTimeMs;
      state.lastFrameAt = null;
      updateRunButton();
    });
    els.stepBtn.addEventListener('click', function () {
      state.paused = true;
      updateRunButton();
      stepOneEvent();
    });
    els.resetBtn.addEventListener('click', reset);
    els.addOneBtn.addEventListener('click', function () {
      schedule('arrival', state.simTimeMs, { manual: true });
      processUntil(state.simTimeMs, 1000);
      render();
    });
    els.burstBtn.addEventListener('click', function () {
      schedule('arrival', state.simTimeMs, { manual: true, count: Math.max(1, intParam('burstSize')) });
      processUntil(state.simTimeMs, 5000);
      render();
    });
    els.showMatchEvents.addEventListener('change', renderHistory);

    for (const id of ['arrivalRate', 'cadenceSec']) {
      document.getElementById(id).addEventListener('change', function () {
        scheduleNextArrival(state.simTimeMs);
        scheduleNextCadence(state.simTimeMs);
      });
    }
  }

  cacheElements();
  initCharts();
  bindEvents();
  reset();
  window.requestAnimationFrame(animationLoop);
})();
