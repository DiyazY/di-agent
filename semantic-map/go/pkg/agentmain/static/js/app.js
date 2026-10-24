// Bootstrap: the poll loop, the census strip, and the small bits of chrome
// (theme, toasts, the prompt dialog, keyboard) that the other modules borrow.
//
// The loop is deliberately a poll rather than a stream. The agent exposes no
// event socket, and adding one to make a console feel live would be the tail
// wagging the dog; a 2s GET of /state is a few kilobytes and costs the edge node
// nothing it would notice. What makes the page feel alive is not the transport —
// it is that the revision, the journal and the value arcs all move together,
// because they are all read from the same snapshot.

import { api } from './api.js';
import { Model, fmt, escapeHTML } from './model.js';
import { GraphView, tooltipHTML } from './graph.js';
import { Inspector } from './inspector.js';
import { Drawer } from './drawer.js';
import { censusBar } from './charts.js';

const model = new Model();
let timer = null;
let live = true;
let interval = 2000;
let lastRevision = -1;
let failures = 0;
let inflight = false;
let groupTicks = 0; // GET /groups every tenth poll: the topology moves with pods, not with values
let statusFilter = 'live';

/**
 * What each status choice asks the agent for.
 *
 * This has to travel in the request. GET /state with no status defaults to
 * active+stale server-side, so a retired property never reaches the page at all
 * — filtering the response could only ever hide things, never reveal them, which
 * left "retired only" permanently empty and made "everything" a synonym for
 * "active + stale".
 */
const STATUS_QUERY = {
  live: undefined,                               // the server's own default
  all: ['active', 'stale', 'retired'],
  active: ['active'],
  stale: ['stale'],
  retired: ['retired'],
};

// ── chrome helpers ───────────────────────────────────────────────────────────

const toastHost = document.getElementById('toasts');
function toast(message, kind = '') {
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.textContent = message;
  toastHost.append(el);
  setTimeout(() => el.remove(), kind === 'err' ? 6000 : 3000);
}

const tipEl = document.getElementById('tooltip');
const tooltip = {
  show(e, spec) {
    tipEl.innerHTML = tooltipHTML(spec);
    tipEl.classList.add('show');
    tipEl.setAttribute('aria-hidden', 'false');
    const pad = 14;
    const r = tipEl.getBoundingClientRect();
    let x = e.clientX + pad;
    let y = e.clientY + pad;
    if (x + r.width > window.innerWidth - 8) x = e.clientX - r.width - pad;
    if (y + r.height > window.innerHeight - 8) y = e.clientY - r.height - pad;
    tipEl.style.left = `${Math.max(8, x)}px`;
    tipEl.style.top = `${Math.max(8, y)}px`;
  },
  hide() {
    tipEl.classList.remove('show');
    tipEl.setAttribute('aria-hidden', 'true');
  },
};

/** A promise-shaped modal, so a caller can `await` an operator's answer. */
const dialog = document.getElementById('prompt-dialog');
function ask({ title, fields, hint, ok = 'Apply' }) {
  return new Promise((resolve) => {
    document.getElementById('prompt-title').textContent = title;
    const body = document.getElementById('prompt-body');
    body.innerHTML = fields.map((f) => `
      <label>
        <span>${escapeHTML(f.label)}</span>
        <input name="${escapeHTML(f.name)}" type="${f.type || 'text'}"
               ${f.step ? `step="${f.step}"` : ''} ${f.min !== undefined ? `min="${f.min}"` : ''}
               ${f.max !== undefined ? `max="${f.max}"` : ''}
               value="${escapeHTML(f.value ?? '')}" placeholder="${escapeHTML(f.placeholder || '')}" />
      </label>`).join('') + (hint ? `<p class="hint">${escapeHTML(hint)}</p>` : '');
    document.getElementById('prompt-ok').textContent = ok;

    const done = (value) => {
      dialog.removeEventListener('close', onClose);
      okBtn.removeEventListener('click', onOK);
      cancelBtn.removeEventListener('click', onCancel);
      if (dialog.open) dialog.close();
      resolve(value);
    };
    const collect = () => Object.fromEntries(
      [...body.querySelectorAll('input')].map((i) => [i.name, i.value]));

    const okBtn = document.getElementById('prompt-ok');
    const cancelBtn = document.getElementById('prompt-cancel');
    const onOK = () => done(collect());
    const onCancel = () => done(null);
    const onClose = () => done(null);

    okBtn.addEventListener('click', onOK);
    cancelBtn.addEventListener('click', onCancel);
    dialog.addEventListener('close', onClose);
    dialog.showModal();
    body.querySelector('input')?.focus();
  });
}

