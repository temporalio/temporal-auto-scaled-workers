(function () {
  'use strict';

  let queue = [];
  let consumers = [];
  let lastConsumerCreationRealTime = 0;
  let creationHistory = [];
  let running = false;
  let simTime = 0;
  let lastRealTime = 0;
  let tickIntervalId = null;
  let arrivalIntervalId = null;
  let arrivalAccumulator = 0;
  let nextWorkId = 1;
  let nextConsumerId = 1;
  let lastScaleCheckRealTime = 0;
  let lastScaleCheckDispatchRate = -1;
  let arrivalTimestamps = [];
  let dispatchTimestamps = [];
  let chartData = [];
  let lastChartSampleTime = 0;
  let chart = null;
  let rateChartData = [];
  let rateChart = null;
  const RATE_WINDOW_MS = 10000;
  const RATE_TRIM_MS = 60000;
  const CHART_WINDOW_MS = 300000;
  const CHART_SAMPLE_MS = 500;

  function getConfig() {
    return {
      slotsPerConsumer: Math.max(1, parseInt(document.getElementById('slotsPerConsumer').value, 10) || 5),
      workDurationMinMs: (parseFloat(document.getElementById('workDurationMin').value) || 30) * 1000,
      workDurationMaxMs: (parseFloat(document.getElementById('workDurationMax').value) || 60) * 1000,
      consumerLifespanMs: (parseFloat(document.getElementById('consumerLifespan').value) || 120) * 1000,
      creationThrottleMs: parseInt(document.getElementById('creationThrottle').value, 10) || 500,
      maxConsumers: Math.max(1, parseInt(document.getElementById('maxConsumers').value, 10) || 10),
      arrivalRate: (function () {
        var v = parseFloat(document.getElementById('arrivalRate').value);
        return (isNaN(v) || v < 0) ? 5 : v;
      })(),
      itemsPerClick: Math.max(1, parseInt(document.getElementById('itemsPerClick').value, 10) || 1),
      scaleCheckIntervalMs: (parseFloat(document.getElementById('scaleCheckInterval').value) || 60) * 1000,
      queueDepthThreshold: (function () {
        var v = parseInt(document.getElementById('queueDepthThreshold').value, 10);
        return (isNaN(v) || v < 0) ? 10 : v;
      })(),
      consumerCreationFailurePct: (function () {
        var v = parseFloat(document.getElementById('consumerCreationFailurePct').value);
        return (isNaN(v) || v < 0) ? 10 : Math.min(100, Math.max(0, v));
      })(),
      maxDispatchRate: (function () {
        var v = parseFloat(document.getElementById('maxDispatchRate').value);
        return (isNaN(v) || v < 0) ? 0 : v;
      })(),
      dispatchRatePrecision: (function () {
        var v = parseFloat(document.getElementById('dispatchRatePrecision').value);
        return (isNaN(v) || v < 0) ? 0 : v;
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

  function tryCreateConsumer(rule) {
    const config = getConfig();
    const nowReal = Date.now();

    if (consumers.length >= config.maxConsumers) {
      logEvent('max-reached', 'Max reached', rule);
      return false;
    }

    if (nowReal - lastConsumerCreationRealTime < config.creationThrottleMs) {
      logEvent('throttled', 'Throttled', rule);
      return false;
    }

    if (config.consumerCreationFailurePct > 0 && Math.random() * 100 < config.consumerCreationFailurePct) {
      logEvent('failed', 'Failed', rule);
      return false;
    }

    const slots = [];
    for (let i = 0; i < config.slotsPerConsumer; i++) slots.push(null);
    consumers.push({
      id: nextConsumerId++,
      createdAt: simTime,
      slots: slots
    });
    lastConsumerCreationRealTime = nowReal;
    logEvent('created', 'Created', rule);
    assignWorkToSlots(config);
    return true;
  }

  function addItems(count) {
    const config = getConfig();
    const now = Date.now();
    for (let i = 0; i < count; i++) {
      arrivalTimestamps.push(now);
      queue.push(Object.assign(createWorkItem(config), { enqueuedAt: now }));
    }
    const throttled = assignWorkToSlots(config);
    if (!throttled && queue.length > 0) {
      tryCreateConsumer('Add items (queue non-empty)');
    }
  }

  function removeDeadConsumers(config) {
    const toRemove = [];
    for (let i = 0; i < consumers.length; i++) {
      if (simTime - consumers[i].createdAt >= config.consumerLifespanMs) {
        toRemove.push(i);
      }
    }
    const now = Date.now();
    for (let i = toRemove.length - 1; i >= 0; i--) {
      const c = consumers[toRemove[i]];
      for (let s = 0; s < c.slots.length; s++) {
        if (c.slots[s] !== null) {
          queue.push(Object.assign({}, c.slots[s].workItem, { enqueuedAt: now }));
        }
      }
      consumers.splice(toRemove[i], 1);
      logEvent('stopped', 'Stopped', 'Lifespan elapsed');
    }
    if (toRemove.length > 0) {
      assignWorkToSlots(config);
    }
  }

  function freeCompletedSlots(config) {
    for (let c = 0; c < consumers.length; c++) {
      for (let s = 0; s < consumers[c].slots.length; s++) {
        const slot = consumers[c].slots[s];
        if (slot !== null && slot.endTime <= simTime) {
          consumers[c].slots[s] = null;
        }
      }
    }
    return assignWorkToSlots(config);
  }

  function tick() {
    if (!running) return;
    const config = getConfig();
    const nowReal = Date.now();
    const realDelta = nowReal - lastRealTime;
    lastRealTime = nowReal;
    simTime += realDelta;

    removeDeadConsumers(config);
    const dispatchThrottled = freeCompletedSlots(config);

    if (nowReal - lastScaleCheckRealTime >= config.scaleCheckIntervalMs) {
      lastScaleCheckRealTime = nowReal;
      const currentDispatchCount = countInWindow(dispatchTimestamps, RATE_WINDOW_MS, nowReal);
      const currentDispatchRate = currentDispatchCount / (RATE_WINDOW_MS / 1000);
      const dispatchRateStable = currentDispatchRate > 0 && lastScaleCheckDispatchRate >= 0 &&
        Math.abs(currentDispatchRate - lastScaleCheckDispatchRate) <= config.dispatchRatePrecision;
      lastScaleCheckDispatchRate = currentDispatchRate;
      if (dispatchRateStable) {
        logEvent('no-action', 'No action', 'Dispatch rate unchanged');
      } else if (queue.length > config.queueDepthThreshold) {
        tryCreateConsumer('Queue depth > threshold');
      } else if (queue.length > 0 && (nowReal - lastConsumerCreationRealTime) >= config.consumerLifespanMs) {
        tryCreateConsumer('Lifespan elapsed with pending queue');
      } else {
        logEvent('no-action', 'No action', queue.length === 0 ? 'Scale check: queue empty' : 'Scale check: queue below threshold');
      }
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

  function initChart() {
    const canvas = document.getElementById('chart');
    if (!canvas) return;
    chart = new Chart(canvas, createChartConfig(
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
    ));
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
      chartData.push({ t: now, consumers: consumers.length, queue: queue.length });
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

    const container = document.getElementById('consumersSlots');
    if (consumers.length === 0) {
      container.innerHTML = '<p class="muted">No consumers. Start the simulation or add tasks.</p>';
    } else {
      container.innerHTML = consumers.map(function (c) {
        const bar = c.slots.map(function (s) {
          if (s !== null) {
            const tooltip = 'Task #' + s.workItem.id +
              '\nStart: ' + formatSimTime(s.startTime) +
              '\nEnd:   ' + formatSimTime(s.endTime);
            return '<span class="slot busy" title="' + tooltip + '"></span>';
          }
          return '<span class="slot"></span>';
        }).join('');
        return (
          '<div class="consumer-row">' +
          '<span class="consumer-id">Consumer ' + c.id + '</span>' +
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
    lastConsumerCreationRealTime = 0;
    lastScaleCheckRealTime = 0;
    lastScaleCheckDispatchRate = -1;
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
