// The inspector: one property or one relationship, in full, plus the counterfactual
// studio.
//
// The studio is the part worth the screen space. The map's distinguishing claim is
// not that it stores telemetry but that it can be asked a question and the answer
// can be reproduced afterwards from the state that produced it. So every estimate
// run here surfaces its decision id and links to the trace, and every caveat the
// agent attached is shown rather than summarised away.

import { api } from './api.js';
import { fmt, escapeHTML, effective, isEditing, factValue, subjectOf } from './model.js';
import { sparkline, strengthLayers, contributionBars, signSplit, meter } from './charts.js';

export class Inspector {
  constructor(root, ctx) {
    this.root = root;
    this.ctx = ctx;              // { model, toast, ask, selectProperty, openDecision, refresh }
    this.selection = null;
    this.assume = new Map();     // property id -> assumed value
    this.without = new Set();    // subjects or properties to leave out
    this.lastEstimate = null;
    this.explainText = null;
  }

  show(sel) {
    const changed = sel?.id !== this.selection?.id || sel?.type !== this.selection?.type;
    this.selection = sel;
    if (changed) {
      this.assume.clear();
      this.without.clear();
      this.lastEstimate = null;
      this.explainText = null;
    }
    this.render();
  }

  /**
   * Re-render on a poll — but never on top of someone.
   *
   * The panel is rebuilt wholesale, so a refresh that lands while an operator is
   * typing a reason, dragging an assumption slider or reading a trace would take
   * the caret and throw the scroll position away. Twice a second, that makes the
   * panel unusable. A focused editing control inside the panel means the refresh
   * waits; the next poll after they click away picks the new values up.
   *
   * Only editing controls count. A dialog hands focus back to the button that
   * opened it, and a clicked button keeps focus in Chromium, so a guard on any
   * focused element would freeze the panel after every action until the operator
   * happened to click elsewhere.
   */
  refresh() {
    if (!this.selection) return;
    if (isEditing(document.activeElement, this.root)) return;
    this.render();
  }

  render() {
    const sel = this.selection;
    if (!sel) { this.root.classList.add('hidden'); this.root.replaceChildren(); return; }
    this.root.classList.remove('hidden');

    // Rebuilding resets the scroll box; carry the position across so a live panel
    // does not scroll itself back to the top while it is being read.
    const previous = this.root.querySelector('.inspector-body');
    this.restoreScroll = previous ? previous.scrollTop : 0;

    const model = this.ctx.model();
    if (sel.type === 'machine') {
      this.renderMachine(model);
    } else if (sel.type === 'property') {
      const p = model.property(sel.id);
      if (!p) return this.renderMissing(sel.id, 'property');
      this.renderProperty(p, model);
    } else {
      const r = model.relationship(sel.id);
      if (!r) return this.renderMissing(sel.id, 'relationship');
      this.renderRelationship(r, model);
    }
  }

  /** What the machine is: the facts read at startup, each with where it came from. */
  renderMachine(model) {
    const facts = model.facts;
    const own = model.properties.filter((p) => !subjectOf(p));
    const subjects = model.subjects().length;
    this.root.replaceChildren(node(`
      <div class="inspector-head">
        <div class="title"><div class="eyebrow">machine</div><h2>${escapeHTML(model.owner || 'this machine')}</h2></div>
        <button class="btn ghost small" data-act="close" aria-label="close">✕</button>
      </div>
      <div class="inspector-body">
        <div class="section">
          <p class="muted" style="font-size:11.5px;margin:0;line-height:1.5">
            ${own.length} of the map's properties are the machine's own; ${subjects} subject${subjects === 1 ? '' : 's'}
            run on it. The facts below were read when the agent started and hold while it runs:
            they are the frame its values are read in, and every review decision records them.</p>
        </div>
        <div class="section">
          <h3>Facts</h3>
          ${facts.length ? `<dl class="kv">${facts.map((f) => `
            <dt>${escapeHTML(f.key.replace(/_/g, ' '))}</dt>
            <dd title="read by ${escapeHTML(f.source)} · ${escapeHTML(fmt.clock(f.read_at))}">${escapeHTML(factValue(f))}</dd>`).join('')}
          </dl>` : `<div class="empty"><strong>No facts read</strong>
            <span class="why">The agent reads them at startup from the sources named by
            <span class="mono">-facts</span> (default <span class="mono">host</span>);
            <span class="mono">-facts none</span> turns them off.</span></div>`}
        </div>
        ${facts.length ? `<div class="section"><h3>Read by</h3><div class="chips">
          ${[...new Set(facts.map((f) => f.source))].map((s) => `<span class="badge mono">${escapeHTML(s)}</span>`).join('')}
        </div></div>` : ''}
      </div>`));
    this.wireCommon();
  }