// ── the three views ──────────────────────────────────────────────────────────

const graph = new GraphView(document.getElementById('graph'), {
  tooltip,
  onSelect: (sel) => inspector.show(sel),
  onHighlightCleared: () => drawer.unpoint(),
});

const inspector = new Inspector(document.getElementById('inspector'), {
  model: () => model,
  toast,
  ask,
  refresh: () => tick(),
  selectProperty: (id) => graph.select(id ? { type: 'property', id } : null),
  selectRelationship: (id) => graph.select({ type: 'relationship', id }),
  openDecision: (id) => drawer.openDecision(id),
});

const drawer = new Drawer(document.getElementById('drawer'), {
  model: () => model,
  toast,
  ask,
  refresh: () => tick(),
  selectProperty: (id) => graph.select({ type: 'property', id }),
  highlight: (spec) => graph.highlight(spec),
  filterSubject: (subject) => {
    const input = document.getElementById('search');
    input.value = subject;
    graph.setFilter({ text: subject });
  },
});

// ── the census strip ─────────────────────────────────────────────────────────

const STAT_SPECS = [
  {
    key: 'properties',
    label: 'Properties',
    value: (c) => c.properties_total ?? 0,
    sub: (c) => `${c.properties_active ?? 0} active · ${c.properties_stale ?? 0} stale · ${c.properties_retired ?? 0} retired`,
    bar: (c) => [
      { label: 'active', n: c.properties_active ?? 0, color: 'var(--status-good)' },
      { label: 'stale', n: c.properties_stale ?? 0, color: 'var(--status-serious)' },
      { label: 'retired', n: c.properties_retired ?? 0, color: 'var(--ink-muted)' },
    ],
  },
  {
    key: 'shape',
    label: 'Observed / derived',
    value: (c) => `${c.observed ?? 0}/${c.derived ?? 0}`,
    sub: (c) => `${c.properties_unobserved ?? 0} never observed`,
  },
  {
    key: 'subjects',
    label: 'Subjects',
    value: (c) => c.subjects ?? 0,
    sub: () => 'narrower than the node',
  },
  {
    key: 'relationships',
    label: 'Relationships',
    value: (c) => c.relationships_total ?? 0,
    sub: (c) => `${c.relationships_live ?? 0} live`,
    bar: (c) => [
      { label: 'seeded', n: c.seeded ?? 0, color: 'var(--prov-seeded)' },
      { label: 'learned', n: c.learned ?? 0, color: 'var(--prov-learned)' },
      { label: 'discovered', n: c.discovered ?? 0, color: 'var(--prov-discovered)' },
      { label: 'asserted', n: c.asserted ?? 0, color: 'var(--prov-asserted)' },
    ],
  },
  {
    key: 'discovered',
    label: 'Discovered',
    value: (c) => c.discovered ?? 0,
    sub: () => 'structure nobody declared',
  },
  {
    key: 'suspect',
    label: 'Sign suspect',
    value: (c) => c.relationships_sign_suspect ?? 0,
    sub: (c) => ((c.relationships_sign_suspect ?? 0) > 0
      ? 'a declared direction the machine refutes'
      : 'no claim is being contradicted'),
    alarm: (c) => (c.relationships_sign_suspect ?? 0) > 0,
  },
];

const statsHost = document.getElementById('stats');
const statEls = new Map();

function buildStats() {
  statsHost.replaceChildren();
  for (const spec of STAT_SPECS) {
    const el = document.createElement('div');
    el.className = 'stat';
    // The census bar is created here, empty, rather than on first data: a tile
    // that grows a row once the first poll lands resizes the canvas under the
    // graph, and the layout below reads that as a reason to re-settle.
    el.innerHTML = `<span class="label">${escapeHTML(spec.label)}</span>
      <span class="value">—</span><span class="sub"></span>
      ${spec.bar ? '<div class="censusbar"></div>' : '<div class="censusbar" style="visibility:hidden"></div>'}`;
    statsHost.append(el);
    statEls.set(spec.key, el);
  }
}

function renderStats(counts) {
  for (const spec of STAT_SPECS) {
    const el = statEls.get(spec.key);
    const valueEl = el.querySelector('.value');
    const next = String(spec.value(counts));
    if (valueEl.textContent !== next && valueEl.textContent !== '—') {
      valueEl.classList.remove('changed');
      void valueEl.offsetWidth;          // restart the animation
      valueEl.classList.add('changed');
    }
    valueEl.textContent = next;
    el.querySelector('.sub').textContent = spec.sub(counts);
    el.classList.toggle('alarm', !!spec.alarm?.(counts));

    if (spec.bar) {
      const bar = el.querySelector('.censusbar');
      bar.replaceChildren(...censusBar(spec.bar(counts)).children);
    }
  }
}

