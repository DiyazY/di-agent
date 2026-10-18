// The drawer: the record, the queue, and everything wider than one property.
//
// Journal, decisions, discovery, subjects, cluster, backbone — six panes over the
// endpoints the previous console never called. Two habits run through all of them:
//
//   * an empty list always says WHY it is empty. "No candidates" and "the proposer
//     needs 30 pairs and has 11" are different facts, and only the second one tells
//     an operator whether to wait or to go and look at something.
//   * a bounded window is stated. The journal drops its oldest entries, and a
//     reader who cannot see that will read an absence as evidence that nothing
//     happened.

import { api } from './api.js';
import { fmt, escapeHTML, effective, directionSign, isEditing, factValue } from './model.js';

const JOURNAL_BUFFER = 400;

export class Drawer {
  constructor(root, ctx) {
    this.root = root;
    this.ctx = ctx;                 // { model, toast, selectProperty, refresh, ask }
    this.active = 'journal';
    this.events = [];
    this.journalWindow = null;
    this.sinceRevision = 0;
    this.decisions = [];
    this.openDecisionID = null;
    this.candidates = [];
    this.peers = null;
    this.backbone = null;
    this.where = null;      // the last GET /state/where answer
    this.whereProperty = '';
    this.answer = null;     // the last POST /explain answer
    this.question = '';
    this.asking = false;
    this.askError = null;
    this.busy = new Set();  // panes with an operator-started request in flight
    // The row whose part of the map is highlighted on the canvas: {pane, id}.
    this.pointed = null;
    this.panes = {};
    for (const name of ['journal', 'decisions', 'discovery', 'subjects', 'cluster', 'backbone', 'ask']) {
      this.panes[name] = document.getElementById(`pane-${name}`);
    }
    this.wireTabs();
  }

  wireTabs() {
    this.root.querySelectorAll('.tab').forEach((tab) => {
      tab.addEventListener('click', () => this.activate(tab.dataset.pane));
    });
    document.getElementById('drawer-toggle').addEventListener('click', () => {
      const collapsed = this.root.classList.toggle('collapsed');
      document.getElementById('drawer-toggle').textContent = collapsed ? '▴' : '▾';
    });
    this.wireResize();
  }

  /**
   * Drag the drawer's top edge to give the record more or less of the page. The
   * height is remembered per browser; double-click the edge for the default back.
   * The canvas above is a flex sibling, so it takes whatever is left, and its
   * ResizeObserver re-frames the graph.
   */
  wireResize() {
    const handle = document.getElementById('drawer-resize');
    const KEY = 'di-agent.drawer-height';
    const clamp = (h) => Math.round(Math.min(window.innerHeight - 160, Math.max(80, h)));
    const set = (h) => { this.root.style.height = `${clamp(h)}px`; };
    try {
      const saved = Number(localStorage.getItem(KEY));
      if (saved > 0) set(saved);
    } catch (_) { /* storage unavailable: start at the default */ }

    let drag = null;
    handle.addEventListener('pointerdown', (e) => {
      drag = { y: e.clientY, h: this.root.getBoundingClientRect().height };
      handle.classList.add('active');
      handle.setPointerCapture(e.pointerId);
      e.preventDefault();
    });
    handle.addEventListener('pointermove', (e) => {
      if (drag) set(drag.h + (drag.y - e.clientY));
    });
    const end = (e) => {
      if (!drag) return;
      drag = null;
      handle.classList.remove('active');
      try { handle.releasePointerCapture(e.pointerId); } catch (_) { /* already gone */ }
      try { localStorage.setItem(KEY, String(this.root.getBoundingClientRect().height)); } catch (_) { /* not remembered */ }
    };
    handle.addEventListener('pointerup', end);
    handle.addEventListener('pointercancel', end);
    handle.addEventListener('dblclick', () => {
      this.root.style.height = '';
      try { localStorage.removeItem(KEY); } catch (_) { /* nothing to forget */ }
    });
  }

  /** Returns the poll it starts, so a caller can wait for it. */
  activate(name) {
    this.active = name;
    this.root.classList.remove('collapsed');
    document.getElementById('drawer-toggle').textContent = '▾';
    this.root.querySelectorAll('.tab').forEach((t) => {
      t.setAttribute('aria-selected', String(t.dataset.pane === name));
    });
    for (const [k, pane] of Object.entries(this.panes)) {
      pane.classList.toggle('active', k === name);
    }
    return this.poll({ force: true });
  }

  /** Called after every /state poll, plus on demand. */
  async poll({ force = false } = {}) {
    // The journal and the discovery queue are cheap and are what makes the page
    // feel live, so they are fetched every tick regardless of which pane is open;
    // the rest are fetched only when visible.
    await Promise.allSettled([
      this.pollJournal(),
      this.pollCandidates(),
      this.active === 'decisions' || force ? this.pollDecisions() : null,
      this.active === 'cluster' ? this.pollPeers() : null,
      this.active === 'backbone' && (!this.backbone || force) ? this.pollBackbone() : null,
    ]);
    this.renderActive();
    this.renderCounts();
  }

  renderActive() {
    // The ask pane owns its own lifecycle: it holds a typed question, an answer
    // worth reading, and a request that can take minutes. A poll-driven rerender
    // would discard all three, so it only redraws when it asks to.
    if (this.active === 'ask' && this.askRendered) return;
    // Every pane is rebuilt wholesale, so a poll must not land on top of an
    // operator: an open dropdown would close mid-choice, and a button disabled
    // for its request would come back enabled and invite a duplicate. The pane
    // redraws itself when its request settles; the next poll catches up after.
    const pane = this.panes[this.active];
    if (this.busy.has(this.active) || isEditing(document.activeElement, pane)) return;
    this.askRendered = this.active === 'ask';
    switch (this.active) {
      case 'journal': return this.renderJournal();
      case 'decisions': return this.renderDecisions();
      case 'discovery': return this.renderDiscovery();
      case 'subjects': return this.renderSubjects();
      case 'cluster': return this.renderCluster();
      case 'backbone': return this.renderBackbone();
      case 'ask': return this.renderAsk();
    }
  }