  renderMissing(id, what) {
    this.root.replaceChildren(node(`
      <div class="inspector-head">
        <div class="title"><div class="eyebrow">${what}</div><h2>${escapeHTML(id)}</h2></div>
        <button class="btn ghost small" data-act="close">✕</button>
      </div>
      <div class="inspector-body"><div class="empty">
        <strong>No longer in the view</strong>
        <span class="why">It may have been retired, or the current filter excludes it.
        Retirement is soft — switch the status filter to “everything” to see it.</span>
      </div></div>`));
    this.wireCommon();
  }

  // ── property ──────────────────────────────────────────────────────────────

  renderProperty(p, model) {
    const inbound = model.influencesOn(p.id);
    const outbound = model.influencesOf(p.id);
    const series = model.series(p.id);
    const prev = series.length > 1 ? series[series.length - 2].v : null;
    const delta = prev === null ? null : p.value - prev;

    const frag = node(`
      <div class="inspector-head">
        <div class="title">
          <div class="eyebrow">${p.kind} property${p.subject ? ` · <span title="${escapeHTML(p.subject)}">${escapeHTML(model.nameOf(p.subject))}</span>` : ''}</div>
          <h2>${escapeHTML(p.id)}</h2>
        </div>
        <button class="btn ghost small" data-act="close" aria-label="close">✕</button>
      </div>
      <div class="inspector-body">
        <div class="section">
          <div class="hero">
            <span class="figure">${p.n_observations ? fmt.num(p.value, 3) : '—'}</span>
            <span class="unit">${escapeHTML(p.unit || '')}</span>
            ${delta !== null && Math.abs(delta) > 1e-9
              ? `<span class="delta ${delta > 0 ? 'up' : 'down'}">${fmt.signed(delta, 3)}</span>` : ''}
          </div>
          <div data-slot="spark"></div>
          <div class="chips" style="margin-top:10px">
            <span class="badge status-${p.status}">${statusGlyph(p.status)} ${p.status}</span>
            <span class="badge">${p.kind}</span>
            ${p.range_declared ? `<span class="badge mono">range ${fmt.num(p.range[0], 2)}–${fmt.num(p.range[1], 2)}</span>`
              : `<span class="badge" title="no range was declared, so the value is not normalised anywhere in this console">range undeclared</span>`}
            ${p.out_of_range ? `<span class="badge alarm">${p.out_of_range} out of range</span>` : ''}
          </div>
        </div>

        <div class="section">
          <h3>Confidence</h3>
          <div data-slot="confidence"></div>
          <p class="muted" style="font-size:11px;margin:7px 0 0;line-height:1.5">
            How much of this rests on observation rather than assumption:
            ${p.n_observations} observation${p.n_observations === 1 ? '' : 's'} so far.
          </p>
        </div>

        <div class="section">
          <h3>Record</h3>
          <dl class="kv">
            <dt>first observed</dt><dd>${fmt.ago(p.first_observed)}</dd>
            <dt>last observed</dt><dd>${fmt.ago(p.last_observed)}</dd>
            ${p.source ? `<dt>source</dt><dd class="mono">${escapeHTML(p.source)}</dd>` : ''}
            ${p.retired_reason ? `<dt>retired because</dt><dd>${escapeHTML(p.retired_reason)}</dd>` : ''}
          </dl>
          ${labelRows(p.labels)}
        </div>

        ${p.members?.length ? `
        <div class="section">
          <h3>Recomputed from <span class="muted">${p.members.length} member${p.members.length === 1 ? '' : 's'}</span></h3>
          <table class="grid"><tbody>
            ${p.members.map((m) => {
              const mp = model.property(m);
              return `<tr data-goto="${escapeHTML(m)}">
                <td class="mono">${escapeHTML(m)}</td>
                <td class="num">${mp ? (mp.n_observations ? fmt.num(mp.value, 3) : 'unobserved') : 'not held'}</td>
              </tr>`;
            }).join('')}
          </tbody></table>
          <p class="muted" style="font-size:11px;margin:7px 0 0;line-height:1.5">
            A derived property is never stored — it is recomputed from its members, so this
            summary cannot disagree with what it summarises.
          </p>
        </div>` : ''}

        <div class="section">
          <h3>Influenced by <span class="muted">${inbound.length}</span></h3>
          ${relTable(inbound, 'from')}
        </div>

        <div class="section">
          <h3>Influences <span class="muted">${outbound.length}</span></h3>
          ${relTable(outbound, 'to')}
        </div>

        <div class="section" data-slot="estimate"></div>

        <div class="section">
          <h3>Explain</h3>
          <button class="btn small" data-act="explain">Render the agent's own explanation</button>
          ${this.explainText !== null
            ? `<pre class="rationale" style="margin-top:9px">${escapeHTML(this.explainText)}</pre>` : ''}
        </div>

        <div class="section">
          <h3>Operator</h3>
          <button class="btn small danger" data-act="retire-property">Retire this property…</button>
          <p class="muted" style="font-size:11px;margin:7px 0 0;line-height:1.5">
            Retirement is soft and requires a reason: earlier decisions stay reconstructible,
            and an observation revives the property.
          </p>
        </div>
      </div>`);

    this.root.replaceChildren(frag);
    this.root.querySelector('[data-slot="spark"]').append(sparkline(series, { unit: p.unit || '' }));
    this.root.querySelector('[data-slot="confidence"]')
      .append(meter(p.confidence, `${fmt.num(p.confidence, 3)} · n=${p.n_observations}`));
    this.renderEstimate(this.root.querySelector('[data-slot="estimate"]'), p, model);
    this.wireCommon();
    this.wireProperty(p);
    this.applyScroll();
  }