// ── the poll loop ────────────────────────────────────────────────────────────

async function tick() {
  // One poll at a time. Two overlapping requests can resolve out of order, and
  // ingesting the older snapshot second rewinds the revision, flashes the badge
  // and pushes a stale point into every sparkline.
  if (inflight) return;
  inflight = true;
  try {
    const view = await api.state({ status: STATUS_QUERY[statusFilter] });
    model.ingest(view);
    failures = 0;
    setHealth('ok', `agent ok`);

    document.getElementById('owner').textContent = view.owner || 'unidentified node';
    const revEl = document.getElementById('revision');
    document.getElementById('revision-n').textContent = view.revision;
    if (lastRevision !== -1 && view.revision !== lastRevision) {
      revEl.classList.remove('bumped');
      void revEl.offsetWidth;
      revEl.classList.add('bumped');
      setTimeout(() => revEl.classList.remove('bumped'), 700);
    }
    lastRevision = view.revision;

    // Hull names and badges come from the acting endpoints; only the first tick waits.
    const acting = refreshActing();
    if (!actingAwaited) {
      actingAwaited = true;
      await Promise.race([acting, new Promise((r) => setTimeout(r, 1500))]);
    }
    if (groupTicks++ % 10 === 0) {
      try { model.topology = await api.groups(); } catch { /* keep the last; boxes fall back to subjects */ }
    }
    renderStats(model.counts);
    graph.render(model);
    inspector.refresh();
    await drawer.poll();
  } catch (err) {
    failures++;
    setHealth('down', failures > 2 ? 'agent unreachable' : 'retrying…');
    if (failures === 1) toast(`Lost the agent: ${err.message}`, 'err');
  } finally {
    inflight = false;
  }
}

/**
 * The acting endpoints, read alongside /state so a hull's name and badge match the
 * Actions pane. They never hold the map back: only the first tick waits for them
 * (briefly, so the drawer can choose its default pane); after that the last known
 * answer is drawn and a changed one redraws when it lands.
 *
 * Polling is as light as the answers allow. /relief and /relief/actions every tick;
 * /workloads (names and frozen state change slowly) every fourth tick, or at once
 * when the map's set of subjects changed. A 404 from /relief means relief is off in
 * the spec; then only /relief is asked again, every tenth tick, so an agent
 * restarted with relief on is noticed without a reload. A failure keeps what was
 * last known rather than blanking the names, and says it is stale.
 */
const OFF_PROBE_EVERY = 10;

/**
 * GET /relief answers one status per lever, in lever order; the Actions pane and
 * the hulls read one. The counts are summed, the cooling workloads merged, and the
 * phase names each lever's when there is more than one. The levers stay, as given.
 */
function reliefSummary({ levers = [] }) {
  const sum = (k) => levers.reduce((n, l) => n + (l[k] || 0), 0);
  const kept = sum('kept'), reverted = sum('reverted');
  return {
    levers,
    phase: levers.map((l) => (levers.length > 1 ? `${l.action} ${l.phase}` : l.phase)).join(', '),
    paused: levers.length > 0 && levers.every((l) => l.paused),
    runtime: levers[0]?.runtime,
    actions: sum('actions'), kept, reverted, refused: sum('refused'), stuck: sum('stuck'),
    predictions_held: kept + reverted > 0 ? kept / (kept + reverted) : 0,
    cooling: Object.assign({}, ...levers.map((l) => l.cooling || {})),
  };
}
let actingTicks = 0;
let offTicks = 0;
let actingBusy = false;
let actingAwaited = false;
let workloadsKey = null;

async function refreshActing() {
  if (actingBusy) return;
  if (model.actingState === 'off' && ++offTicks % OFF_PROBE_EVERY !== 0) return;
  actingBusy = true;
  try {
    const prev = model.acting;
    let relief = prev.relief, actions = prev.actions, workloads = prev.workloads;
    let state = model.actingState, error = '';
    try {
      const r = await api.relief();
      if (r === null) {
        state = 'off'; relief = null; actions = []; workloads = [];
      } else {
        state = 'on'; relief = reliefSummary(r);
        const key = model.subjects().map((s) => s.id).join(',');
        const wantWorkloads = actingTicks % 4 === 0 || key !== workloadsKey;
        const [a, w] = await Promise.allSettled([api.reliefActions(), wantWorkloads ? api.workloads() : null]);
        if (a.status === 'fulfilled') actions = a.value || [];
        else error = a.reason.message;
        if (wantWorkloads && w.status === 'fulfilled') { workloads = w.value || []; workloadsKey = key; }
        else if (wantWorkloads) error = error || w.reason.message;
      }
    } catch (err) {
      error = err.message;        // /relief itself failed: not "off", just unanswered
    }
    actingTicks++;
    const changed = JSON.stringify([relief, actions, workloads]) !== JSON.stringify([prev.relief, prev.actions, prev.workloads])
      || state !== model.actingState || error !== model.actingError;
    model.actingState = state;
    model.actingError = error;
    if (changed) {
      model.setActing({ relief, actions, workloads });
      if (model.view) { graph.render(model); drawer.renderActive(); drawer.renderCounts(); }
    }
  } finally {
    actingBusy = false;
  }
}