  renderCounts() {
    const model = this.ctx.model();
    set('c-journal', this.events.length);
    set('c-decisions', this.decisions.length);
    set('c-subjects', model?.counts?.subjects ?? 0);
    set('c-cluster', (this.peers?.peers?.length ?? 0)
      + Object.keys(this.peers?.silent || {}).length);
    const cEl = document.getElementById('c-discovery');
    cEl.textContent = this.candidates.length;
    // Pending discovery is the one count worth interrupting for: the map has
    // produced structure it was not told about and is waiting on a human.
    cEl.classList.toggle('hot', this.candidates.length > 0);
  }

  // ── journal ───────────────────────────────────────────────────────────────

  async pollJournal() {
    const res = await api.journal({ since: this.sinceRevision || undefined, limit: 200 });
    this.journalWindow = {
      held: res.held, dropped: res.dropped, oldest: res.oldest_revision, revision: res.revision,
    };
    const fresh = res.events || [];
    if (fresh.length) {
      const known = new Set(this.events.map((e) => `${e.revision}:${e.kind}:${e.target}`));
      for (const e of fresh) {
        const key = `${e.revision}:${e.kind}:${e.target}`;
        if (known.has(key)) continue;
        e.__fresh = this.sinceRevision > 0;   // the first load is not "new"
        this.events.unshift(e);
      }
      this.sinceRevision = Math.max(this.sinceRevision, ...fresh.map((e) => e.revision));
      if (this.events.length > JOURNAL_BUFFER) this.events.length = JOURNAL_BUFFER;
    }
  }

  renderJournal() {
    const w = this.journalWindow;
    const note = w ? `
      <div class="window-note">
        <span><strong>${w.held}</strong> entries held</span>
        <span><strong>${w.dropped}</strong> dropped</span>
        <span>oldest revision <strong>${w.oldest}</strong></span>
        <span class="muted">The journal is bounded, so an absence here is not evidence that nothing happened.</span>
      </div>` : '';

    if (!this.events.length) {
      this.panes.journal.innerHTML = note + `<div class="empty">
        <strong>Nothing recorded yet</strong>
        <span class="why">Every mutation — a property admitted, gone stale, retired, a
        relationship asserted, a decision taken — advances the revision and lands here.</span>
      </div>`;
      return;
    }

    this.panes.journal.innerHTML = note + `<div class="journal">${this.events.map((e) => `
      <div class="row${e.__fresh ? ' fresh' : ''}">
        <span class="rev">r${e.revision}</span>
        <span class="kind ${kindClass(e.kind)}">${escapeHTML(shortKind(e.kind))}</span>
        <span class="target" title="${escapeHTML(e.target || '')}">${escapeHTML(e.target || '—')}</span>
        <span class="detail">${escapeHTML(detailLine(e))}</span>
        <span class="actor">${escapeHTML(e.actor || '')} · ${fmt.clock(e.at)}</span>
      </div>`).join('')}</div>`;
    // The highlight is a one-shot: a row stays new only until it has been seen.
    for (const e of this.events) e.__fresh = false;
  }

  // ── decisions ─────────────────────────────────────────────────────────────

  async pollDecisions() {
    const res = await api.decisions(40);
    const fresh = res.decisions || [];
    // The listing is capped, so an older trace the operator deliberately opened
    // is not in it. Dropping it here would make the row vanish from under them a
    // moment after they clicked "Open the trace".
    const held = this.openDecisionID
      && this.decisions.find((d) => d.id === this.openDecisionID);
    if (held && !fresh.some((d) => d.id === held.id)) fresh.unshift(held);
    this.decisions = fresh;
  }

  async openDecision(id) {
    // Awaited: activate() starts a poll that replaces this.decisions wholesale,
    // and if it lands after the fetch below it discards the record just injected.
    await this.activate('decisions');
    try {
      const d = await api.decision(id);
      this.openDecisionID = id;
      if (!this.decisions.some((x) => x.id === id)) this.decisions.unshift(d);
      else this.decisions = this.decisions.map((x) => (x.id === id ? d : x));
      if (!this.isPointed('decisions', id)) this.point('decisions', id, decisionSpan(d));
      this.renderDecisions();
    } catch (err) {
      this.ctx.toast(err.gone
        ? `${err.message} — the trace existed but has been evicted.`
        : err.message, 'err');
    }
  }

  renderDecisions() {
    if (!this.decisions.length) {
      this.panes.decisions.innerHTML = `<div class="empty">
        <strong>No decisions recorded</strong>
        <span class="why">Ask the map something — select a property and run an estimate —
        and the answer is recorded here with the exact state that produced it.</span>
      </div>`;
      return;
    }

    this.panes.decisions.innerHTML = `<div class="pane-pad">
      <table class="grid">
        <thead><tr><th>decision</th><th>question</th><th class="num">revision</th><th>when</th><th></th></tr></thead>
        <tbody>${this.decisions.map((d) => `
          <tr data-decision="${escapeHTML(d.id)}"${this.isPointed('decisions', d.id) ? ' class="pointed"' : ''}
              title="open the trace, and highlight on the map what this decision read">
            <td class="mono">${escapeHTML(d.id)}</td>
            <td>${escapeHTML(d.question || '—')}</td>
            <td class="num">${fmt.int(d.revision)}</td>
            <td class="muted nowrap">${fmt.ago(d.at)}</td>
            <td class="muted">${this.openDecisionID === d.id ? '▴' : '▾'}</td>
          </tr>
          ${this.openDecisionID === d.id ? `<tr><td colspan="5">${decisionTrace(d)}</td></tr>` : ''}
        `).join('')}</tbody>
      </table>
    </div>`;

    this.panes.decisions.querySelectorAll('[data-decision]').forEach((row) => {
      row.addEventListener('click', () => {
        const id = row.dataset.decision;
        this.openDecisionID = this.openDecisionID === id ? null : id;
        const d = this.decisions.find((x) => x.id === id);
        // Opening a trace points at what it read; closing it lets go.
        if (this.openDecisionID ? !this.isPointed('decisions', id) : this.isPointed('decisions', id)) {
          this.point('decisions', id, decisionSpan(d));
        }
        this.renderDecisions();
      });
    });
    this.panes.decisions.querySelectorAll('[data-cite]').forEach((el) => {
      el.addEventListener('click', (e) => { e.stopPropagation(); this.ctx.selectProperty(el.dataset.cite); });
    });
  }

