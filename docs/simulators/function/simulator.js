(function () {
  'use strict';

  let queue = [];
  let consumers = [];
  let lastScaleUpTimeMs = 0;
  let creationHistory = [];
  let running = false;
  let simTime = 0;
  let lastRealTime = 0;
  let tickIntervalId = null;
  let arrivalIntervalId = null;
  let arrivalAccumulator = 0;
  let nextWorkId = 1;
  let nextConsumerId = 1;
  let lastMetricsPollRealTime = 0;
  let lastDispatchRate = -1;
  // --- flat dispatch rate detection state (engages only when dispatch-rate epsilon > 0) ---
  let flatSinceMs = 0;          // when dispatch first went flat with a backlog (0 = not flat)
  let suppressUntilMs = 0;      // fast-path suppression flag: growth gated while now < this
  let refRate = -1;             // dispatch rate anchored when flat began; must move off it to resume (-1 = none)
  let nextPollIntervalMs = 0;   // adaptive poll: 0 = use the configured interval
  let gatingStatus = 'off';     // 'off' | 'clear' | 'confirming' | 'suppressed'
  let noSyncPending = false;    // a no-sync match occurred this batch window (for ~500ms task-add batching)
  let lastBatchRealTime = 0;
  let arrivalTimestamps = [];
  let dispatchTimestamps = [];
  let chartData = [];
  let lastChartSampleTime = 0;
  let chart = null;
  let rateChartData = [];
  let rateChart = null;
  const RATE_WINDOW_MS = 30000; // dispatch/arrival averaging window — models the server's fixed ~30s metric window (not a tunable config)
  const RATE_TRIM_MS = 90000;
  const TASK_ADD_BATCH_MS = 500;   // client batches no-sync signals (MinSignalIntervalNoSyncMatch default); fixed infra
  const CHART_WINDOW_MS = 300000;
  const CHART_SAMPLE_MS = 500;

  function getConfig() {
    return {
      slotsPerConsumer: Math.max(1, parseInt(document.getElementById('slotsPerConsumer').value, 10) || 5),
      workDurationMinMs: (parseFloat(document.getElementById('workDurationMin').value) || 30) * 1000,
      workDurationMaxMs: (parseFloat(document.getElementById('workDurationMax').value) || 60) * 1000,
      scaleUpCooloffMs: (function () {
        var v = parseInt(document.getElementById('scaleUpCooloff').value, 10);
        return (isNaN(v) || v < 0) ? 100 : v;
      })(),
      maxWorkerLifetimeMs: (function () {
        var v = parseInt(document.getElementById('maxWorkerLifetime').value, 10);
        return (isNaN(v) || v < 0) ? 600000 : v;
      })(),
      metricsPollIntervalMs: (function () {
        var poll = parseInt(document.getElementById('metricsPollInterval').value, 10);
        var cooloff = parseInt(document.getElementById('scaleUpCooloff').value, 10);
        if (isNaN(poll) || poll < 10000) poll = 60000;
        if (!isNaN(cooloff) && cooloff > 0 && poll < cooloff) return cooloff;
        return poll;
      })(),
      maxWorkers: Math.max(1, parseInt(document.getElementById('maxWorkers').value, 10) || 10),
      arrivalRate: (function () {
        var v = parseFloat(document.getElementById('arrivalRate').value);
        return (isNaN(v) || v < 0) ? 5 : v;
      })(),
      itemsPerClick: Math.max(1, parseInt(document.getElementById('itemsPerClick').value, 10) || 1),
      scaleUpBacklogThreshold: (function () {
        var v = parseInt(document.getElementById('scaleUpBacklogThreshold').value, 10);
        return (isNaN(v) || v < 0) ? 0 : v;
      })(),
      maxDispatchRate: (function () {
        var v = parseFloat(document.getElementById('maxDispatchRate').value);
        return (isNaN(v) || v < 0) ? 0 : v;
      })(),
      workerExitAfterMs: (function () {
        var v = parseFloat(document.getElementById('workerExitAfter').value);
        return (isNaN(v) || v < 0) ? 50000 : v * 1000;
      })(),
      scaleUpDispatchRateEpsilon: (function () {
        var v = parseFloat(document.getElementById('scaleUpDispatchRateEpsilon').value);
        return (isNaN(v) || v < 0) ? 0 : v;
      })(),
      // --- physics / fidelity knobs (mirror the real world) ---
      coldStartMs: (function () {
        var v = parseFloat(document.getElementById('coldStart').value);
        return (isNaN(v) || v < 0) ? 0 : v * 1000;
      })(),
      // --- flat dispatch rate detection knobs (engage only when epsilon > 0) ---
      dispatchConfirmMs: (function () {
        var v = parseInt(document.getElementById('dispatchConfirm').value, 10);
        return (isNaN(v) || v < 0) ? 45000 : v;
      })(),
      dispatchSuppressMs: (function () {
        var v = parseInt(document.getElementById('dispatchSuppress').value, 10);
        return (isNaN(v) || v < 0) ? 120000 : v;
      })(),
      suppressPollIntervalMs: (function () {
        var v = parseInt(document.getElementById('suppressPollInterval').value, 10);
        return (isNaN(v) || v < 5000) ? 30000 : v;
      })()
    };
  }

  function formatAgo(value) {
    const ago = Math.round((Date.now() - value) / 1000);
    if (ago <= 0) return 'now';
    if (ago < 60) return '-' + ago + 's';
    const m = Math.floor(ago / 60);
    const s = ago % 60;
    return s === 0 ? '-' + m + 'm' : '-' + m + 'm' + s + 's';
  }

  function formatSimTime(ms) {
    const totalSec = Math.floor(ms / 1000);
    const h = Math.floor(totalSec / 3600);
    const m = Math.floor((totalSec % 3600) / 60);
    const s = totalSec % 60;
    if (h > 0) return h + 'h ' + String(m).padStart(2, '0') + 'm ' + String(s).padStart(2, '0') + 's';
    if (m > 0) return m + 'm ' + String(s).padStart(2, '0') + 's';
    return s + 's';
  }

  function randomDuration(config) {
    const min = config.workDurationMinMs;
    const max = config.workDurationMaxMs;
    return min + Math.random() * (max - min);
  }

  function createWorkItem(config) {
    return { id: nextWorkId++, durationMs: randomDuration(config) };
  }

  function findFreeSlot() {
    for (let c = 0; c < consumers.length; c++) {
      if (consumers[c].readyAt !== undefined && simTime < consumers[c].readyAt) continue; // still cold-starting
      for (let s = 0; s < consumers[c].slots.length; s++) {
        if (consumers[c].slots[s] === null) return { consumer: consumers[c], index: s };
      }
    }
    return null;
  }

  function countInWindow(timestamps, windowMs, now) {
    return timestamps.filter(function (t) { return t >= now - windowMs; }).length;
  }

  function logEvent(outcome, label, rule) {
    creationHistory.unshift({
      time: new Date().toISOString(),
      outcome: outcome,
      label: label,
      rule: rule,
      consumersAfter: consumers.length
    });
  }

  function createChartConfig(datasets, yScales) {
    return {
      type: 'line',
      data: { datasets: datasets },
      options: {
        animation: false,
        responsive: true,
        maintainAspectRatio: false,
        interaction: { mode: 'index', intersect: false },
        plugins: {
          legend: {
            labels: { color: '#e6e8ec', font: { size: 11 }, boxWidth: 16, boxHeight: 2 }
          },
          tooltip: {
            callbacks: {
              title: function (items) {
                if (!items.length) return '';
                const ago = Math.round((Date.now() - items[0].parsed.x) / 1000);
                return ago <= 0 ? 'now' : ago + 's ago';
              }
            }
          }
        },
        scales: Object.assign({
          x: {
            type: 'linear',
            min: Date.now() - CHART_WINDOW_MS,
            max: Date.now(),
            ticks: {
              color: '#8b909a',
              font: { size: 11 },
              maxTicksLimit: 7,
              callback: formatAgo
            },
            grid: { color: '#3a3e48' }
          }
        }, yScales)
      }
    };
  }

  function assignWorkToSlots(config) {
    let throttled = false;
    while (queue.length > 0) {
      const free = findFreeSlot();
      if (!free) break;
      if (config.maxDispatchRate > 0) {
        const now = Date.now();
        const recentDispatches = countInWindow(dispatchTimestamps, 1000, now);
        if (recentDispatches >= config.maxDispatchRate) { throttled = true; break; }
      }
      const item = queue.shift();
      free.consumer.slots[free.index] = {
        startTime: simTime,
        endTime: simTime + item.durationMs,
        workItem: item
      };
      dispatchTimestamps.push(Date.now());
    }
    return throttled;
  }

  function currentDispatchRate(now) {
    return countInWindow(dispatchTimestamps, RATE_WINDOW_MS, now) / (RATE_WINDOW_MS / 1000);
  }

  function invokeWorker(rule) {
    const config = getConfig();

    if (consumers.length >= config.maxWorkers) {
      logEvent('max-reached', 'Max reached', rule);
      return false;
    }

    const slots = [];
    for (let i = 0; i < config.slotsPerConsumer; i++) slots.push(null);
    consumers.push({
      id: nextConsumerId++,
      createdAt: simTime,
      readyAt: simTime + config.coldStartMs,   // cold start: can't dispatch until warmed
      slots: slots
    });
    lastScaleUpTimeMs = Date.now();
    logEvent('created', 'Invoked', rule);
    assignWorkToSlots(config);
    return true;
  }

  function expireWorkers(config) {
    if (config.workerExitAfterMs <= 0 || consumers.length === 0) return;

    const survivors = [];
    const requeued = [];
    let expiredCount = 0;

    consumers.forEach(function (consumer) {
      if (simTime - consumer.createdAt < config.workerExitAfterMs) {
        survivors.push(consumer);
        return;
      }

      expiredCount++;
      consumer.slots.forEach(function (slot) {
        if (slot !== null) requeued.push(slot.workItem);
      });
    });

    if (expiredCount === 0) return;

    consumers = survivors;
    if (requeued.length > 0) queue = requeued.concat(queue);

    logEvent(
      'expired',
      'Expired',
      expiredCount + ' worker' + (expiredCount === 1 ? '' : 's') +
        ' reached exit time' +
        (requeued.length > 0 ? ', requeued ' + requeued.length + ' task' + (requeued.length === 1 ? '' : 's') : '')
    );
  }

  function processTaskAdd(noSyncCount) {
    if (noSyncCount <= 0) return;

    const config = getConfig();
    const now = Date.now();

    // flat dispatch rate detection: the fast path just obeys the poll's persisted suppression lease.
    if (now < suppressUntilMs) {
      logEvent('suppressed', 'Suppressed', 'Task-add gated (flat dispatch rate)');
      return;
    }

    const elapsed = now - lastScaleUpTimeMs;
    if (elapsed >= config.scaleUpCooloffMs) {
      invokeWorker('Task-add no-sync');
    } else {
      logEvent('throttled', 'Throttled', 'Task-add cooloff (' + noSyncCount + ' no-sync)');
    }
  }

  // Confirm whether dispatch is at a flat-rate ceiling; persists the verdict (flat_since, ref_rate, suppress lease)
  // that the fast path reads, and returns whether growth is currently suppressed. epsilon <= 0 clears the verdict and
  // returns false, reverting to today's behavior. Mirrors detectActivityCeiling in the Go algorithm (no branch-off).
  function detectFlatDispatch(config) {
    if (config.scaleUpDispatchRateEpsilon <= 0) {
      suppressUntilMs = 0; flatSinceMs = 0; refRate = -1;
      gatingStatus = 'off';
      return false;
    }

    const now = Date.now();
    const backlog = queue.length;
    const rate = currentDispatchRate(now);

    const material = backlog > config.scaleUpBacklogThreshold;
    const band = config.scaleUpDispatchRateEpsilon * refRate;         // RELATIVE band (fraction of dispatch)
    const moved = refRate >= 0 && Math.abs(rate - refRate) > band;    // dispatch rose OR dropped

    if (!material || moved || rate <= 0) {
      suppressUntilMs = 0; flatSinceMs = 0; refRate = -1;             // resume: clear the verdict
    } else if (flatSinceMs === 0) {
      flatSinceMs = now; refRate = rate;                             // anchor the bar; start confirming
    } else if (now - flatSinceMs >= config.dispatchConfirmMs) {
      suppressUntilMs = now + config.dispatchSuppressMs;             // confirmed flat dispatch -> suppress
    }

    lastDispatchRate = rate;
    const suppressed = suppressUntilMs > now;
    gatingStatus = suppressed ? 'suppressed' : (flatSinceMs !== 0 ? 'confirming' : 'clear');
    return suppressed;
  }

  // One scale-up per poll: growth (gated by the suppression verdict) OR maintenance (lifetime refresh, never gated).
  // Cadence backs off while suppressing. epsilon <= 0 -> detectFlatDispatch returns false -> exactly today's behavior.
  function processMetricsPoll() {
    const config = getConfig();
    const suppressed = detectFlatDispatch(config);
    nextPollIntervalMs = suppressed ? config.suppressPollIntervalMs : config.metricsPollIntervalMs;

    const now = Date.now();
    const backlog = queue.length;
    const growth = backlog > config.scaleUpBacklogThreshold && (now - lastScaleUpTimeMs) >= config.scaleUpCooloffMs;
    const maintenance = config.maxWorkerLifetimeMs > 0 && backlog > 0 && (now - lastScaleUpTimeMs) >= config.maxWorkerLifetimeMs;

    if (growth && !suppressed) {
      invokeWorker('Poll: backlog growth');
    } else if (maintenance) {
      invokeWorker('Poll: maintenance (lifetime refresh)');
    } else if (suppressed) {
      logEvent('suppressed', 'Suppressed', 'Poll: growth gated (flat dispatch rate)');
    } else if (backlog === 0) {
      logEvent('no-action', 'No action', 'Poll: queue empty');
    } else {
      logEvent('no-action', 'No action', 'Poll: ' + gatingStatus);
    }
  }

  function addItems(count) {
    const config = getConfig();
    const now = Date.now();
    for (let i = 0; i < count; i++) {
      arrivalTimestamps.push(now);
      queue.push(Object.assign(createWorkItem(config), { enqueuedAt: now }));
    }
    assignWorkToSlots(config);
    // a no-sync match this arrival → defer to the batch flush in tick() (the client batches no-sync signals)
    if (queue.length > 0) noSyncPending = true;
  }

  function freeCompletedSlots() {
    for (let c = 0; c < consumers.length; c++) {
      for (let s = 0; s < consumers[c].slots.length; s++) {
        const slot = consumers[c].slots[s];
        if (slot !== null && slot.endTime <= simTime) {
          consumers[c].slots[s] = null;
        }
      }
    }
  }

  function tick() {
    if (!running) return;
    const config = getConfig();
    const nowReal = Date.now();
    const realDelta = nowReal - lastRealTime;
    lastRealTime = nowReal;
    simTime += realDelta;

    freeCompletedSlots();
    expireWorkers(config);
    assignWorkToSlots(config);

    // fast path fires once per batch window (the client batches no-sync signals; fixed ~500ms)
    if (nowReal - lastBatchRealTime >= TASK_ADD_BATCH_MS) {
      lastBatchRealTime = nowReal;
      if (noSyncPending) processTaskAdd(1);
      noSyncPending = false;
    }

    const pollInterval = (config.scaleUpDispatchRateEpsilon > 0 && nextPollIntervalMs > 0)
      ? nextPollIntervalMs : config.metricsPollIntervalMs;
    if (nowReal - lastMetricsPollRealTime >= pollInterval) {
      lastMetricsPollRealTime = nowReal;
      processMetricsPoll();
    }

    render();
  }

  function arrivalTick() {
    if (!running) return;
    const config = getConfig();
    if (config.arrivalRate <= 0) return;
    arrivalAccumulator += config.arrivalRate * 0.1;
    const n = Math.floor(arrivalAccumulator);
    arrivalAccumulator -= n;
    if (n > 0) addItems(n);
  }

  // shade the Workers & backlog chart during flat-dispatch suppression windows
  const suppressionShadePlugin = {
    id: 'suppressionShade',
    beforeDatasetsDraw: function (c) {
      const area = c.chartArea;
      if (!area || chartData.length === 0) return;
      const ctx = c.ctx;
      const xs = c.scales.x;
      ctx.save();
      ctx.fillStyle = 'rgba(199, 146, 234, 0.13)';   // translucent purple = suppressed
      let start = null;
      for (let i = 0; i <= chartData.length; i++) {
        const sup = i < chartData.length && chartData[i].suppressed;
        if (sup && start === null) {
          start = chartData[i].t;
        } else if (!sup && start !== null) {
          const end = (i < chartData.length ? chartData[i] : chartData[i - 1]).t;
          const x0 = Math.max(area.left, xs.getPixelForValue(start));
          const x1 = Math.min(area.right, xs.getPixelForValue(end));
          if (x1 > x0) ctx.fillRect(x0, area.top, x1 - x0, area.bottom - area.top);
          start = null;
        }
      }
      ctx.restore();
    }
  };

  function initChart() {
    const canvas = document.getElementById('chart');
    if (!canvas) return;
    const cfg = createChartConfig(
      [
        {
          label: 'Workers',
          data: [],
          borderColor: '#5b8def',
          backgroundColor: 'transparent',
          borderWidth: 2,
          pointRadius: 0,
          yAxisID: 'yLeft',
          tension: 0
        },
        {
          label: 'Backlog',
          data: [],
          borderColor: '#ff9800',
          backgroundColor: 'transparent',
          borderWidth: 2,
          pointRadius: 0,
          yAxisID: 'yRight',
          tension: 0
        }
      ],
      {
        yLeft: {
          type: 'linear',
          position: 'left',
          min: 0,
          ticks: { color: '#5b8def', font: { size: 11 }, precision: 0 },
          grid: { color: '#3a3e48' }
        },
        yRight: {
          type: 'linear',
          position: 'right',
          min: 0,
          ticks: { color: '#ff9800', font: { size: 11 }, precision: 0 },
          grid: { drawOnChartArea: false }
        }
      }
    );
    cfg.plugins = [suppressionShadePlugin];
    chart = new Chart(canvas, cfg);
  }

  function renderChart() {
    if (!chart) return;
    const now = Date.now();
    chart.data.datasets[0].data = chartData.map(function (d) { return { x: d.t, y: d.consumers }; });
    chart.data.datasets[1].data = chartData.map(function (d) { return { x: d.t, y: d.queue }; });
    chart.options.scales.x.min = now - CHART_WINDOW_MS;
    chart.options.scales.x.max = now;
    chart.update('none');
  }

  function initRateChart() {
    const canvas = document.getElementById('rateChart');
    if (!canvas) return;
    rateChart = new Chart(canvas, createChartConfig(
      [
        {
          label: 'Arrival rate',
          data: [],
          borderColor: '#4caf50',
          backgroundColor: 'transparent',
          borderWidth: 2,
          pointRadius: 0,
          tension: 0
        },
        {
          label: 'Dispatch rate',
          data: [],
          borderColor: '#5b8def',
          backgroundColor: 'transparent',
          borderWidth: 2,
          pointRadius: 0,
          tension: 0
        }
      ],
      {
        y: {
          type: 'linear',
          position: 'left',
          min: 0,
          ticks: { color: '#8b909a', font: { size: 11 } },
          grid: { color: '#3a3e48' },
          title: { display: true, text: 'items/s', color: '#8b909a', font: { size: 11 } }
        }
      }
    ));
  }

  function renderRateChart() {
    if (!rateChart) return;
    const now = Date.now();
    rateChart.data.datasets[0].data = rateChartData.map(function (d) { return { x: d.t, y: d.arrivalRate }; });
    rateChart.data.datasets[1].data = rateChartData.map(function (d) { return { x: d.t, y: d.dispatchRate }; });
    rateChart.options.scales.x.min = now - CHART_WINDOW_MS;
    rateChart.options.scales.x.max = now;
    rateChart.update('none');
  }

  function render() {
    const config = getConfig();
    const now = Date.now();

    if (now - lastChartSampleTime >= CHART_SAMPLE_MS) {
      lastChartSampleTime = now;
      chartData.push({ t: now, consumers: consumers.length, queue: queue.length, suppressed: gatingStatus === 'suppressed' });
      chartData = chartData.filter(function (d) { return d.t >= now - CHART_WINDOW_MS; });
      rateChartData.push({
        t: now,
        arrivalRate: countInWindow(arrivalTimestamps, RATE_WINDOW_MS, now) / (RATE_WINDOW_MS / 1000),
        dispatchRate: countInWindow(dispatchTimestamps, RATE_WINDOW_MS, now) / (RATE_WINDOW_MS / 1000)
      });
      rateChartData = rateChartData.filter(function (d) { return d.t >= now - CHART_WINDOW_MS; });
    }
    renderChart();
    renderRateChart();

    arrivalTimestamps = arrivalTimestamps.filter(function (t) { return t >= now - RATE_TRIM_MS; });
    dispatchTimestamps = dispatchTimestamps.filter(function (t) { return t >= now - RATE_TRIM_MS; });

    const arrivalInWindow = countInWindow(arrivalTimestamps, RATE_WINDOW_MS, now);
    const dispatchInWindow = countInWindow(dispatchTimestamps, RATE_WINDOW_MS, now);
    const actualArrivalRate = (arrivalInWindow / (RATE_WINDOW_MS / 1000)).toFixed(2);
    const dispatchRateVal = (dispatchInWindow / (RATE_WINDOW_MS / 1000)).toFixed(2);

    let busySlots = 0;
    let totalSlots = 0;
    consumers.forEach(function (c) {
      c.slots.forEach(function (s) {
        totalSlots++;
        if (s !== null) busySlots++;
      });
    });

    const oldestAge = queue.length > 0
      ? ((now - queue[0].enqueuedAt) / 1000).toFixed(2)
      : '—';

    document.getElementById('queueDepth').textContent = queue.length;
    document.getElementById('consumerCount').textContent = consumers.length;
    document.getElementById('slotUtilization').textContent =
      busySlots + ' / ' + totalSlots + ' slots in use';
    document.getElementById('actualArrivalRate').textContent = arrivalTimestamps.length > 0 ? actualArrivalRate : '—';
    document.getElementById('dispatchRate').textContent = dispatchTimestamps.length > 0 ? dispatchRateVal : '—';
    document.getElementById('oldestQueueAge').textContent = oldestAge;

    const detectorEl = document.getElementById('detectorState');
    if (detectorEl) {
      const on = config.scaleUpDispatchRateEpsilon > 0;
      const state = on ? gatingStatus : 'off';
      detectorEl.textContent = on ? state : 'off (ε=0)';
      detectorEl.className = 'metric-value detector-' + state;
    }

    const container = document.getElementById('consumersSlots');
    if (consumers.length === 0) {
      container.innerHTML = '<p class="muted">No workers. Start the simulation or add tasks.</p>';
    } else {
      container.innerHTML = consumers.map(function (c) {
        const warming = c.readyAt !== undefined && simTime < c.readyAt;
        const bar = c.slots.map(function (s) {
          if (s !== null) {
            const tooltip = 'Task #' + s.workItem.id +
              '\nStart: ' + formatSimTime(s.startTime) +
              '\nEnd:   ' + formatSimTime(s.endTime);
            return '<span class="slot busy" title="' + tooltip + '"></span>';
          }
          return '<span class="slot' + (warming ? ' warming' : '') + '"></span>';
        }).join('');
        return (
          '<div class="consumer-row">' +
          '<span class="consumer-id">Worker ' + c.id + (warming ? ' ⏳' : '') + '</span>' +
          '<span class="slot-bar">' + bar + '</span>' +
          '</div>'
        );
      }).join('');
    }

    const tbody = document.getElementById('historyBody');
    if (creationHistory.length === 0) {
      tbody.innerHTML = '<tr><td colspan="4" class="muted">No events yet.</td></tr>';
    } else {
      tbody.innerHTML = creationHistory.map(function (e) {
        const time = new Date(e.time);
        const timeStr = time.toLocaleTimeString() + '.' + String(time.getMilliseconds()).padStart(3, '0');
        return (
          '<tr>' +
          '<td>' + timeStr + '</td>' +
          '<td><span class="history-outcome ' + e.outcome + '">' + e.label + '</span></td>' +
          '<td>' + (e.rule || '—') + '</td>' +
          '<td>' + e.consumersAfter + '</td>' +
          '</tr>'
        );
      }).join('');
    }
  }

  function start() {
    if (running) return;
    running = true;
    lastRealTime = Date.now();
    document.getElementById('btnStart').disabled = true;
    document.getElementById('btnPause').disabled = false;

    tickIntervalId = setInterval(tick, 100);
    arrivalIntervalId = setInterval(arrivalTick, 100);
  }

  function pause() {
    running = false;
    if (tickIntervalId) clearInterval(tickIntervalId);
    tickIntervalId = null;
    if (arrivalIntervalId) clearInterval(arrivalIntervalId);
    arrivalIntervalId = null;
    document.getElementById('btnStart').disabled = false;
    document.getElementById('btnPause').disabled = true;
  }

  function reset() {
    pause();
    queue = [];
    consumers = [];
    nextConsumerId = 1;
    nextWorkId = 1;
    lastScaleUpTimeMs = 0;
    lastMetricsPollRealTime = 0;
    lastDispatchRate = -1;
    flatSinceMs = 0;
    suppressUntilMs = 0;
    refRate = -1;
    nextPollIntervalMs = 0;
    gatingStatus = 'off';
    noSyncPending = false;
    lastBatchRealTime = 0;
    creationHistory = [];
    simTime = 0;
    lastRealTime = 0;
    arrivalAccumulator = 0;
    arrivalTimestamps = [];
    dispatchTimestamps = [];
    chartData = [];
    rateChartData = [];
    lastChartSampleTime = 0;
    render();
  }

  // One-click parameter presets. Realistic ceiling and its ε=0 baseline share the same workload
  // (arrival 30 vs dispatch cap 15) and differ only in the detector epsilon, so flipping between
  // them shows the detector's effect directly. Fills the inputs, resets, and starts.
  const PROFILES = {
    realistic: {
      slotsPerConsumer: 5, workDurationMin: 8, workDurationMax: 12,
      arrivalRate: 30, itemsPerClick: 1, maxDispatchRate: 15,
      workerExitAfter: 120, coldStart: 3, maxWorkers: 150,
      scaleUpCooloff: 200, metricsPollInterval: 15000, scaleUpBacklogThreshold: 200,
      maxWorkerLifetime: 90000, scaleUpDispatchRateEpsilon: 0.05,
      dispatchConfirm: 15000, dispatchSuppress: 60000, suppressPollInterval: 15000
    },
    baseline: {
      slotsPerConsumer: 5, workDurationMin: 8, workDurationMax: 12,
      arrivalRate: 30, itemsPerClick: 1, maxDispatchRate: 15,
      workerExitAfter: 120, coldStart: 3, maxWorkers: 150,
      scaleUpCooloff: 200, metricsPollInterval: 15000, scaleUpBacklogThreshold: 200,
      maxWorkerLifetime: 90000, scaleUpDispatchRateEpsilon: 0,
      dispatchConfirm: 15000, dispatchSuppress: 60000, suppressPollInterval: 15000
    }
  };

  function applyProfile(name) {
    const p = PROFILES[name];
    if (!p) return;
    Object.keys(p).forEach(function (id) {
      const el = document.getElementById(id);
      if (el) el.value = p[id];
    });
    reset();
    start();
  }

  document.querySelectorAll('.profile-btn').forEach(function (btn) {
    btn.addEventListener('click', function () { applyProfile(btn.getAttribute('data-profile')); });
  });

  document.getElementById('btnStart').addEventListener('click', start);
  document.getElementById('btnPause').addEventListener('click', pause);
  document.getElementById('btnReset').addEventListener('click', reset);
  document.getElementById('btnAddTasks').addEventListener('click', function () {
    const config = getConfig();
    addItems(config.itemsPerClick);
    render();
  });

  initChart();
  initRateChart();
  render();
})();