  applyScroll() {
    const body = this.root.querySelector('.inspector-body');
    if (body && this.restoreScroll) body.scrollTop = this.restoreScroll;
  }

  wireProperty(p) {
    this.root.querySelector('[data-act="explain"]')?.addEventListener('click', async (e) => {
      const btn = e.currentTarget;
      btn.disabled = true;
      try {
        this.explainText = await api.explainProperty(p.id);
        this.render();
      } catch (err) {
        this.ctx.toast(err.message, 'err');
        btn.disabled = false;
      }
    });

    this.root.querySelector('[data-act="retire-property"]')?.addEventListener('click', async () => {
      const got = await this.ctx.ask({
        title: `Retire ${p.id}`,
        fields: [
          { name: 'reason', label: 'Reason (required)', placeholder: 'why this stopped being part of the system' },
          { name: 'actor', label: 'Actor', value: 'operator' },
        ],
        hint: 'The agent refuses a retirement with no stated basis, because such a claim cannot be audited later.',
        ok: 'Retire',
      });
      if (!got) return;
      if (!got.reason?.trim()) return this.ctx.toast('A reason is required.', 'err');
      try {
        await api.retireProperty(p.id, got.reason.trim(), got.actor?.trim() || 'operator');
        this.ctx.toast(`Retired ${p.id}`, 'ok');
        this.ctx.refresh();
      } catch (err) { this.ctx.toast(err.message, 'err'); }
    });
  }

  // ── the counterfactual studio ─────────────────────────────────────────────