  // ── discovery ─────────────────────────────────────────────────────────────

  async pollCandidates() {
    const res = await api.candidates();
    this.candidates = Array.isArray(res) ? res : (res?.candidates || []);
    // A candidate settled or withdrawn leaves the queue; what pointed at it lets go.
    if (this.pointed?.pane === 'discovery' && !this.candidates.some((c) => c.CandidateID === this.pointed.id)) {
      this.ctx.highlight(null);
    }
  }

  // ── pointing at the map ───────────────────────────────────────────────────

  /**
   * Highlight a row's part of the map, or let go of it when the row is already
   * pointed at. The canvas says what it could not draw — a property filtered out by
   * the search or no longer in the map — and that is passed on rather than shown as
   * a highlight with a piece missing.
   */
  point(pane, id, spec) {
    if (this.pointed?.pane === pane && this.pointed.id === id) {
      this.ctx.highlight(null);
      return;
    }
    this.ctx.highlight(null);
    this.pointed = { pane, id };
    const missing = this.ctx.highlight(spec) || [];
    if (missing.length) {
      const model = this.ctx.model();
      this.ctx.toast(`Not on the canvas: ${missing.map((m) => (model ? model.display(m) : m)).join(', ')}`
        + ' — cleared by the search, or no longer in the map.', 'err');
    }
  }

  /** The canvas let go (Escape, a click on empty canvas, another row). */
  unpoint() {
    if (!this.pointed) return;
    this.pointed = null;
    this.renderActive();
  }

  isPointed(pane, id) { return this.pointed?.pane === pane && this.pointed.id === id; }

