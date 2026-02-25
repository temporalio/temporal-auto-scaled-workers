(function () {
  let chart1 = null;
  let chart2 = null;

  const state = {
    queue: [],
    consumers: [],
    nextTaskId: 1,
    nextConsumerId: 1,
    simTimeMs: 0,
    lastRealTime: null,
    lastArrivalSimTimeMs: 0,
    arrivalTimestamps: [],
    dispatchTimestamps: [],
    paused: true,
    chartPoints: [],
    lastChartSampleMs: -Infinity,
    pid: { integral: 0, prevError: 0, lastRunMs: -Infinity, output: 0 },
    scaleLog: []
  };
  const RATE_WINDOW_MS = 10000;
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
          { label: 'Backlog', data: [], borderColor: '#d29922', backgroundColor: 'rgba(210,153,34,0.08)', borderWidth: 1.5, pointRadius: 0, tension: 0.3, fill: false },
          { label: 'PID output', data: [], borderColor: '#bc8cff', backgroundColor: 'rgba(188,140,255,0.06)', borderWidth: 1, borderDash: [4, 3], pointRadius: 0, tension: 0.3, fill: false }
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

  function tickPID() {
    const intervalMs = getParam('pidIntervalMs');
    if (state.simTimeMs - state.pid.lastRunMs < intervalMs) return;
    const dt = state.pid.lastRunMs === -Infinity
      ? intervalMs / 1000
      : (state.simTimeMs - state.pid.lastRunMs) / 1000;
    state.pid.lastRunMs = state.simTimeMs;

    const kp = getParam('kp');
    const ki = getParam('ki');
    const kd = getParam('kd');
    const maxConsumers = getParam('maxConsumers');

    const error = state.queue.length - getParam('pidSetpoint');

    state.pid.integral += error * dt;
    if (ki > 0) {
      state.pid.integral = Math.min(state.pid.integral, maxConsumers / ki);
      state.pid.integral = Math.max(state.pid.integral, -maxConsumers / ki);
    }

    const derivative = (error - state.pid.prevError) / dt;
    state.pid.prevError = error;

    const raw = kp * error + ki * state.pid.integral + kd * derivative;
    state.pid.output = raw;

    const desired = Math.max(0, Math.min(maxConsumers, Math.round(raw)));
    const current = state.consumers.length;
    const slotsPerConsumer = getParam('slotsPerConsumer');

    if (desired > current) {
      for (let i = 0; i < desired - current; i++) {
        state.consumers.push({
          id: state.nextConsumerId++,
          slots: Array(slotsPerConsumer).fill(null),
          createdAt: state.simTimeMs
        });
      }
      state.scaleLog.unshift({
        at: state.simTimeMs,
        direction: 'up',
        from: current,
        to: desired,
        reason: `backlog=${state.queue.length}, error=${error.toFixed(1)}, output=${raw.toFixed(2)}`
      });
      if (state.scaleLog.length > 50) state.scaleLog.pop();
    } else if (desired < current) {
      const sorted = state.consumers.slice().sort((a, b) => b.createdAt - a.createdAt);
      for (let i = 0; i < current - desired; i++) {
        const c = sorted[i];
        for (const slot of c.slots) {
          if (slot !== null) state.queue.unshift({ id: slot.itemId, enqueuedAt: slot.enqueuedAt });
        }
        state.consumers = state.consumers.filter(x => x.id !== c.id);
      }
      state.scaleLog.unshift({
        at: state.simTimeMs,
        direction: 'down',
        from: current,
        to: desired,
        reason: `backlog=${state.queue.length}, error=${error.toFixed(1)}, output=${raw.toFixed(2)}`
      });
      if (state.scaleLog.length > 50) state.scaleLog.pop();
    }
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
    tickPID();
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
    const windowSec = RATE_WINDOW_MS / 1000;
    document.getElementById('metricArrivalRate').textContent = (arrivalsInWindow / windowSec).toFixed(1);
    document.getElementById('metricDispatchRate').textContent = (dispatchesInWindow / windowSec).toFixed(1);
    document.getElementById('metricConsumers').textContent = state.consumers.length;

    if (state.simTimeMs - state.lastChartSampleMs >= CHART_SAMPLE_INTERVAL_MS) {
      state.lastChartSampleMs = state.simTimeMs;
      const pts = state.chartPoints;
      pts.push({
        label: (state.simTimeMs / 1000).toFixed(1),
        consumerCount: state.consumers.length,
        queueDepth: state.queue.length,
        arrivalRate: arrivalsInWindow / windowSec,
        dispatchRate: dispatchesInWindow / windowSec,
        pidOutput: state.pid.output
      });
      if (pts.length > CHART_MAX_POINTS) pts.shift();
      const labels = pts.map(p => p.label);
      chart1.data.labels = labels;
      chart1.data.datasets[0].data = pts.map(p => p.consumerCount);
      chart1.data.datasets[1].data = pts.map(p => p.queueDepth);
      chart1.data.datasets[2].data = pts.map(p => p.pidOutput);
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

    const logEl = document.getElementById('scaleLog');
    logEl.innerHTML = state.scaleLog.slice(0, 50).map(e => {
      const t = (e.at / 1000).toFixed(1) + 's';
      const dir = e.direction === 'up' ? '▲' : '▼';
      const cls = e.direction === 'up' ? 'scale-up' : 'scale-down';
      return `<div class="history-entry ${cls}">[${t}] ${dir} ${e.from} → ${e.to} consumers &nbsp;<span class="reason">${e.reason}</span></div>`;
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