  renderEstimate(slot, p, model) {
    const subjects = model.subjects();
    const assumable = model.properties
      .filter((x) => x.status !== 'retired' && x.id !== p.id)
      .sort((a, b) => a.id.localeCompare(b.id));

    slot.innerHTML = `
      <h3>
        Estimate
        <span class="muted" style="text-transform:none;letter-spacing:0;font-weight:400">
          answered from the map
        </span>
      </h3>

      <div class="stack">
        <div>
          <div class="row-between" style="margin-bottom:5px">
            <span class="muted" style="font-size:11px">Suppose a property were…</span>
            <select data-act="add-assume" style="width:auto;max-width:170px">
              <option value="">add an assumption…</option>
              ${assumable.map((x) => `<option value="${escapeHTML(x.id)}">${escapeHTML(x.id)}</option>`).join('')}
            </select>
          </div>
          <div data-slot="assumptions">
            ${[...this.assume.entries()].map(([id, v]) => {
              const ap = model.property(id);
              // Estimate substitutes the raw value, so a slider is only offered where
              // the property declares the range it lives in. Without one, any bound
              // this page invented would clamp the assumption to a value the operator
              // never meant; the number field is left unbounded instead.
              if (!ap?.range_declared) {
                return `<div class="assume-row" data-assume="${escapeHTML(id)}">
                  <span class="id" title="${escapeHTML(id)}">${escapeHTML(id)}</span>
                  <input type="number" step="any" value="${v}" data-assume-value />
                  <button class="btn ghost small" data-drop-assume aria-label="remove">✕</button>
                  <span class="muted" style="grid-column:1 / -1;font-size:11px">no declared range — enter the raw value</span>
                </div>`;
              }
              const [lo, hi] = ap.range;
              const step = (hi - lo) / 200 || 'any';
              return `<div class="assume-row" data-assume="${escapeHTML(id)}">
                <span class="id" title="${escapeHTML(id)}">${escapeHTML(id)}</span>
                <input type="number" step="any" min="${lo}" max="${hi}" value="${v}" data-assume-value />
                <button class="btn ghost small" data-drop-assume aria-label="remove">✕</button>
                <input type="range" min="${lo}" max="${hi}" step="${step}" value="${v}"
                       data-assume-range style="grid-column:1 / -1" />
              </div>`;
            }).join('') || '<p class="muted" style="font-size:11px;margin:0">None — this estimates the system as it stands.</p>'}
          </div>
        </div>

        <div>
          <div class="row-between" style="margin-bottom:5px">
            <span class="muted" style="font-size:11px">…or were not here at all</span>
            <select data-act="add-without" style="width:auto;max-width:170px">
              <option value="">leave something out…</option>
              ${subjects.map((s) => `<option value="${escapeHTML(s.id)}">subject ${escapeHTML(s.name || s.id)}</option>`).join('')}
              ${assumable.map((x) => `<option value="${escapeHTML(x.id)}">property ${escapeHTML(x.id)}</option>`).join('')}
            </select>
          </div>
          <div class="chips" data-slot="without">
            ${[...this.without].map((w) => `<span class="badge">${escapeHTML(w)}
              <button class="btn ghost small" data-drop-without="${escapeHTML(w)}"
                      style="padding:0 2px;margin-left:2px">✕</button></span>`).join('')
              || '<span class="muted" style="font-size:11px">Nothing excluded.</span>'}
          </div>
        </div>

        <button class="btn primary" data-act="run-estimate">
          Estimate ${escapeHTML(p.id)}${this.assume.size || this.without.size ? ' under these supposals' : ''}
        </button>

        <div data-slot="outcome"></div>
      </div>`;

    if (this.lastEstimate && this.lastEstimate.answer?.target === p.id) {
      slot.querySelector('[data-slot="outcome"]').append(this.outcome(this.lastEstimate));
    }

    // -- wiring
    slot.querySelector('[data-act="add-assume"]').addEventListener('change', (e) => {
      const id = e.target.value;
      if (!id) return;
      const ap = model.property(id);
      this.assume.set(id, Number((ap?.value ?? 0).toFixed(3)));
      this.render();
    });
    slot.querySelector('[data-act="add-without"]').addEventListener('change', (e) => {
      if (!e.target.value) return;
      this.without.add(e.target.value);
      this.render();
    });
    slot.querySelectorAll('[data-drop-assume]').forEach((b) => b.addEventListener('click', (e) => {
      this.assume.delete(e.target.closest('[data-assume]').dataset.assume);
      this.render();
    }));
    slot.querySelectorAll('[data-drop-without]').forEach((b) => b.addEventListener('click', (e) => {
      this.without.delete(e.currentTarget.dataset.dropWithout);
      this.render();
    }));
    // The number field and the slider are two views of one value; neither re-renders
    // the panel, so dragging stays smooth and the caret is never stolen.
    slot.querySelectorAll('[data-assume]').forEach((row) => {
      const id = row.dataset.assume;
      const num = row.querySelector('[data-assume-value]');
      const rng = row.querySelector('[data-assume-range]');
      const set = (v) => {
        this.assume.set(id, Number(v));
        if (num.value !== v) num.value = v;
        if (rng && rng.value !== v) rng.value = v;
      };
      num.addEventListener('input', () => set(num.value));
      rng?.addEventListener('input', () => set(rng.value));
    });

    slot.querySelector('[data-act="run-estimate"]').addEventListener('click', async (e) => {
      const btn = e.currentTarget;
      btn.disabled = true;
      try {
        const res = await api.estimate({
          target: p.id,
          assume: [...this.assume.entries()].map(([k, v]) => `${k}=${v}`),
          without: [...this.without],
        });
        this.lastEstimate = res;
        this.render();
      } catch (err) {
        this.ctx.toast(err.message, 'err');
      } finally { btn.disabled = false; }
    });
  }