  /** How far discovery was rationed, from /state; empty when the proposer is off. */
  rationingLine() {
    const d = this.ctx.model()?.view?.discovery;
    if (!d) return '';
    const by = d.tested_by_class || {};
    const budget = d.budget ? `a budget of ${fmt.int(d.budget)}` : 'no budget limit';
    const short = d.budget && d.eligible_pairs > d.tested_pairs;
    return `<p class="muted" style="font-size:11.5px;margin:0 0 9px;line-height:1.5"
        title="subject → node ${fmt.int(by['subject->node'] || 0)} · node ↔ node ${fmt.int(by['node<->node'] || 0)} · subject ↔ subject ${fmt.int(by['subject<->subject'] || 0)}">
        Testing ${fmt.int(d.tested_pairs)} of ${fmt.int(d.eligible_pairs)} pairs among
        ${fmt.int(d.properties)} properties, with ${budget}${d.idle_pairs ? `; ${fmt.int(d.idle_pairs)} more held by properties gone quiet` : ''}${d.displaced ? `; ${fmt.int(d.displaced)} displaced so far` : ''}.
        ${short ? 'The most variable properties get pairs first, so a quiet pair may not have been looked at yet.' : ''}
        ${d.fdr ? `A candidate's p must be at or under ${Number(d.p_bar).toPrecision(2)}: the bar at a ${fmt.num(d.fdr * 100, 1)}% false-discovery rate over the ${fmt.int(d.p_values_tested)} pairs with enough readings.` : 'The p-value bar is not corrected for how many pairs are tested.'}
        ${(d.groups || []).length ? `${fmt.int(d.groups.length)} arithmetic group${d.groups.length > 1 ? 's' : ''}, each paired outside through one member: ${d.groups.map((g) => `<span class="mono" title="${escapeHTML(g.members.join(', '))}">${escapeHTML(g.representative)}</span> for ${fmt.int(g.members.length)} of ${escapeHTML(g.chart)}`).join('; ')}.` : ''}
      </p>`;
  }

  renderDiscovery() {
    const model = this.ctx.model();
    if (!this.candidates.length) {
      this.panes.discovery.innerHTML = `<div class="empty">
        <strong>Nothing proposed right now</strong>
        ${this.rationingLine()}
        <span class="why">The proposer pairs any two properties: a subject's against the
        node's, the node's with each other, and subjects' with each other. It
        only emits a candidate once each end has moved enough to matter and the correlation
        clears the configured threshold over the configured minimum number of pairs
        (<span class="mono">-proposer-threshold</span>, <span class="mono">-proposer-min-pairs</span>),
        with a p-value under a bar that tightens as more pairs are tested
        (<span class="mono">-proposer-fdr</span>). Series of one chart that move as one are
        grouped rather than proposed.
        An empty queue means no tested pair has cleared both yet — not that the proposer is off.</span>
      </div>`;
      return;
    }

    this.panes.discovery.innerHTML = `<div class="pane-pad">
      ${this.rationingLine()}
      <p class="muted" style="font-size:11.5px;margin:0 0 9px;line-height:1.5">
        The map produced structure it was not told about. Confirming one admits it as a
        relationship with <span class="mono">discovered</span> provenance; rejecting tells
        the proposer to forget the pair. A pair shown with ↔ has no direction yet:
        correlation does not say which end drives the other, so you choose when you confirm.
      </p>
      <table class="grid">
        <thead><tr><th>pair</th><th>sign</th><th class="num">|r|</th><th class="num">p</th>
          <th class="num">pairs</th><th>status</th><th></th></tr></thead>
        <tbody>${this.candidates.map((c) => {
          const sign = directionSign(c.Direction);
          const from = escapeHTML(c.FromID), to = escapeHTML(c.ToID), id = escapeHTML(c.CandidateID);
          // Shown by name; the ids stay in the buttons, which is what the agent takes.
          const fromN = escapeHTML(model ? model.display(c.FromID) : c.FromID);
          const toN = escapeHTML(model ? model.display(c.ToID) : c.ToID);
          const confirm = c.Undirected
            ? `<button class="btn small primary" data-cand="${id}" data-verdict="confirm" data-from="${from}" title="confirm as ${fromN} → ${toN}">Confirm →</button>
               <button class="btn small primary" data-cand="${id}" data-verdict="confirm" data-from="${to}" title="confirm as ${toN} → ${fromN}">Confirm ←</button>`
            : `<button class="btn small primary" data-cand="${id}" data-verdict="confirm">Confirm</button>`;
          const notes = [
            c.Kind === 'composition' ? `<span class="muted" title="the source is part of the target by definition, so the correlation is accounting rather than something learned">composition</span>` : '',
            c.Kind === 'same-metric' ? `<span class="muted" title="two subjects' readings of one metric often move together because something drives both, not because either drives the other">same metric: a shared driver is likely</span>` : '',
            c.Kind === 'same-subject' ? `<span class="muted" title="two metrics of one workload, such as a capped pod's CPU and its throttling, often move together by construction">same subject: may hold by construction</span>` : '',
            ...(c.Caveats || []).map((x) => `<span class="muted">${escapeHTML(x)}</span>`),
          ].filter(Boolean).join('<br>');
          return `<tr data-cand-row="${id}"${this.isPointed('discovery', c.CandidateID) ? ' class="pointed"' : ''}>
            <td class="mono" title="${from} ${c.Undirected ? '↔' : '→'} ${to} — click the row to see the pair on the map">${fromN} ${c.Undirected ? '↔' : '→'} ${toN}${notes ? `<div style="font-family:var(--font);font-size:11px;margin-top:2px">${notes}</div>` : ''}</td>
            <td style="color:${sign < 0 ? 'var(--sign-neg)' : 'var(--sign-pos)'}">${sign < 0 ? '↓ negative' : '↑ positive'}</td>
            <td class="num">${fmt.num(c.MIScore, 3)}</td>
            <td class="num" title="${c.PBar ? `cleared the bar ${Number(c.PBar).toPrecision(2)}, corrected for the pairs tested` : 'no correction for the pairs tested'}">${fmt.num(c.PValue, 4)}</td>
            <td class="num">${fmt.int(c.NObservations)}</td>
            <td class="muted">${escapeHTML(String(c.Status ?? ''))}</td>
            <td class="nowrap">
              ${confirm}
              <button class="btn small" data-cand="${id}" data-verdict="defer">Defer</button>
              <button class="btn small danger" data-cand="${id}" data-verdict="reject">Reject</button>
            </td>
          </tr>`;
        }).join('')}</tbody>
      </table>
    </div>`;

    this.panes.discovery.querySelectorAll('[data-cand-row]').forEach((row) => {
      row.addEventListener('click', (e) => {
        if (e.target.closest('button')) return;
        const c = this.candidates.find((x) => x.CandidateID === row.dataset.candRow);
        if (!c) return;
        this.point('discovery', c.CandidateID, { pairs: [{ from: c.FromID, to: c.ToID, undirected: !!c.Undirected }] });
        this.renderDiscovery();
      });
    });

    this.panes.discovery.querySelectorAll('[data-cand]').forEach((b) => {
      b.addEventListener('click', async (e) => {
        e.stopPropagation();
        const { cand, verdict, from } = b.dataset;
        if (this.busy.has('discovery')) return;
        this.busy.add('discovery');
        this.panes.discovery.querySelectorAll('[data-cand]').forEach((x) => { x.disabled = true; });
        try {
          await api.reviewCandidate(cand, verdict, from);
          this.ctx.toast(`Candidate ${verdict}ed`, 'ok');
          await this.pollCandidates();
          this.ctx.refresh();
        } catch (err) {
          this.ctx.toast(err.message, 'err');
        } finally {
          this.busy.delete('discovery');
          this.renderDiscovery();
          this.renderCounts();
        }
      });
    });
  }

  // ── subjects ──────────────────────────────────────────────────────────────

  renderSubjects() {
    const model = this.ctx.model();
    const subjects = model.subjects();
    if (!subjects.length) {
      this.panes.subjects.innerHTML = `<div class="empty">
        <strong>No subject-scoped properties</strong>
        <span class="why">A sample carries a subject such as
        <span class="mono">pod:redis</span>, and a scoped sample becomes the property
        <span class="mono">metric@subject</span>. With none present, the map currently
        holds node-level properties only.</span>
      </div>`;
      return;
    }

    this.panes.subjects.innerHTML = `<div class="pane-pad">
      <table class="grid">
        <thead><tr><th>subject</th><th>kind</th><th class="num">properties</th>
          <th>status</th><th>properties held</th></tr></thead>
        <tbody>${subjects.map((s) => `
          <tr data-subject="${escapeHTML(s.id)}">
            <td class="mono" title="${escapeHTML(s.id)}">${escapeHTML(s.name || s.identity)}</td>
            <td class="muted">${escapeHTML(s.kind)}</td>
            <td class="num">${s.properties.length}</td>
            <td><span class="badge status-${s.status}">${s.status}</span></td>
            <td>${s.properties.map((p) => `
              <span class="badge mono" data-goto="${escapeHTML(p.id)}" style="cursor:pointer">
                ${escapeHTML(p.id.split('@')[0])} ${p.n_observations ? fmt.num(p.value, 2) : '—'}
              </span>`).join(' ')}</td>
          </tr>`).join('')}</tbody>
      </table>
      <p class="muted" style="font-size:11.5px;margin:9px 0 0;line-height:1.5">
        Each subject's properties are its own. They are never routed into a construct —
        the map holds what each workload is doing, not only what the node aggregates to.
      </p>
    </div>`;

    this.panes.subjects.querySelectorAll('[data-goto]').forEach((b) => {
      b.addEventListener('click', (e) => { e.stopPropagation(); this.ctx.selectProperty(b.dataset.goto); });
    });
    this.panes.subjects.querySelectorAll('[data-subject]').forEach((row) => {
      row.addEventListener('click', () => this.ctx.filterSubject(row.dataset.subject));
    });
  }

  // ── cluster ───────────────────────────────────────────────────────────────

  async pollPeers() {
    this.peers = await api.statePeers();
  }

  renderCluster() {
    const p = this.peers;
    if (!p) { this.panes.cluster.innerHTML = `<div class="empty">Loading…</div>`; return; }

    // `silent` maps a URL to the reason that peer has never answered. It is an
    // object, not an array, and a peer in it is deliberately absent from `peers`
    // — the store has no owner id to key it under. Both of those bit the first
    // version: the length check never fired, and a configured-but-unreachable
    // peer was reported as "none configured", which is the opposite diagnosis.
    const silent = Object.entries(p.silent || {});

    if (!p.peers?.length && !silent.length) {
      this.panes.cluster.innerHTML = `<div class="empty">
        <strong>No peers registered</strong>
        <span class="why">This agent is node-local by design: one agent per machine,
        its map holding that machine's state. Cluster-level questions go to peers, and
        none are configured — start the daemon with
        <span class="mono">-peers http://node_1:8080,…</span> or POST to
        <span class="mono">/peers</span>.</span>
      </div>`;
      return;
    }

    if (!p.peers?.length) {
      this.panes.cluster.innerHTML = `<div class="pane-pad">
        <p style="font-size:12.5px;color:var(--status-serious);margin:0 0 9px;line-height:1.55">
          <strong>${silent.length} peer${silent.length === 1 ? ' is' : 's are'} configured but
          ${silent.length === 1 ? 'has' : 'have'} never answered</strong>, so this agent holds no
          state for ${silent.length === 1 ? 'it' : 'them'} and cannot name
          ${silent.length === 1 ? 'it' : 'them'}. That is a different situation from a known peer
          that is currently unreachable.
        </p>
        <table class="grid">
          <thead><tr><th>url</th><th>why it has never answered</th></tr></thead>
          <tbody>${silent.map(([url, why]) => `<tr>
            <td class="mono">${escapeHTML(url)}</td>
            <td><span class="badge alarm">! ${escapeHTML(why)}</span></td>
          </tr>`).join('')}</tbody>
        </table>
        <div class="row-between" style="margin-top:10px">
          <span class="muted" style="font-size:11.5px">Local node
            <span class="mono">${escapeHTML(p.self || '')}</span>.</span>
          <button class="btn small" data-act="refresh-peers">Poll peers now</button>
        </div>
      </div>`;
      this.wireRefreshPeers();
      return;
    }

    this.panes.cluster.innerHTML = `<div class="pane-pad">
      <div class="row-between" style="margin-bottom:9px">
        <span class="muted" style="font-size:11.5px">
          Local node <span class="mono">${escapeHTML(p.self || '')}</span>. Peer state is
          reported as received and never merged — a property with the same id on two nodes
          describes two systems.
        </span>
        <button class="btn small" data-act="refresh-peers">Poll peers now</button>
      </div>
      <table class="grid">
        <thead><tr><th>peer</th><th>url</th><th class="num">revision</th><th class="num">age</th>
          <th class="num">properties</th><th class="num">subjects</th><th>last attempt</th></tr></thead>
        <tbody>${p.peers.map((x) => `
          <tr>
            <td class="mono">${escapeHTML(x.peer_id || '—')}</td>
            <td class="mono muted">${escapeHTML(x.url)}</td>
            <td class="num">${fmt.int(x.revision)}</td>
            <td class="num">${fmt.duration(x.age_seconds)}</td>
            <td class="num">${fmt.int(x.counts?.properties_total)}</td>
            <td class="num">${fmt.int(x.counts?.subjects)}</td>
            <td>${x.error
              ? `<span class="badge alarm">! ${escapeHTML(x.error)}</span>`
              : `<span class="badge status-active">● answered</span>`}</td>
          </tr>`).join('')}</tbody>
      </table>
      ${this.whereSection()}
      ${p.stale?.length ? `<p style="font-size:11.5px;color:var(--status-serious);margin:9px 0 0">
        Stale snapshots: ${p.stale.map(escapeHTML).join(', ')} — a snapshot is only as good as its age.</p>` : ''}
      ${silent.length ? `<p style="font-size:11.5px;color:var(--status-serious);margin:5px 0 0;line-height:1.5">
        Never answered, so no state is held for ${silent.length === 1 ? 'it' : 'them'}:
        ${silent.map(([url, why]) => `${escapeHTML(url)} (${escapeHTML(why)})`).join('; ')}</p>` : ''}
    </div>`;

    this.wireRefreshPeers();
  }

  /**
   * "Who else has this property, and what do they say it is."
   *
   * The answers are kept apart on purpose. The reason to ask several nodes is to
   * be able to pick one, and that needs each machine's own number — a mean across
   * machines is a figure describing none of them, so none is offered here.
   */
  whereSection() {
    const model = this.ctx.model();
    const options = model.properties
      .filter((x) => x.status !== 'retired')
      .map((x) => x.id)
      .sort();
    const w = this.where;

    return `<div style="margin-top:14px;border-top:1px solid var(--border);padding-top:11px">
      <div class="row-between" style="margin-bottom:8px">
        <span class="muted" style="font-size:11.5px">Locate one property across the cluster</span>
        <span class="inline">
          <select data-act="where-pick" style="width:auto;max-width:230px">
            <option value="">choose a property…</option>
            ${options.map((id) => `<option value="${escapeHTML(id)}"${
              id === this.whereProperty ? ' selected' : ''}>${escapeHTML(id)}</option>`).join('')}
          </select>
        </span>
      </div>
      ${w ? `
        <table class="grid">
          <thead><tr><th>node</th><th class="num">value</th><th class="num">confidence</th>
            <th>status</th><th class="num">observations</th><th class="num">age</th></tr></thead>
          <tbody>${(w.holders || []).map((h) => `<tr>
            <td class="mono">${escapeHTML(h.node)}${h.local
              ? ' <span class="badge" style="font-size:9.5px">this node</span>' : ''}</td>
            <td class="num">${fmt.num(h.value, 4)}</td>
            <td class="num">${fmt.num(h.confidence, 3)}</td>
            <td><span class="badge status-${escapeHTML(h.status)}">${escapeHTML(h.status)}</span></td>
            <td class="num">${fmt.int(h.n_observations)}</td>
            <td class="num muted">${h.age_seconds ? fmt.duration(h.age_seconds) : '—'}</td>
          </tr>`).join('')}</tbody>
        </table>
        ${(w.holders || []).length > 1 ? `<p class="muted" style="font-size:11px;margin:7px 0 0;line-height:1.5">
          Spread across ${w.holders.length} nodes:
          ${fmt.num(Math.min(...w.holders.map((h) => h.value)), 4)} to
          ${fmt.num(Math.max(...w.holders.map((h) => h.value)), 4)}.
          ${escapeHTML(w.note || '')}</p>` : ''}
      ` : '<p class="muted" style="font-size:11.5px;margin:0">Nothing looked up yet.</p>'}
    </div>`;
  }

  wireWhere() {
    this.panes.cluster.querySelector('[data-act="where-pick"]')?.addEventListener('change', async (e) => {
      const id = e.target.value;
      this.whereProperty = id;
      if (!id) { this.where = null; return this.renderCluster(); }
      this.busy.add('cluster');
      try {
        this.where = await api.where(id);
      } catch (err) {
        this.where = null;
        this.ctx.toast(err.message, 'err');
      } finally {
        this.busy.delete('cluster');
      }
      this.renderCluster();
    });
  }

  wireRefreshPeers() {
    this.panes.cluster.querySelector('[data-act="refresh-peers"]')?.addEventListener('click', async (e) => {
      if (this.busy.has('cluster')) return;
      e.currentTarget.disabled = true;
      this.busy.add('cluster');
      try {
        const res = await api.refreshPeers();
        this.ctx.toast(`${res.answered} answered, ${res.failed} failed`, res.failed ? 'err' : 'ok');
        await this.pollPeers();
      } catch (err) {
        this.ctx.toast(err.message, 'err');
      } finally {
        this.busy.delete('cluster');
        this.renderCluster();
      }
    });
    this.wireWhere();
  }

  // ── ask ───────────────────────────────────────────────────────────────────

  /**
   * The natural-language surface over the map.
   *
   * The reason this is worth a pane rather than a chat box is the citation list.
   * Every claim the answer makes is supposed to name the property or relationship
   * it came from, and the agent runs a deterministic validator over those
   * citations before the answer is allowed out — an answer naming a number the
   * map does not hold is refused rather than shown. So the citations are rendered
   * as the primary result, not a footnote, and each one selects that property on
   * the canvas: the claim and the state that justifies it, one click apart.
   */
  renderAsk() {
    const a = this.answer;
    const busy = this.asking;

    this.panes.ask.innerHTML = `<div class="pane-pad">
      <div class="inline" style="gap:8px;margin-bottom:10px">
        <input type="text" id="ask-input" placeholder="Ask about this system — e.g. what is it doing right now?"
               value="${escapeHTML(this.question || '')}" ${busy ? 'disabled' : ''}
               style="flex:1;min-width:240px" />
        <button class="btn primary" data-act="ask" ${busy ? 'disabled' : ''}>
          ${busy ? 'Thinking…' : 'Ask'}
        </button>
      </div>

      ${busy ? `<p class="muted" style="font-size:11.5px;margin:0;line-height:1.5">
        The agent is reading its own map to answer. Against a local model this takes
        seconds to minutes; the answer is withheld until its citations check out
        against the state model.</p>` : ''}

      ${this.askError ? `<div class="empty" style="padding:14px 0">
        <strong style="color:var(--status-critical)">The agent did not answer</strong>
        <span class="why">${escapeHTML(this.askError)}</span>
        ${/not enabled|501/i.test(this.askError) ? `<span class="why muted">
          Start the daemon with <span class="mono">-explain-provider openai-compatible</span>
          and an <span class="mono">-explain-url</span> it can reach.</span>` : ''}
      </div>` : ''}

      ${a ? `
        <div class="stack">
          <p style="font-size:13px;line-height:1.6;margin:0">${escapeHTML(a.answer || '')}</p>

          <div class="chips">
            <span class="badge">confidence: ${escapeHTML(a.confidence || '—')}</span>
            <span class="badge mono">${escapeHTML(a.model_name || '')}</span>
            <span class="badge">${fmt.int(a.iterations)} iteration${a.iterations === 1 ? '' : 's'}</span>
            <span class="badge">${fmt.int((a.usage || {}).total_tokens)} tokens</span>
            ${(a.usage || {}).tool_calls !== undefined
              ? `<span class="badge">${fmt.int(a.usage.tool_calls)} tool calls</span>` : ''}
          </div>

          <div>
            <h3 style="margin:0 0 6px;font-size:10.5px;text-transform:uppercase;letter-spacing:.07em;color:var(--ink-muted)">
              Cited from the map <span style="text-transform:none;letter-spacing:0;font-weight:400">
              — checked against the state model before the answer was released</span>
            </h3>
            ${(a.citations || []).length ? `<div class="chips">
              ${a.citations.map((c) => {
                const bits = [];
                if (c.value !== undefined) bits.push(fmt.num(c.value, 4));
                if (c.effective !== undefined && c.effective !== null) bits.push(`eff ${fmt.num(c.effective, 3)}`);
                else if (c.established !== undefined && c.established !== null) bits.push(`est ${fmt.num(c.established, 3)}`);
                if (c.confidence) bits.push(`c=${fmt.num(c.confidence, 3)}`);
                if (c.n_observations) bits.push(`n=${c.n_observations}`);
                return `<span class="badge mono" data-cite-kind="${escapeHTML(c.kind)}"
                              data-cite="${escapeHTML(c.id)}"
                              style="cursor:${c.kind === 'property' ? 'pointer' : 'default'}"
                              title="${escapeHTML(c.kind)}">
                  ${escapeHTML(c.id)}${bits.length ? ' · ' + escapeHTML(bits.join(' ')) : ''}
                </span>`;
              }).join('')}
            </div>` : `<p class="muted" style="font-size:11.5px;margin:0">
              None — an answer citing nothing is one the map did not supply.</p>`}
          </div>

          ${(a.tool_trace || []).length ? `<div>
            <h3 style="margin:0 0 6px;font-size:10.5px;text-transform:uppercase;letter-spacing:.07em;color:var(--ink-muted)">
              What it read
            </h3>
            <table class="grid"><tbody>
              ${a.tool_trace.map((t) => `<tr>
                <td class="mono" style="width:150px">${escapeHTML(t.name)}</td>
                <td class="muted">${escapeHTML(t.result_digest || t.error || '')}</td>
              </tr>`).join('')}
            </tbody></table>
          </div>` : ''}

          ${a.critic_verdict ? `<p class="muted" style="font-size:11px;margin:0;line-height:1.5">
            Critic: ${escapeHTML(JSON.stringify(a.critic_verdict).slice(0, 300))}</p>` : ''}
        </div>
      ` : (!busy && !this.askError ? `<div class="empty">
        <strong>Ask the map something</strong>
        <span class="why">The answer is assembled from this node's own state and is
        released only once every citation in it has been checked against that state.
        A claim the map cannot support does not reach you.</span>
      </div>` : '')}
    </div>`;

    const input = this.panes.ask.querySelector('#ask-input');
    const run = async () => {
      const q = (input.value || '').trim();
      if (!q || this.asking) return;
      this.question = q;
      this.asking = true;
      this.askError = null;
      this.answer = null;
      this.renderAsk();
      try {
        this.answer = await api.explain(q);
      } catch (err) {
        this.askError = err.message;
      } finally {
        this.asking = false;
        this.renderAsk();
      }
    };
    this.panes.ask.querySelector('[data-act="ask"]')?.addEventListener('click', run);
    input?.addEventListener('keydown', (e) => { if (e.key === 'Enter') run(); });

    // A citation is a pointer into the map, so make it behave like one.
    this.panes.ask.querySelectorAll('[data-cite-kind="property"]').forEach((el) => {
      el.addEventListener('click', () => this.ctx.selectProperty(el.dataset.cite));
    });
  }

  // ── backbone ──────────────────────────────────────────────────────────────

  async pollBackbone() {
    const [graph, propositions] = await Promise.all([api.graph(), api.propositions()]);
    this.backbone = { graph, propositions: propositions?.propositions || propositions || [] };
  }

  renderBackbone() {
    const b = this.backbone;
    if (!b) { this.panes.backbone.innerHTML = `<div class="empty">Loading…</div>`; return; }
    const props = Array.isArray(b.propositions) ? b.propositions : [];

    this.panes.backbone.innerHTML = `<div class="pane-pad">
      <p class="muted" style="font-size:11.5px;margin:0 0 9px;line-height:1.5">
        The declared backbone the map was seeded from. Its constructs appear on the canvas
        as <strong>derived</strong> properties — this pane is where the declaration itself,
        and its deprecations, stay readable.
      </p>
      <table class="grid">
        <thead><tr><th>proposition</th><th>from</th><th>to</th><th>direction</th>
          <th class="num">prior</th><th>evidence</th><th></th></tr></thead>
        <tbody>${props.map((p) => `
          <tr${p.deprecated ? ' style="opacity:.5"' : ''}>
            <td class="mono">${escapeHTML(p.proposition_id)}</td>
            <td class="mono">${escapeHTML(p.from)}</td>
            <td class="mono">${escapeHTML(p.to)}</td>
            <td>${escapeHTML(p.direction)}</td>
            <td class="num">${p.instantiated
              ? fmt.num(p.prior_strength, 3)
              : '<span class="muted" title="not instantiated: the value on the wire is a placeholder, not a prior">—</span>'}</td>
            <td class="muted">${escapeHTML((p.evidence_sources || []).join(', ') || '—')}</td>
            <td>${p.deprecated
              ? `<span class="badge" title="${escapeHTML(p.deprecated_reason || '')}">deprecated</span>`
              : `<button class="btn small danger" data-deprecate="${escapeHTML(p.proposition_id)}">Deprecate…</button>`}</td>
          </tr>`).join('')}</tbody>
      </table>
      <div class="row-between" style="margin-top:12px">
        <span class="muted" style="font-size:11.5px">
          ${(b.graph?.constructs || []).length} constructs · ${props.length} propositions
          · ${props.filter((p) => p.instantiated).length} with an instantiated prior
        </span>
        <button class="btn small" data-act="tune">Apply an operator intent…</button>
      </div>
    </div>`;

    this.panes.backbone.querySelectorAll('[data-deprecate]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        const got = await this.ctx.ask({
          title: `Deprecate ${btn.dataset.deprecate}`,
          fields: [{ name: 'reason', label: 'Reason (required)' }],
          ok: 'Deprecate',
        });
        if (!got?.reason?.trim()) return;
        try {
          await api.deprecate(btn.dataset.deprecate, got.reason.trim());
          this.ctx.toast('Proposition deprecated', 'ok');
          await this.pollBackbone();
          this.renderBackbone();
        } catch (err) { this.ctx.toast(err.message, 'err'); }
      });
    });

    this.panes.backbone.querySelector('[data-act="tune"]')?.addEventListener('click', async () => {
      const got = await this.ctx.ask({
        title: 'Operator intent',
        fields: [
          { name: 'intent', label: 'Intent', placeholder: 'e.g. prefer_low_latency' },
          { name: 'operator', label: 'Operator', value: 'operator' },
        ],
        hint: 'Tuning records the intent and the adjustments it implied, so the change stays attributable.',
        ok: 'Apply',
      });
      if (!got?.intent?.trim()) return;
      try {
        const res = await api.tune(got.intent.trim(), got.operator?.trim() || 'operator');
        this.ctx.toast(`${(res.applied || []).length} adjustment(s) applied`, 'ok');
        await this.pollBackbone();
        this.renderBackbone();
      } catch (err) { this.ctx.toast(err.message, 'err'); }
    });
  }
}

// ── helpers ──────────────────────────────────────────────────────────────────

const set = (id, v) => { const el = document.getElementById(id); if (el) el.textContent = v; };

function shortKind(kind) {
  const k = String(kind || '');
  const dot = k.indexOf('.');
  return dot === -1 ? k : k.slice(dot + 1);
}

function kindClass(kind) {
  const k = String(kind || '');
  if (k.includes('admitted') || k.includes('declared')) return 'admitted';
  if (k.includes('stale')) return 'stale';
  if (k.includes('retired')) return 'retired';
  if (k.includes('asserted')) return 'asserted';
  if (k.includes('conflict') || k.includes('failed')) return 'conflict';
  if (k === 'decision') return 'decision';
  return '';
}

/**
 * Render one journal-detail or decision-answer value as text.
 *
 * Both of those arrive as free-form `map[string]any`, so a value can be a nested
 * object — a decision's `hypothetical` is one — and the default stringification
 * of those is "[object Object]", which tells a reader nothing at all.
 */
function plain(v) {
  if (v === null || v === undefined) return '—';
  if (typeof v === 'number') return String(Number(v.toFixed(4)));
  if (Array.isArray(v)) return v.map(plain).join(', ');
  if (typeof v === 'object') {
    return Object.entries(v).map(([k, x]) => `${k} ${plain(x)}`).join(' · ');
  }
  return String(v);
}

function detailLine(e) {
  if (e.kind === 'decision' && e.decision) return e.decision.question || '';
  const d = e.detail;
  if (!d) return '';
  return Object.entries(d).map(([k, v]) => `${k}=${plain(v)}`).join('  ');
}

/**
 * What a decision covers on the map: the properties and relationships it read and,
 * for a candidate review, the candidate's two ends as a would-be edge.
 */
function decisionSpan(d) {
  if (!d) return { nodes: [] };
  const c = d.inputs?.candidate;
  return {
    nodes: (d.properties_read || []).map((p) => p.id),
    edges: (d.relationships_read || []).map((r) => r.id),
    pairs: c?.FromID && c?.ToID ? [{ from: c.FromID, to: c.ToID, undirected: !!c.Undirected }] : [],
  };
}

/** The full trace: what was read, what was supposed, and the agent's own words. */
function decisionTrace(d) {
  const props = d.properties_read || [];
  const rels = d.relationships_read || [];
  return `<div style="padding:10px 2px 14px;display:flex;flex-direction:column;gap:12px">
    ${d.answer ? `<div>
      <h3 style="margin:0 0 6px;font-size:10.5px;text-transform:uppercase;letter-spacing:.07em;color:var(--ink-muted)">Answer</h3>
      <dl class="kv" style="max-width:460px">${Object.entries(d.answer).map(([k, v]) =>
        `<dt>${escapeHTML(k)}</dt><dd>${escapeHTML(plain(v))}</dd>`).join('')}</dl>
    </div>` : ''}

    ${(d.assumptions && Object.keys(d.assumptions).length) || (d.excluded || []).length ? `<div>
      <h3 style="margin:0 0 6px;font-size:10.5px;text-transform:uppercase;letter-spacing:.07em;color:var(--ink-muted)">Supposed</h3>
      <div class="chips">
        ${Object.entries(d.assumptions || {}).map(([k, v]) =>
          `<span class="badge mono">${escapeHTML(k)} = ${escapeHTML(String(v))}</span>`).join('')}
        ${(d.excluded || []).map((x) => `<span class="badge mono">without ${escapeHTML(x)}</span>`).join('')}
      </div>
    </div>` : ''}

    <div>
      <h3 style="margin:0 0 6px;font-size:10.5px;text-transform:uppercase;letter-spacing:.07em;color:var(--ink-muted)">
        Read ${props.length} propert${props.length === 1 ? 'y' : 'ies'}, ${rels.length} relationship${rels.length === 1 ? '' : 's'}
        <span style="text-transform:none;letter-spacing:0;font-weight:400"> — copied at the time, so this record cannot become a description of a later system</span>
      </h3>
      <div class="chips">
        ${props.map((p) => `<span class="badge mono" data-cite="${escapeHTML(p.id)}" style="cursor:pointer"
          title="confidence ${fmt.num(p.confidence, 3)} · click to inspect">
          ${escapeHTML(p.id)} = ${fmt.num(p.value, 3)}</span>`).join('')}
      </div>
      ${rels.length ? `<div class="chips" style="margin-top:6px">
        ${rels.map((r) => {
          const eff = effective(r);
          return `<span class="badge mono">
            <span class="swatch" style="background:var(--prov-${escapeHTML(r.provenance)})"></span>
            ${escapeHTML(r.from)} → ${escapeHTML(r.to)} ${eff.known ? fmt.num(eff.value, 3) : 'unknown'}</span>`;
        }).join('')}</div>` : ''}
    </div>

    ${(d.facts_read || []).length ? `<div>
      <h3 style="margin:0 0 6px;font-size:10.5px;text-transform:uppercase;letter-spacing:.07em;color:var(--ink-muted)">On the machine</h3>
      <div class="chips">${d.facts_read.map((f) => `<span class="badge mono" title="read by ${escapeHTML(f.source)}">
        ${escapeHTML(f.key)} ${escapeHTML(factValue(f))}</span>`).join('')}</div>
    </div>` : ''}

    ${d.rationale ? `<pre class="rationale">${escapeHTML(d.rationale)}</pre>` : ''}

    ${(d.caveats || []).length ? `<ul class="caveats">
      ${d.caveats.map((c) => `<li>${escapeHTML(c)}</li>`).join('')}</ul>` : ''}
  </div>`;
}
