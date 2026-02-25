(function () {
  let chart1 = null;
  let chart2 = null;

  const state = {
    queue: [],
    consumers: [],
    nextTaskId: 1,
    nextConsumerId: 1,
    lastConsumerCreatedAt: 0,
    lastConsumerTerminatedAt: 0,
    simTimeMs: 0,
    lastRealTime: null,
    lastArrivalSimTimeMs: 0,
    arrivalTimestamps: [],
    dispatchTimestamps: [],
    creationHistory: [],
    pendingScaleRequest: false,
    paused: true,
    ratesByConsumerCount: {},
    chartPoints: [],
    lastChartSampleMs: -Infinity
  };
  const RATE_WINDOW_MS = 10000;
  const WINDOW_SEC = RATE_WINDOW_MS / 1000;
  const CHART_SAMPLE_INTERVAL_MS = 500;
  const CHART_MAX_POINTS = 600;

  function getParam(id) {
    const el = document.getElementById(id);
    const n = parseFloat(el.value);
    return el.type === 'number' ? n : el.value;
  }

  function randomBetween(min, max) {
    return min + Math.random() * (max - min);
  }

  function computePeak(samples) {
    if (samples.length === 0) return 0;
    const sorted = samples.slice().sort((a, b) => a - b);
    return sorted[Math.ceil(0.99 * sorted.length) - 1];
  }

  function makeChartOptions() {
    return {
      animation: false,
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: {
          labels: {
            color: '#8b949e',
            font: { family: "'JetBrains Mono','SF Mono',monospace", size: 11 },
            boxWidth: 12,
            padding: 12
          }
        }
      },
      scales: {
        x: {
          ticks: { color: '#8b949e', font: { size: 10 }, maxTicksLimit: 8 },
          grid: { color: '#2d3a4d' }
        },
        y: {
          ticks: { color: '#8b949e', font: { size: 10 } },
          grid: { color: '#2d3a4d' },
          beginAtZero: true
        }
      }
    };
  }

  function initCharts() {
    chart1 = new Chart(document.getElementById('chartConsumersBacklog'), {
      type: 'line',
      data: {
        labels: [],
        datasets: [
          { label: 'Consumers', data: [], borderColor: '#58a6ff', backgroundColor: 'rgba(88,166,255,0.08)', borderWidth: 1.5, pointRadius: 0, tension: 0.3, fill: false },
          { label: 'Backlog', data: [], borderColor: '#d29922', backgroundColor: 'rgba(210,153,34,0.08)', borderWidth: 1.5, pointRadius: 0, tension: 0.3, fill: false }
        ]
      },
      options: makeChartOptions()
    });
    chart2 = new Chart(document.getElementById('chartRates'), {
      type: 'line',
      data: {
        labels: [],
        datasets: [
          { label: 'Arrival rate (/s)', data: [], borderColor: '#3fb950', backgroundColor: 'rgba(63,185,80,0.08)', borderWidth: 1.5, pointRadius: 0, tension: 0.3, fill: false },
          { label: 'Dispatch rate (/s)', data: [], borderColor: '#f85149', backgroundColor: 'rgba(248,81,73,0.08)', borderWidth: 1.5, pointRadius: 0, tension: 0.3, fill: false }
        ]
      },
      options: makeChartOptions()
    });
  }

  function addToQueue() {
    state.queue.push({
      id: state.nextTaskId++,
      enqueuedAt: state.simTimeMs
    });
    state.arrivalTimestamps.push(state.simTimeMs);
    state.pendingScaleRequest = true;
  }

  function tryCreateConsumer() {
    const maxConsumers = getParam('maxConsumers');
    const cooldownMs = getParam('scaleUpCooldownMs');
    const now = state.simTimeMs;
    if (state.consumers.length >= maxConsumers) {
      state.creationHistory.unshift({
        at: now,
        success: false,
        reason: `Scale blocked: max consumers reached (${state.consumers.length} >= ${maxConsumers})`
      });
      state.pendingScaleRequest = false;
      return false;
    }
    if (now - state.lastConsumerCreatedAt < cooldownMs) {
      return false;
    }
    const slotsPerConsumer = getParam('slotsPerConsumer');
    state.consumers.push({
      id: state.nextConsumerId++,
      slots: Array(slotsPerConsumer).fill(null),
      createdAt: now
    });
    state.lastConsumerCreatedAt = now;
    state.creationHistory.unshift({
      at: now,
      success: true,
      reason: 'New consumer created (queue had work, no free slot; cooldown elapsed)'
    });
    state.pendingScaleRequest = false;
    return true;
  }

  function assignWork() {
    const slotsPerConsumer = getParam('slotsPerConsumer');
    const workMin = getParam('workDurationMin') * 1000;
    const workMax = getParam('workDurationMax') * 1000;
    for (const consumer of state.consumers) {
      for (let i = 0; i < consumer.slots.length; i++) {
        if (consumer.slots[i] !== null) continue;
        if (state.queue.length === 0) break;
        const item = state.queue.shift();
        const duration = randomBetween(workMin, workMax);
        consumer.slots[i] = {
          itemId: item.id,
          enqueuedAt: item.enqueuedAt,
          startedAt: state.simTimeMs,
          duration
        };
        state.dispatchTimestamps.push(state.simTimeMs);
      }
    }
  }

  function completeWork() {
    for (const consumer of state.consumers) {
      for (let i = 0; i < consumer.slots.length; i++) {
        const slot = consumer.slots[i];
        if (slot === null) continue;
        if (state.simTimeMs >= slot.startedAt + slot.duration) {
          consumer.slots[i] = null;
        }
      }
    }
  }

  function hasFreeSlot() {
    for (const c of state.consumers) {
      if (c.slots.some(s => s === null)) return true;
    }
    return false;
  }

  function tryTerminateConsumer(currentDispatchRate) {
    const n = state.consumers.length;
    if (n <= 0) return false;
    const cooldownMs = getParam('scaleDownCooldownMs');
    const now = state.simTimeMs;
    if (now - state.lastConsumerTerminatedAt < cooldownMs) return false;
    const thresholdPct = getParam('scaleDownThresholdPct');
    const thresholdFrac = thresholdPct / 100;
    const samplesAtLower = state.ratesByConsumerCount[n - 1];
    const peakAtLower = samplesAtLower ? computePeak(samplesAtLower) : undefined;
    const shouldScaleDown = n === 1
      ? currentDispatchRate === 0
      : (peakAtLower !== undefined && currentDispatchRate < peakAtLower * thresholdFrac);
    if (!shouldScaleDown) return false;

    const byCreated = state.consumers.slice().sort((a, b) => b.createdAt - a.createdAt);
    const toRemove = byCreated[0];
    state.consumers = state.consumers.filter(c => c.id !== toRemove.id);
    state.lastConsumerTerminatedAt = now;
    const reason = n === 1
      ? 'Scaled down to 0 consumers (no dispatches)'
      : `Scaled down: rate ${currentDispatchRate.toFixed(1)}/s below ${thresholdPct}% of peak for ${n - 1} consumers (${(peakAtLower * thresholdFrac).toFixed(1)}/s, peak ${peakAtLower.toFixed(1)}/s)`;
    state.creationHistory.unshift({
      at: now,
      success: true,
      terminated: true,
      reason
    });
    return true;
  }

  function tick(deltaRealMs) {
    state.simTimeMs += deltaRealMs;

    const rate = getParam('arrivalRate');
    if (rate > 0) {
      const simSecSinceLastArrival = (state.simTimeMs - state.lastArrivalSimTimeMs) / 1000;
      const toAdd = Math.floor(simSecSinceLastArrival * rate);
      if (toAdd > 0) {
        state.lastArrivalSimTimeMs += (toAdd / rate) * 1000;
        for (let i = 0; i < toAdd; i++) addToQueue();
      }
    }

    completeWork();
    assignWork();

    const cutoff = state.simTimeMs - RATE_WINDOW_MS;
    const dispatchesInWindow = state.dispatchTimestamps.filter(t => t >= cutoff).length;
    const currentDispatchRate = WINDOW_SEC > 0 ? dispatchesInWindow / WINDOW_SEC : 0;
    const n = state.consumers.length;
    if (n > 0) {
      if (!state.ratesByConsumerCount[n]) state.ratesByConsumerCount[n] = [];
      state.ratesByConsumerCount[n].push(currentDispatchRate);
    }
    tryTerminateConsumer(currentDispatchRate);

    if (state.queue.length > 0 && !hasFreeSlot()) state.pendingScaleRequest = true;
    if (state.pendingScaleRequest && state.queue.length > 0 && !hasFreeSlot()) {
      tryCreateConsumer();
    }
  }

  function updateUI() {
    const totalSlots = state.consumers.reduce((acc, c) => acc + c.slots.length, 0);
    const busySlots = state.consumers.reduce((acc, c) => acc + c.slots.filter(s => s !== null).length, 0);

    document.getElementById('metricQueueDepth').textContent = state.queue.length;
    const oldest = state.queue.length > 0
      ? (state.simTimeMs - Math.min(...state.queue.map(q => q.enqueuedAt))) / 1000
      : 0;
    document.getElementById('metricOldestAge').textContent = oldest.toFixed(1);
    const cutoff = state.simTimeMs - RATE_WINDOW_MS;
    state.arrivalTimestamps = state.arrivalTimestamps.filter(t => t >= cutoff);
    state.dispatchTimestamps = state.dispatchTimestamps.filter(t => t >= cutoff);
    const arrivalsInWindow = state.arrivalTimestamps.length;
    const dispatchesInWindow = state.dispatchTimestamps.length;
    document.getElementById('metricArrivalRate').textContent = (arrivalsInWindow / WINDOW_SEC).toFixed(1);
    document.getElementById('metricDispatchRate').textContent = (dispatchesInWindow / WINDOW_SEC).toFixed(1);
    document.getElementById('metricConsumers').textContent = state.consumers.length;

    if (state.simTimeMs - state.lastChartSampleMs >= CHART_SAMPLE_INTERVAL_MS) {
      state.lastChartSampleMs = state.simTimeMs;
      const pts = state.chartPoints;
      pts.push({
        label: (state.simTimeMs / 1000).toFixed(1),
        consumerCount: state.consumers.length,
        queueDepth: state.queue.length,
        arrivalRate: arrivalsInWindow / WINDOW_SEC,
        dispatchRate: dispatchesInWindow / WINDOW_SEC
      });
      if (pts.length > CHART_MAX_POINTS) pts.shift();
      const labels = pts.map(p => p.label);
      chart1.data.labels = labels;
      chart1.data.datasets[0].data = pts.map(p => p.consumerCount);
      chart1.data.datasets[1].data = pts.map(p => p.queueDepth);
      chart1.update('none');
      chart2.data.labels = labels;
      chart2.data.datasets[0].data = pts.map(p => p.arrivalRate);
      chart2.data.datasets[1].data = pts.map(p => p.dispatchRate);
      chart2.update('none');
    }
    document.getElementById('metricUtilization').textContent = totalSlots ? Math.round((busySlots / totalSlots) * 100) + '%' : '0%';

    const list = document.getElementById('consumersList');
    list.innerHTML = state.consumers.map(c => {
      const busy = c.slots.filter(s => s !== null).length;
      const slotsHtml = c.slots.map(s => s === null ? '<span class="slot free"></span>' : '<span class="slot busy"></span>').join('');
      return `<div class="consumer"><div class="consumer-header"><span class="consumer-id">Consumer #${c.id}</span><span>${busy}/${c.slots.length} slots</span></div><div class="slots">${slotsHtml}</div></div>`;
    }).join('') || '<div class="consumer">No consumers yet</div>';

    const currentCount = state.consumers.length;
    const rateEntries = Object.keys(state.ratesByConsumerCount)
      .map(Number)
      .sort((a, b) => a - b);
    const peakBody = document.getElementById('peakRateBody');
    if (rateEntries.length === 0) {
      peakBody.innerHTML = '<tr><td colspan="2" class="empty">No data yet</td></tr>';
    } else {
      peakBody.innerHTML = rateEntries.map(n => {
        const cls = n === currentCount ? ' class="current-count"' : '';
        return `<tr${cls}><td>${n}</td><td>${computePeak(state.ratesByConsumerCount[n]).toFixed(1)}</td></tr>`;
      }).join('');
    }

    const historyEl = document.getElementById('creationHistory');
    historyEl.innerHTML = state.creationHistory.slice(0, 50).map(h => {
      const t = (h.at / 1000).toFixed(1) + 's';
      const cls = h.terminated ? ' terminated' : (h.success ? '' : ' failed');
      return `<div class="history-entry${cls}">[${t}] <span class="reason">${h.reason}</span></div>`;
    }).join('') || '<div class="history-entry">No events yet</div>';
  }

  function runLoop(now) {
    if (state.paused) {
      state.lastRealTime = now;
      updateUI();
      requestAnimationFrame(runLoop);
      return;
    }
    if (state.lastRealTime !== null) {
      tick(now - state.lastRealTime);
    }
    state.lastRealTime = now;
    updateUI();
    requestAnimationFrame(runLoop);
  }

  function updatePauseButton() {
    const btn = document.getElementById('pauseBtn');
    btn.textContent = state.paused ? 'Start' : 'Pause';
  }

  document.getElementById('addTaskBtn').addEventListener('click', function () {
    addToQueue();
  });

  document.getElementById('pauseBtn').addEventListener('click', function () {
    state.paused = !state.paused;
    updatePauseButton();
  });

  updatePauseButton();
  initCharts();
  requestAnimationFrame(runLoop);
})();