  /** The answer: level, projection, contributions, caveats, and the decision id. */
  outcome(res) {
    const a = res.answer || {};
    const hyp = res.hypothetical;
    const wrap = document.createElement('div');
    wrap.className = 'outcome';
    wrap.style.marginTop = '10px';

    wrap.append(node(`
      <div class="pair">
        <div class="col">
          <div class="cap">as it stands</div>
          <div class="val">${fmt.num(a.level, 3)}</div>
        </div>
        ${hyp ? `
        <div class="col">
          <div class="cap">under the supposal</div>
          <div class="val" style="color:${hyp.delta >= 0 ? 'var(--sign-pos)' : 'var(--sign-neg)'}">
            ${fmt.num(hyp.projected_level, 3)}
          </div>
          <div style="font-size:11.5px;color:var(--ink-secondary);font-variant-numeric:tabular-nums">
            ${fmt.signed(hyp.delta, 3)}
          </div>
        </div>` : ''}
      </div>
      <dl class="kv">
        <dt>confidence</dt><dd>${fmt.num(a.confidence, 3)}</dd>
        <dt>status</dt><dd>${escapeHTML(a.status || '—')}</dd>
        <dt>sensitivity</dt><dd>${fmt.num(a.sensitivity, 4)} per normalised unit</dd>
        <dt>revision</dt><dd>${fmt.int(res.revision)}</dd>
      </dl>`));

    const contribSection = node(`<div><h3 style="margin:12px 0 7px">Contributions</h3></div>`);
    contribSection.append(contributionBars(res.influences, { hypothetical: !!hyp }));
    wrap.append(contribSection);

    if (res.caveats?.length) {
      const c = node(`<div><h3 style="margin:12px 0 7px">Caveats the agent attached</h3></div>`);
      const ul = document.createElement('ul');
      ul.className = 'caveats';
      for (const cv of res.caveats) {
        const li = document.createElement('li');
        li.textContent = cv;
        ul.append(li);
      }
      c.append(ul);
      wrap.append(c);
    }

    if (res.rationale) {
      const r = node(`<div><h3 style="margin:12px 0 7px">Rationale</h3>
        <pre class="rationale">${escapeHTML(res.rationale)}</pre></div>`);
      wrap.append(r);
    }

    const trace = node(`
      <div class="row-between" style="margin-top:12px;gap:8px">
        <span class="badge mono" title="the decision id this answer was recorded under">
          ${escapeHTML(res.decision_id || '—')}
        </span>
        <button class="btn small" data-act="open-decision">Open the trace</button>
      </div>`);
    trace.querySelector('[data-act="open-decision"]').addEventListener('click',
      () => this.ctx.openDecision(res.decision_id));
    wrap.append(trace);
    return wrap;
  }

  // ── relationship ──────────────────────────────────────────────────────────