function setHealth(state, label) {
  const el = document.getElementById('health');
  el.dataset.state = state;
  document.getElementById('health-label').textContent = label;
}

/**
 * Chained rather than periodic: the next poll is scheduled once the previous one
 * has finished, so a slow agent stretches the cadence instead of accumulating
 * overlapping requests.
 *
 * A hidden tab never schedules. Checking here rather than only in the
 * visibilitychange handler matters: a tick already in flight when the tab is
 * hidden has no timer to cancel, and would otherwise reschedule itself on
 * completion and poll the node indefinitely in the background.
 */
function schedule() {
  clearTimeout(timer);
  if (!live || document.hidden) return;
  timer = setTimeout(async () => {
    await tick();
    schedule();
  }, interval);
}

// ── controls ─────────────────────────────────────────────────────────────────

document.getElementById('live-btn').addEventListener('click', () => {
  live = true;
  document.getElementById('live-btn').setAttribute('aria-pressed', 'true');
  schedule();
  tick();
});
document.getElementById('step-btn').addEventListener('click', () => {
  live = false;
  document.getElementById('live-btn').setAttribute('aria-pressed', 'false');
  clearTimeout(timer);
  tick();
});
document.getElementById('interval').addEventListener('change', (e) => {
  interval = Number(e.target.value);
  schedule();
});

document.getElementById('search').addEventListener('input', (e) => {
  graph.setFilter({ text: e.target.value });
});
document.getElementById('status-filter').addEventListener('change', (e) => {
  statusFilter = e.target.value;
  tick();            // the choice changes what is fetched, not just what is drawn
});
document.getElementById('relayout-btn').addEventListener('click', () => graph.relayout());
document.getElementById('fit-btn').addEventListener('click', () => graph.fit());
document.getElementById('legend-btn').addEventListener('click', (e) => {
  const on = e.currentTarget.getAttribute('aria-pressed') !== 'true';
  e.currentTarget.setAttribute('aria-pressed', String(on));
  document.getElementById('legends').classList.toggle('hidden', !on);
  // Hiding the legend gives the canvas a row back; re-frame so the graph uses it.
  requestAnimationFrame(() => graph.fit());
});

// Theme: light, dark, or follow the OS. Stored so a reload does not undo a choice.
const themeBtn = document.getElementById('theme-btn');
themeBtn.addEventListener('click', () => {
  const current = document.documentElement.dataset.theme || 'auto';
  const next = current === 'auto' ? 'light' : current === 'light' ? 'dark' : 'auto';
  if (next === 'auto') delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = next;
  try {
    if (next === 'auto') localStorage.removeItem('sm-theme');
    else localStorage.setItem('sm-theme', next);
  } catch (_) { /* private mode: the choice simply does not persist */ }
  themeBtn.textContent = next === 'light' ? '☀' : next === 'dark' ? '☾' : '◐';
  themeBtn.title = `theme: ${next}`;
});

document.addEventListener('keydown', (e) => {
  if (e.key === '/' && e.target.tagName !== 'INPUT' && e.target.tagName !== 'TEXTAREA') {
    e.preventDefault();
    document.getElementById('search').focus();
  } else if (e.key === 'Escape' && !dialog.open) {
    graph.highlight(null);
    graph.select(null);
  } else if (e.key === ' ' && e.target === document.body) {
    e.preventDefault();
    (live ? document.getElementById('step-btn') : document.getElementById('live-btn')).click();
  }
});

// A hidden tab should not keep polling an edge node it is not being read on.
document.addEventListener('visibilitychange', () => {
  if (document.hidden) clearTimeout(timer);
  else if (live) { tick().then(schedule); }
});

// ── go ───────────────────────────────────────────────────────────────────────

buildStats();
api.version()
  .then((v) => {
    const label = `v${v.agent_version}${v.build_commit ? ` · ${v.build_commit}` : ''}`;
    document.getElementById('health').title = `${label} · Go ${v.go_version}`;
  })
  .catch(() => { /* the version is chrome; its absence must not block the console */ });

tick();
schedule();