  renderRelationship(r, model) {
    const eff = effective(r);
    const frag = node(`
      <div class="inspector-head">
        <div class="title">
          <div class="eyebrow">relationship${r.label ? ` · ${escapeHTML(r.label)}` : ''}</div>
          <h2>${escapeHTML(r.from)} → ${escapeHTML(r.to)}</h2>
        </div>
        <button class="btn ghost small" data-act="close" aria-label="close">✕</button>
      </div>
      <div class="inspector-body">
        <div class="section">
          <div class="chips">
            <span class="badge" style="color:${r.sign < 0 ? 'var(--sign-neg)' : 'var(--sign-pos)'}">
              ${r.sign < 0 ? '↓ negative' : '↑ positive'}
            </span>
            <span class="badge"><span class="swatch" style="background:var(--prov-${escapeHTML(r.provenance)})"></span>
              ${escapeHTML(r.provenance)}</span>
            <span class="badge status-${r.status}">${statusGlyph(r.status)} ${r.status}</span>
            ${r.sign_suspect ? '<span class="badge alarm">! sign suspect</span>' : ''}
          </div>
          ${r.note ? `<p class="muted" style="font-size:11.5px;margin:9px 0 0;line-height:1.5">${escapeHTML(r.note)}</p>` : ''}
        </div>

        <div class="section">
          <h3>Strength <span class="muted" style="text-transform:none;letter-spacing:0;font-weight:400">
            by authority, not arithmetic</span></h3>
          <div data-slot="layers"></div>
        </div>

        <div class="section">
          <h3>Confidence</h3>
          <div data-slot="confidence"></div>
        </div>

        <div class="section">
          <h3>Declared sign vs observation</h3>
          <div data-slot="sign"></div>
        </div>

        <div class="section">
          <h3>Record</h3>
          <dl class="kv">
            <dt>id</dt><dd class="mono">${escapeHTML(r.id)}</dd>
            <dt>paired observations</dt><dd>${fmt.int(r.n_observations)}</dd>
            <dt>first observed</dt><dd>${fmt.ago(r.first_observed)}</dd>
            <dt>last observed</dt><dd>${fmt.ago(r.last_observed)}</dd>
            ${r.retired_reason ? `<dt>retired because</dt><dd>${escapeHTML(r.retired_reason)}</dd>` : ''}
          </dl>
        </div>

        <div class="section">
          <h3>Endpoints</h3>
          <table class="grid"><tbody>
            <tr data-goto="${escapeHTML(r.from)}"><td class="muted">source</td>
              <td class="mono">${escapeHTML(r.from)}</td>
              <td class="num">${valueOf(model, r.from)}</td></tr>
            <tr data-goto="${escapeHTML(r.to)}"><td class="muted">target</td>
              <td class="mono">${escapeHTML(r.to)}</td>
              <td class="num">${valueOf(model, r.to)}</td></tr>
          </tbody></table>
        </div>

        <div class="section">
          <h3>Operator</h3>
          <div class="inline">
            <button class="btn small" data-act="assert">Assert a strength…</button>
            <button class="btn small danger" data-act="retire-rel">Retire…</button>
          </div>
          <p class="muted" style="font-size:11px;margin:8px 0 0;line-height:1.5">
            An assertion outranks both learned layers and takes effect in full. The learned
            estimates are kept beside it, so what was measured stays readable.
          </p>
        </div>
      </div>`);

    this.root.replaceChildren(frag);
    this.root.querySelector('[data-slot="layers"]').append(strengthLayers(r));
    this.root.querySelector('[data-slot="confidence"]')
      .append(meter(r.confidence, `${fmt.num(r.confidence, 3)} · n=${r.n_observations}`));
    this.root.querySelector('[data-slot="sign"]').append(signSplit(r));
    this.wireCommon();
    this.applyScroll();

    this.root.querySelector('[data-act="assert"]').addEventListener('click', async () => {
      const got = await this.ctx.ask({
        title: `Assert strength for ${r.from} → ${r.to}`,
        fields: [
          { name: 'strength', label: 'Strength (0–1)', type: 'number', value: fmt.num(eff.value, 3), step: '0.01', min: '0', max: '1' },
          { name: 'reason', label: 'Reason (required)', placeholder: 'on what basis this overrides what was measured' },
          { name: 'actor', label: 'Actor', value: 'operator' },
        ],
        hint: 'The agent refuses an assertion with no stated reason: an override of measurement needs a record of why.',
        ok: 'Assert',
      });
      if (!got) return;
      if (!got.reason?.trim()) return this.ctx.toast('A reason is required.', 'err');
      // An empty field reads as 0 and garbage reads as NaN, which the wire turns
      // into null and Go decodes as 0.0 — an assertion that outranks both learned
      // layers, never decays, and is undone only by another assertion. Too costly
      // to arrive at by leaving a box blank.
      const strength = Number(got.strength);
      if (got.strength?.trim() === '' || !Number.isFinite(strength)
          || strength < 0 || strength > 1) {
        return this.ctx.toast('Strength must be a number between 0 and 1.', 'err');
      }
      try {
        await api.assertStrength(r.id, strength, got.reason.trim(), got.actor?.trim() || 'operator');
        this.ctx.toast('Assertion recorded', 'ok');
        this.ctx.refresh();
      } catch (err) { this.ctx.toast(err.message, 'err'); }
    });

    this.root.querySelector('[data-act="retire-rel"]').addEventListener('click', async () => {
      const got = await this.ctx.ask({
        title: `Retire ${r.from} → ${r.to}`,
        fields: [
          { name: 'reason', label: 'Reason (required)' },
          { name: 'actor', label: 'Actor', value: 'operator' },
        ],
        ok: 'Retire',
      });
      if (!got) return;
      if (!got.reason?.trim()) return this.ctx.toast('A reason is required.', 'err');
      try {
        await api.retireRelationship(r.id, got.reason.trim(), got.actor?.trim() || 'operator');
        this.ctx.toast('Relationship retired', 'ok');
        this.ctx.refresh();
      } catch (err) { this.ctx.toast(err.message, 'err'); }
    });
  }

  wireCommon() {
    this.root.querySelector('[data-act="close"]')?.addEventListener('click', () => this.ctx.selectProperty(null));
    this.root.querySelectorAll('[data-goto]').forEach((row) => {
      row.addEventListener('click', () => this.ctx.selectProperty(row.dataset.goto));
    });
    this.root.querySelectorAll('[data-goto-rel]').forEach((row) => {
      row.addEventListener('click', () => this.ctx.selectRelationship(row.dataset.gotoRel));
    });
  }
}

// ── helpers ──────────────────────────────────────────────────────────────────

function node(html) {
  const t = document.createElement('template');
  t.innerHTML = html.trim();
  return t.content;
}

const statusGlyph = (s) => (s === 'active' ? '●' : s === 'stale' ? '◐' : '○');

function valueOf(model, id) {
  const p = model.property(id);
  if (!p) return 'not held';
  return p.n_observations ? fmt.num(p.value, 3) : 'unobserved';
}

function relTable(rels, side) {
  if (!rels.length) {
    return `<p class="muted" style="font-size:11.5px;margin:0">None.</p>`;
  }
  return `<table class="grid">
    <thead><tr><th>${side === 'from' ? 'source' : 'target'}</th><th>sign</th>
      <th class="num">effective</th><th>basis</th></tr></thead>
    <tbody>${rels.map((r) => {
      const eff = effective(r);
      const other = side === 'from' ? r.from : r.to;
      return `<tr data-goto-rel="${escapeHTML(r.id)}">
        <td class="mono" title="${escapeHTML(other)}">${escapeHTML(other)}</td>
        <td style="color:${r.sign < 0 ? 'var(--sign-neg)' : 'var(--sign-pos)'}">${r.sign < 0 ? '↓' : '↑'}</td>
        <td class="num">${eff.known ? fmt.num(eff.value, 3) : '—'}</td>
        <td class="muted">${escapeHTML(eff.basis)}</td>
      </tr>`;
    }).join('')}</tbody></table>`;
}

function labelRows(labels) {
  const entries = Object.entries(labels || {});
  if (!entries.length) return '';
  return `<div class="chips" style="margin-top:8px">${entries
    .map(([k, v]) => `<span class="badge mono">${escapeHTML(k)}=${escapeHTML(v)}</span>`).join('')}</div>`;
}
