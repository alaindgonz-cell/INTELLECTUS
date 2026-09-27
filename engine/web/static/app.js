/* INTELLECTUS operator UI — vanilla ES2020, no dependencies, no build step.
 *
 * SECURITY (docs/UI_API.md, mandatory): everything that comes from the
 * server — chat text, proposals, plans, code, test output, diagnoses, event
 * summaries — is untrusted. It is rendered exclusively through text nodes /
 * textContent via the h() helper below. This file never uses innerHTML,
 * outerHTML, insertAdjacentHTML or document.write, and never builds URLs,
 * attributes or styles from server data. Keep it that way.
 */
(() => {
  'use strict';

  // ------------------------------------------------------------------ DOM

  const BLOCKED_PROPS = new Set(['innerHTML', 'outerHTML', 'srcdoc', 'style', 'href', 'src']);
  const PROP_KEYS = new Set(['hidden', 'disabled', 'value', 'checked', 'selected', 'tabIndex']);

  /** Create an element. String/number children become text nodes. */
  function h(tag, props, ...children) {
    const el = document.createElement(tag);
    if (props) {
      for (const [key, value] of Object.entries(props)) {
        if (value === undefined || value === null || value === false) continue;
        if (BLOCKED_PROPS.has(key)) throw new Error('h(): refusing to set ' + key);
        if (key.startsWith('on')) {
          if (typeof value !== 'function') throw new Error('h(): event handler must be a function');
          el.addEventListener(key.slice(2).toLowerCase(), value);
        } else if (key === 'class') {
          el.className = value;
        } else if (key === 'text') {
          el.textContent = String(value);
        } else if (key === 'dataset') {
          for (const [dk, dv] of Object.entries(value)) if (dv !== undefined && dv !== null) el.dataset[dk] = String(dv);
        } else if (PROP_KEYS.has(key)) {
          el[key] = value;
        } else {
          el.setAttribute(key, value === true ? '' : String(value));
        }
      }
    }
    appendAll(el, children);
    return el;
  }

  function appendAll(parent, children) {
    for (const c of children) {
      if (c === null || c === undefined || c === false) continue;
      if (Array.isArray(c)) appendAll(parent, c);
      else if (c instanceof Node) parent.appendChild(c);
      else parent.appendChild(document.createTextNode(String(c)));
    }
    return parent;
  }

  const $ = (id) => document.getElementById(id);
  const clear = (el) => { el.replaceChildren(); return el; };

  // ----------------------------------------------------------- formatting

  function toMs(t) {
    const n = Number(t);
    if (!Number.isFinite(n) || n <= 0) return null;
    if (n > 1e14) return Math.round(n / 1e6); // nanoseconds
    if (n > 1e11) return n; // milliseconds
    return n * 1000; // seconds
  }
  function fmtTime(t) {
    const ms = toMs(t);
    if (ms === null) return '—';
    return new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
  }
  function fmtDateTime(t) {
    const ms = toMs(t);
    return ms === null ? '' : new Date(ms).toLocaleString();
  }
  function fmtElapsed(ms) {
    const s = Math.max(0, Math.floor(ms / 1000));
    if (s < 60) return s + 's';
    const m = Math.floor(s / 60);
    return m + 'm ' + String(s % 60).padStart(2, '0') + 's';
  }
  function fmtInt(n) {
    const v = Number(n);
    return Number.isFinite(v) ? v.toLocaleString('en-US') : '—';
  }
  function fmtTokens(n) {
    const v = Number(n);
    if (!Number.isFinite(v)) return '—';
    if (v >= 1e6) return (v / 1e6).toFixed(2) + 'M';
    if (v >= 1e4) return (v / 1e3).toFixed(0) + 'k';
    if (v >= 1e3) return (v / 1e3).toFixed(1) + 'k';
    return String(v);
  }
  function fmtUsd(s, digits) {
    const v = Number(s);
    if (!Number.isFinite(v)) return '—';
    return '$' + v.toFixed(digits);
  }
  function shortDigest(d) {
    const s = String(d ?? '');
    const m = /^([a-z0-9]+):([0-9a-f]{16,})$/i.exec(s);
    if (m) return m[1] + ':' + m[2].slice(0, 10) + '…';
    return s.length > 28 ? s.slice(0, 24) + '…' : s;
  }
  /** Render a JSON-ish value as a compact literal (strings quoted so whitespace is visible). */
  function fmtValue(v) {
    if (v === undefined) return '';
    try { return JSON.stringify(v); } catch (_) { return String(v); }
  }
  /** Render a value as plain text (strings as-is). */
  function fmtText(v) {
    if (v === undefined || v === null) return '';
    if (typeof v === 'string') return v;
    try { return JSON.stringify(v, null, 2); } catch (_) { return String(v); }
  }
  function fmtExpect(expect) {
    if (expect && typeof expect === 'object') {
      if ('raises' in expect) return { text: 'raises ' + fmtText(expect.raises), raises: true };
      if ('returns' in expect) return { text: '→ ' + fmtValue(expect.returns), raises: false };
    }
    if (typeof expect === 'string') return { text: expect, raises: /^raise/i.test(expect) };
    return { text: fmtValue(expect), raises: false };
  }
  function humanize(s) { return String(s ?? '').replace(/_/g, ' '); }

  const TONES = {
    ok: ['ok', 'pass', 'passed', 'completed', 'approved', 'verified', 'succeeded', 'success', 'valid', 'done', 'executed', 'applied'],
    fail: ['fail', 'failed', 'error', 'rejected', 'invalid', 'unavailable'],
    warn: ['warn', 'warning', 'timeout', 'stopped', 'escalated', 'stale', 'unknown', 'outcome_unknown', 'indeterminate', 'awaiting_approval', 'untrusted', 'unverified'],
    run: ['running', 'open', 'pending', 'in_progress', 'coding', 'testing', 'routing', 'planning', 'repairing', 'dispatch_started', 'authorized_intent', 'promoting'],
    info: ['info', 'shadow', 'live', 'simulated'],
    neutral: ['cancelled', 'canceled', 'discarded', 'off', 'skipped', 'none'],
  };
  const TONE_OF = new Map();
  for (const [tone, list] of Object.entries(TONES)) for (const s of list) TONE_OF.set(s, tone);
  const toneOf = (s) => TONE_OF.get(String(s ?? '').toLowerCase()) || 'neutral';

  const STATUS_LABEL = { running: 'Running', ok: 'OK', fail: 'Fail', warn: 'Warning', info: 'Info' };

  function badge(text, tone, extraClass) {
    const t = tone || toneOf(text);
    return h('span', { class: 'badge ' + t + (extraClass ? ' ' + extraClass : '') }, humanize(text));
  }

  // --------------------------------------------------- safe rich renderer

  /**
   * Render untrusted message text. The only formatting: ``` fenced blocks
   * become <pre><code>, `inline` spans become <code>, line breaks are kept
   * (CSS white-space: pre-wrap). Everything is a text node.
   */
  function renderRich(text) {
    const root = h('div', { class: 'rich' });
    const parts = String(text ?? '').split('```');
    for (let i = 0; i < parts.length; i++) {
      let part = parts[i];
      if (i % 2 === 1) {
        let lang = '';
        const nl = part.indexOf('\n');
        if (nl >= 0) {
          const first = part.slice(0, nl).trim();
          if (first === '' || /^[A-Za-z0-9_+#.-]{1,24}$/.test(first)) {
            lang = first;
            part = part.slice(nl + 1);
          }
        }
        part = part.replace(/\n$/, '');
        const code = h('code', null, part);
        root.appendChild(h('pre', { class: 'code-block', 'data-lang': lang || null, 'aria-label': lang ? lang + ' code' : 'code' }, code));
      } else {
        if (i > 0) part = part.replace(/^\n/, '');
        if (i < parts.length - 1) part = part.replace(/\n$/, '');
        if (part) renderInline(part, root);
      }
    }
    return root;
  }

  function renderInline(text, parent) {
    const segs = text.split(/(`[^`\n]+`)/);
    for (const seg of segs) {
      if (!seg) continue;
      if (seg.length > 2 && seg.startsWith('`') && seg.endsWith('`')) {
        parent.appendChild(h('code', { class: 'inline' }, seg.slice(1, -1)));
      } else {
        parent.appendChild(document.createTextNode(seg));
      }
    }
  }

  // ------------------------------------------------------------------ API

  const SESSION_EXPIRED = 'Session expired — reopen the URL printed by `intellectus serve`';

  class ApiError extends Error {
    constructor(status, message) { super(message); this.status = status; }
  }

  async function api(path, opts = {}) {
    const method = opts.method || 'GET';
    const init = { method, credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' } };
    if (method !== 'GET') {
      init.headers['X-Intellectus-CSRF'] = '1';
      if (opts.body !== undefined) {
        init.headers['Content-Type'] = 'application/json';
        init.body = JSON.stringify(opts.body);
      }
    }
    let res;
    try {
      res = await fetch(path, init);
    } catch (_) {
      throw new ApiError(0, 'Cannot reach the INTELLECTUS server — is `intellectus serve` still running?');
    }
    if (res.status === 401) {
      onSessionExpired();
      throw new ApiError(401, SESSION_EXPIRED);
    }
    const raw = await res.text();
    let data = null;
    if (raw) { try { data = JSON.parse(raw); } catch (_) { data = null; } }
    if (!res.ok) {
      let msg = data && (data.error ?? data.message);
      if (msg && typeof msg === 'object') msg = msg.message || msg.code || JSON.stringify(msg);
      if (!msg) msg = raw.trim().slice(0, 300) || res.statusText;
      throw new ApiError(res.status, res.status + ' — ' + msg);
    }
    return data || {};
  }

  const enc = encodeURIComponent;

  // ---------------------------------------------------------------- state

  const KINDS = ['model', 'sandbox', 'core', 'jev', 'approval', 'gateway', 'system', 'error'];
  const KIND_LABEL = { model: 'Model', sandbox: 'Sandbox', core: 'Core', jev: 'Jev', approval: 'Approval', gateway: 'Gateway', system: 'System', error: 'Error' };
  const KIND_GLYPH = { model: 'M', sandbox: 'S', core: 'C', jev: 'J', approval: 'A', gateway: 'G', system: 'i', error: '!' };
  const ACTIVITY_CAP = 1000;

  const state = {
    status: null,
    expired: false,
    conn: 'connecting',
    // chat
    messages: new Map(),
    msgOrder: [],
    msgNodes: new Map(),
    chatLoaded: false,
    chatStick: true,
    pendingReply: 0,
    sending: false,
    cardBusy: new Set(),
    // activity
    items: new Map(),
    itemOrder: [],
    itemNodes: new Map(),
    expandedItems: new Set(),
    actFilter: { kind: null, task: null },
    actStick: true,
    // tasks
    tasks: [],
    openTask: null,
    taskDetail: null,
    taskDetailError: null,
    expandedCell: null,
    confirmCancel: false,
    // files
    files: { loaded: false, mode: 'tree', root: '', tree: null, selected: null, from: '', to: '', diff: null, loading: false, error: null },
    knownRoots: new Map(),
    // events
    events: [],
    eventsLoaded: false,
    eventsLoading: false,
    // ui
    workTab: 'activity',
    dismissed: new Set(),
    popover: null,
  };

  // --------------------------------------------------------------- toasts

  function toast(message, tone) {
    const el = h('div', { class: 'toast' + (tone === 'fail' ? ' fail' : '') }, renderRich(message));
    $('toasts').appendChild(el);
    setTimeout(() => el.remove(), tone === 'fail' ? 8000 : 4500);
  }

  function reportError(err) {
    if (err && err.status === 401) return; // banner already shown
    toast(err && err.message ? err.message : String(err), 'fail');
  }

  // ------------------------------------------------------ session / notices

  function onSessionExpired() {
    if (state.expired) return;
    state.expired = true;
    setConn('expired');
    if (es) { es.close(); es = null; }
    clearTimeout(reconnectTimer);
    const input = $('composer-input');
    input.disabled = true;
    input.placeholder = 'Session expired';
    $('send-btn').disabled = true;
    renderNotices();
  }

  function renderNotices() {
    const box = clear($('notices'));
    if (state.expired) {
      box.appendChild(h('div', { class: 'notice fail', role: 'alert' },
        h('span', { class: 'notice-icon', 'aria-hidden': 'true' }),
        h('span', { class: 'notice-text' }, 'Session expired — reopen the URL printed by ', h('code', null, 'intellectus serve'))));
    }
    const s = state.status;
    if (!s) return;
    const seen = new Map();
    for (const [name, part] of [['Core', s.core], ['Claude', s.claude], ['Jev', s.jev], ['Sandbox', s.sandbox]]) {
      const d = part && typeof part.detail === 'string' ? part.detail.trim() : '';
      if (!d) continue;
      if (!seen.has(d)) seen.set(d, []);
      seen.get(d).push(name);
    }
    for (const [text, sources] of seen) {
      if (state.dismissed.has(text)) continue;
      box.appendChild(h('div', { class: 'notice', title: 'Reported by: ' + sources.join(', ') },
        h('span', { class: 'notice-icon', 'aria-hidden': 'true' }),
        h('span', { class: 'notice-text' }, text),
        h('button', { type: 'button', class: 'btn btn-ghost btn-sm notice-dismiss', 'aria-label': 'Dismiss notice', onclick: () => { state.dismissed.add(text); renderNotices(); } },
          h('span', { class: 'dismiss-label' }, 'Dismiss'), h('span', { class: 'dismiss-x', 'aria-hidden': 'true' }, '×'))));
    }
  }

  // ----------------------------------------------------------- connection

  const CONN_LABEL = { live: 'Live', connecting: 'Connecting…', reconnecting: 'Reconnecting…', offline: 'Offline — retrying', expired: 'Signed out' };
  function setConn(s) {
    state.conn = s;
    const el = $('conn');
    el.dataset.state = s;
    el.querySelector('.conn-label').textContent = CONN_LABEL[s] || s;
    el.title = s === 'live' ? 'Receiving live updates' : CONN_LABEL[s] || '';
  }

  // ---------------------------------------------------------------- status

  function mainModel(models) {
    if (!models || typeof models !== 'object') return '';
    for (const role of ['coder', 'planner', 'intake', 'tester']) if (models[role]) return String(models[role]);
    const first = Object.values(models)[0];
    return first ? String(first) : '';
  }

  function sandboxState(sb) {
    if (!sb || !sb.available) return { tone: 'fail', label: 'unavailable' };
    if (!sb.verified) return { tone: 'warn', label: 'unverified' };
    if (sb.trusted === false) return { tone: 'warn', label: 'untrusted' };
    return { tone: 'ok', label: 'verified' };
  }

  function setPill(id, tone, parts, title) {
    const pill = $(id);
    pill.dataset.state = tone;
    appendAll(clear(pill.querySelector('.pill-value')), parts);
    if (title) pill.title = title;
  }

  let lastApprovedRoot = null;

  function renderStatus() {
    const s = state.status;
    if (!s) return;
    if (lastApprovedRoot !== null && s.approved_root !== lastApprovedRoot && state.files.loaded && state.files.mode === 'tree' && !state.files.root) {
      loadTree();
    }
    lastApprovedRoot = s.approved_root || '';
    if (state.pendingReply && !s.busy && Date.now() - state.pendingReply > 3000) {
      state.pendingReply = 0;
      $('typing').hidden = true;
    }
    const core = s.core || {};
    // .sec parts are secondary and hidden on narrow screens.
    const sec = (text, cls) => (text ? h('span', { class: cls || 'sec' }, text) : null);
    setPill('pill-core', s.busy ? 'run' : 'ok',
      [h('b', null, '#' + fmtInt(s.head_sequence)), sec(' · ' + (core.deductor || '—'))],
      'Core head sequence ' + s.head_sequence + (s.busy ? ' — working' : ''));
    const cl = s.claude || {};
    setPill('pill-claude', cl.configured ? 'ok' : 'warn', [cl.configured ? mainModel(cl.models) || 'configured' : 'not configured']);
    const jev = s.jev || {};
    const mode = String(jev.policy_mode || 'OFF').toUpperCase();
    const jevTone = mode === 'OFF' ? 'neutral' : !jev.configured ? 'warn' : mode === 'LIVE' ? 'ok' : 'info';
    setPill('pill-jev', jevTone, [h('b', null, mode), jev.configured ? sec(jev.model ? ' · ' + jev.model : '') : ' · not configured']);
    const sb = sandboxState(s.sandbox);
    setPill('pill-sandbox', sb.tone, [sb.label, sec(s.sandbox && s.sandbox.kind ? ' · ' + s.sandbox.kind : '', 'sec tert')]);
    const u = s.usage || {};
    const tokens = (Number(u.input_tokens) || 0) + (Number(u.output_tokens) || 0);
    setPill('pill-usage', 'info', [sec(fmtInt(u.model_calls ?? 0) + ' calls · ' + fmtTokens(tokens) + ' tok · '), '≈' + fmtUsd(u.estimated_cost_usd ?? 0, 2) + ' est.'],
      'Estimated cost — computed from token counts, not a bill');
    syncJevControls();
    if (s.approved_root) addKnownRoot(s.approved_root, 'approved root');
    renderNotices();
    if (state.popover) renderPopover();
    renderEventsFooter();
    renderRootSelects();
  }

  async function loadStatus() {
    state.status = await api('/api/status');
    renderStatus();
  }

  // ------------------------------------------------------ Jev mode control

  let jevBusy = false;
  function currentJevMode() {
    return String(state.status?.jev?.policy_mode || '').toUpperCase();
  }
  function syncJevControls() {
    const mode = currentJevMode();
    for (const b of document.querySelectorAll('[data-jev-mode]')) {
      b.setAttribute('aria-checked', String(b.dataset.jevMode === mode));
      b.disabled = jevBusy || state.expired;
    }
  }
  async function setJevMode(mode) {
    if (jevBusy || mode === currentJevMode()) return;
    jevBusy = true;
    const prev = currentJevMode();
    if (state.status && state.status.jev) state.status.jev.policy_mode = mode;
    syncJevControls();
    try {
      await api('/api/policy/jev-mode', { method: 'POST', body: { mode } });
      toast('Jev routing set to ' + mode + '.');
      loadStatus().catch(() => {});
    } catch (err) {
      if (state.status && state.status.jev) state.status.jev.policy_mode = prev;
      reportError(err);
    } finally {
      jevBusy = false;
      renderStatus();
    }
  }
  function jevModeControl() {
    const mode = currentJevMode();
    const seg = h('div', { class: 'seg', role: 'radiogroup', 'aria-label': 'Jev routing mode' });
    for (const [m, label] of [['SHADOW', 'Shadow'], ['LIVE', 'Live'], ['OFF', 'Off']]) {
      seg.appendChild(h('button', {
        type: 'button', role: 'radio', 'data-jev-mode': m, 'aria-checked': String(m === mode),
        disabled: jevBusy || state.expired, onclick: () => setJevMode(m),
      }, label));
    }
    return seg;
  }

  // -------------------------------------------------------------- popover

  let popAnchor = null;
  let trustBusy = false;

  function kv(rows) {
    const dl = h('dl', { class: 'kv' });
    for (const [k, v, mono] of rows) {
      if (v === undefined || v === null || v === '') continue;
      dl.append(h('dt', null, k), h('dd', { class: mono ? 'mono' : null }, v));
    }
    return dl;
  }

  function digestButton(d, labelPrefix) {
    if (!d) return h('span', { class: 'muted' }, '—');
    const full = String(d);
    const shown = shortDigest(full);
    const btn = h('button', {
      type: 'button', class: 'digest', title: 'Click to copy ' + full,
      'aria-label': (labelPrefix || 'Copy') + ' ' + full,
      onclick: (e) => { e.stopPropagation(); copyText(full, btn, shown); },
    }, shown);
    return btn;
  }

  async function copyText(text, btn, restore) {
    let ok = false;
    try {
      if (navigator.clipboard && window.isSecureContext) { await navigator.clipboard.writeText(text); ok = true; }
    } catch (_) { ok = false; }
    if (!ok) {
      const ta = h('textarea', { class: 'sr-only', 'aria-hidden': 'true' });
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
      ta.remove();
    }
    if (btn) {
      btn.classList.add('copied');
      btn.textContent = ok ? 'Copied ✓' : 'Copy failed';
      setTimeout(() => { btn.classList.remove('copied'); btn.textContent = restore; }, 1400);
    }
  }

  function popoverContent(which) {
    const s = state.status || {};
    const head = (title) => h('div', { class: 'pop-head' },
      h('h3', { id: 'popover-title' }, title),
      h('button', { type: 'button', class: 'pop-close', 'aria-label': 'Close', onclick: closePopover }, '×'));
    const detail = (d) => (d ? h('div', { class: 'pop-detail' }, d) : null);

    if (which === 'core') {
      const c = s.core || {};
      return [head('Core'), kv([
        ['Project', s.project_id],
        ['Approved root', digestButton(s.approved_root)],
        ['Head sequence', '#' + fmtInt(s.head_sequence)],
        ['Reducer', c.reducer_version !== undefined ? 'v' + c.reducer_version : ''],
        ['Deductor', c.deductor],
        ['Deductor digest', c.deductor_digest, true],
        ['State', s.busy ? 'working' : 'idle'],
      ]), detail(c.detail),
      h('p', { class: 'pop-note' }, 'The core is the only writer: every change is an event in its append-only log (see Event log).')];
    }
    if (which === 'claude') {
      const c = s.claude || {};
      const models = c.models && typeof c.models === 'object' ? Object.entries(c.models) : [];
      return [head('Claude'), kv([
        ['Configured', c.configured ? 'yes' : 'no'],
        ['Provider', c.provider],
        ...models.map(([role, m]) => [humanize(role), String(m), true]),
      ]), detail(c.detail)];
    }
    if (which === 'jev') {
      const j = s.jev || {};
      return [head('Jev'), kv([
        ['Configured', j.configured ? 'yes' : 'no'],
        ['Policy mode', j.policy_mode],
        ['Model', j.model, true],
      ]), detail(j.detail),
      h('div', { class: 'pop-section' }, h('div', { class: 'section-label' }, 'Routing mode'), jevModeControl(),
        h('p', { class: 'pop-note' }, 'LIVE lets Jev choose routes when confident. SHADOW only records its advice while deterministic routing decides. OFF does not consult Jev.'))];
    }
    if (which === 'sandbox') {
      const sb = s.sandbox || {};
      const st = sandboxState(sb);
      const probes = Array.isArray(sb.probes) ? sb.probes : [];
      const nodes = [head('Sandbox'), kv([
        ['State', badge(st.label, st.tone)],
        ['Kind', sb.kind],
        ['Available', sb.available ? 'yes' : 'no'],
        ['Verified', sb.verified ? 'yes' : 'no'],
        ['Trusted', sb.trusted ? 'yes' : 'no'],
        ['Verified at', fmtDateTime(sb.verified_at)],
      ]), detail(sb.detail)];
      if (sb.implementation_digest) {
        const full = String(sb.implementation_digest);
        const copy = h('button', { type: 'button', class: 'link-btn', 'aria-label': 'Copy implementation digest' }, 'Copy');
        copy.addEventListener('click', () => copyText(full, copy, 'Copy'));
        nodes.push(h('div', { class: 'pop-section' }, h('div', { class: 'section-label' }, 'Implementation digest', copy),
          h('div', { class: 'digest-full' }, full)));
      }
      if (sb.trusted === false) {
        const btn = h('button', { type: 'button', class: 'btn btn-primary btn-sm', disabled: trustBusy || state.expired }, trustBusy ? 'Trusting…' : 'Trust this sandbox');
        btn.addEventListener('click', async () => {
          if (trustBusy) return;
          trustBusy = true;
          btn.disabled = true;
          btn.textContent = 'Trusting…';
          try {
            await api('/api/policy/trust-sandbox', { method: 'POST' });
            toast('Sandbox implementation trusted (signed policy change).');
            trustBusy = false;
            await loadStatus();
          } catch (err) {
            trustBusy = false;
            reportError(err);
            if (state.popover) renderPopover();
          }
        });
        nodes.push(h('div', { class: 'pop-actions' }, btn),
          h('p', { class: 'pop-note' }, 'Trusting signs a policy change: the core will accept acceptance-test receipts from this exact implementation digest.'));
      }
      nodes.push(h('div', { class: 'pop-section' },
        h('div', { class: 'section-label' }, 'Probes (' + probes.filter((p) => p.ok).length + '/' + probes.length + ' passed)'),
        probes.length
          ? h('ul', { class: 'probes' }, probes.map((p) => h('li', { class: 'probe' },
            badge(p.ok ? 'pass' : 'fail', p.ok ? 'ok' : 'fail'),
            h('span', { class: 'pn' }, p.name),
            h('span', { class: 'pd' }, p.detail || ''))))
          : h('p', { class: 'muted small' }, 'No probe results reported.')));
      return nodes;
    }
    if (which === 'usage') {
      const u = s.usage || {};
      return [head('Usage'), kv([
        ['Model calls', fmtInt(u.model_calls ?? 0)],
        ['Input tokens', fmtInt(u.input_tokens ?? 0)],
        ['Output tokens', fmtInt(u.output_tokens ?? 0)],
        ['Cache reads', fmtInt(u.cache_read_input_tokens ?? 0)],
        ['Estimated cost', '≈ ' + fmtUsd(u.estimated_cost_usd ?? 0, 4) + ' (estimate)'],
        ['Jev calls', fmtInt(u.jev_calls ?? 0)],
        ['Jev cost', '≈ ' + fmtUsd(u.jev_cost_usd ?? 0, 4) + ' (estimate)'],
      ]), h('p', { class: 'pop-note' }, 'Costs are estimates from token counts and list prices — not a bill.')];
    }
    return [head(which)];
  }

  function renderPopover() {
    const pop = $('popover');
    if (!state.popover) { pop.hidden = true; return; }
    const hadFocus = pop.contains(document.activeElement);
    appendAll(clear(pop), popoverContent(state.popover));
    pop.hidden = false;
    positionPopover();
    if (hadFocus) pop.focus();
  }

  function positionPopover() {
    const pop = $('popover');
    if (!popAnchor || pop.hidden) return;
    const r = popAnchor.getBoundingClientRect();
    const w = pop.offsetWidth;
    const left = Math.max(16, Math.min(r.left, window.innerWidth - w - 16));
    pop.style.left = left + 'px';
    pop.style.top = Math.round(r.bottom + 8) + 'px';
  }

  function openPopover(which, anchor) {
    if (state.popover === which) { closePopover(); return; }
    closePopover(false);
    state.popover = which;
    popAnchor = anchor;
    anchor.setAttribute('aria-expanded', 'true');
    const pop = $('popover');
    pop.tabIndex = -1;
    renderPopover();
    pop.focus();
  }

  function closePopover(restoreFocus = true) {
    if (!state.popover) return;
    state.popover = null;
    $('popover').hidden = true;
    if (popAnchor) {
      popAnchor.setAttribute('aria-expanded', 'false');
      if (restoreFocus) popAnchor.focus();
    }
    popAnchor = null;
  }

  // ----------------------------------------------------------------- chat

  const ROLES = new Set(['user', 'assistant', 'system']);

  function isNearBottom(el, slack = 60) {
    return el.scrollHeight - el.scrollTop - el.clientHeight < slack;
  }

  function upsertMessage(m, opts = {}) {
    if (!m || typeof m.id !== 'string') return;
    const exists = state.messages.has(m.id);
    state.messages.set(m.id, m);
    collectRootsFromCard(m.card);
    const node = renderMessage(m);
    const list = $('messages');
    if (exists && state.msgNodes.has(m.id)) {
      state.msgNodes.get(m.id).replaceWith(node);
    } else {
      // Insert ordered by ts (almost always an append).
      let idx = state.msgOrder.length;
      const ts = Number(m.ts) || 0;
      while (idx > 0 && (Number(state.messages.get(state.msgOrder[idx - 1])?.ts) || 0) > ts) idx--;
      const before = idx < state.msgOrder.length ? state.msgNodes.get(state.msgOrder[idx]) : null;
      state.msgOrder.splice(idx, 0, m.id);
      list.insertBefore(node, before || null);
    }
    state.msgNodes.set(m.id, node);
    if (m.role !== 'user' && state.pendingReply && !exists) state.pendingReply = 0;
    if (!opts.batch) afterChatChange(m.role === 'user' && !exists);
  }

  function afterChatChange(forceScroll) {
    $('chat-empty').hidden = !(state.chatLoaded && state.messages.size === 0);
    $('typing').hidden = !state.pendingReply;
    const sc = $('chat-scroll');
    if (forceScroll || state.chatStick) sc.scrollTop = sc.scrollHeight;
  }

  function taskChip(taskId, onclick, label) {
    if (!taskId) return null;
    return h('button', { type: 'button', class: 'chip', title: label || 'Open ' + taskId, onclick }, String(taskId));
  }

  function renderMessage(m) {
    const role = ROLES.has(m.role) ? m.role : 'system';
    const li = h('li', { class: 'msg ' + role + (m.card ? ' has-card' : ''), dataset: { id: m.id } });
    const time = h('time', { datetime: toMs(m.ts) ? new Date(toMs(m.ts)).toISOString() : null, title: fmtDateTime(m.ts) }, fmtTime(m.ts));
    const openTask = () => openTaskDetail(m.task_id);
    if (role === 'user') {
      li.append(h('div', { class: 'msg-meta' }, h('span', { class: 'sr-only' }, 'You'), time));
      li.append(h('div', { class: 'bubble' }, renderRich(m.text)));
    } else if (role === 'assistant') {
      li.append(h('div', { class: 'msg-meta' }, h('span', { class: 'who' }, 'INTELLECTUS'), '·', time,
        m.task_id ? taskChip(m.task_id, openTask) : null));
      const bubble = h('div', { class: 'bubble' });
      if (m.text) bubble.append(renderRich(m.text));
      if (m.card) bubble.append(renderCard(m.card));
      li.append(bubble);
    } else {
      li.append(h('div', { class: 'sys-text' }, h('span', { class: 'sr-only' }, 'System notice: '), renderRich(m.text), ' · ', time));
      if (m.card) {
        const c = renderCard(m.card);
        c.classList.add('sys-card');
        li.append(c);
      }
    }
    return li;
  }

  // ---------------------------------------------------------------- cards

  function renderCard(card) {
    if (!card || typeof card !== 'object') return h('div');
    if (card.type === 'proposal') return proposalCard(card);
    if (card.type === 'approval') return approvalCard(card);
    if (card.type === 'report') return reportCard(card);
    return h('div', { class: 'card' }, h('div', { class: 'card-body' }, h('span', { class: 'muted small' }, 'Unsupported card type: ' + String(card.type))));
  }

  function cardShell(kindClass, kindLabel, status, title) {
    const card = h('div', { class: 'card kind-' + kindClass, dataset: { status: status || '' } });
    card.append(h('div', { class: 'card-head' }, h('span', { class: 'card-kind' }, kindLabel), status ? badge(status) : null));
    if (title) card.append(h('div', { class: 'card-title' }, title));
    return card;
  }

  async function cardAction(key, fn, buttons, busyLabel, btn) {
    if (state.cardBusy.has(key)) return;
    state.cardBusy.add(key);
    for (const b of buttons) b.disabled = true;
    const orig = btn.textContent;
    btn.textContent = busyLabel;
    try {
      await fn();
    } catch (err) {
      reportError(err);
      if (err && err.status === 409) loadChat().catch(() => {});
      for (const b of buttons) b.disabled = false;
      btn.textContent = orig;
    } finally {
      state.cardBusy.delete(key);
    }
  }

  function patchCard(pred, patch) {
    for (const m of state.messages.values()) {
      if (m.card && pred(m.card)) {
        upsertMessage({ ...m, card: { ...m.card, ...patch } });
        return;
      }
    }
  }

  function casesTable(cases) {
    const rows = (Array.isArray(cases) ? cases : []).map((c) => {
      const ex = fmtExpect(c.expect);
      return h('tr', null,
        h('td', { class: 'mono' }, String(c.name ?? '')),
        h('td', { class: 'mono' }, fmtValue(c.input)),
        h('td', { class: 'mono' + (ex.raises ? ' exp-raises' : '') }, ex.text));
    });
    return h('div', { class: 'table-wrap' }, h('table', { class: 'data' },
      h('thead', null, h('tr', null, h('th', { scope: 'col' }, 'Name'), h('th', { scope: 'col' }, 'Input'), h('th', { scope: 'col' }, 'Expectation'))),
      h('tbody', null, rows)));
  }

  function entrypointText(ep) {
    if (!ep || typeof ep !== 'object') return '';
    return String(ep.path ?? '') + '::' + String(ep.function ?? '');
  }

  function proposalCard(c) {
    const status = String(c.status || 'pending');
    const card = cardShell('proposal', 'Proposal', status, c.title || 'Untitled proposal');
    const cases = Array.isArray(c.cases) ? c.cases : [];
    const body = h('div', { class: 'card-body' });
    if (c.requirement) body.append(h('div', null, h('div', { class: 'section-label' }, 'Requirement'), h('div', { class: 'card-text' }, c.requirement)));
    if (c.entrypoint) {
      body.append(h('div', { class: 'card-row' }, h('span', { class: 'k' }, 'Entrypoint'),
        h('span', { class: 'v' }, h('code', { class: 'mono' }, entrypointText(c.entrypoint)),
          c.entrypoint.language ? h('span', { class: 'muted' }, ' · ' + c.entrypoint.language) : null)));
    }
    body.append(h('div', null, h('div', { class: 'section-label' }, 'Acceptance tests (' + cases.length + ')'), casesTable(cases)));
    card.append(body);

    const actions = h('div', { class: 'card-actions' });
    const key = 'proposal:' + c.proposal_id;
    if (status === 'pending') {
      const approve = h('button', { type: 'button', class: 'btn btn-primary', disabled: state.cardBusy.has(key) }, 'Approve & start');
      const discard = h('button', { type: 'button', class: 'btn btn-secondary', disabled: state.cardBusy.has(key) }, 'Discard');
      approve.addEventListener('click', () => cardAction(key, async () => {
        const res = await api('/api/proposals/' + enc(c.proposal_id) + '/approve', { method: 'POST' });
        patchCard((x) => x.type === 'proposal' && x.proposal_id === c.proposal_id, { status: 'approved', task_id: res.task_id || c.task_id || null });
        if (res.task_id) toast('Task started: ' + res.task_id);
      }, [approve, discard], 'Approving…', approve));
      discard.addEventListener('click', () => cardAction(key, async () => {
        await api('/api/proposals/' + enc(c.proposal_id) + '/discard', { method: 'POST' });
        patchCard((x) => x.type === 'proposal' && x.proposal_id === c.proposal_id, { status: 'discarded' });
      }, [approve, discard], 'Discarding…', discard));
      actions.append(approve, discard, h('span', { class: 'card-note muted' }, 'Approving signs the requirement and these tests, then starts work.'));
    } else if (status === 'approved') {
      actions.append(h('span', { class: 'card-note' }, 'Task started: '),
        c.task_id ? taskChip(c.task_id, () => openTaskDetail(c.task_id)) : h('span', { class: 'muted' }, 'pending id'));
    } else if (status === 'discarded') {
      actions.append(h('span', { class: 'card-note muted' }, 'Discarded — nothing was started.'));
    } else {
      actions.append(h('span', { class: 'card-note muted' }, humanize(status)));
    }
    card.append(actions);
    return card;
  }

  function approvalCard(c) {
    const status = String(c.status || 'pending');
    const tool = c.tool_id ? String(c.tool_id) : 'action';
    const card = cardShell('approval', 'Approval · ' + tool, status, c.title || 'Approval requested');
    const body = h('div', { class: 'card-body' });
    if (c.summary) body.append(h('div', { class: 'card-text' }, c.summary));
    const checks = Array.isArray(c.checks) ? c.checks : [];
    body.append(h('div', null, h('div', { class: 'section-label' }, 'Checks'),
      checks.length
        ? h('ul', { class: 'checks' }, checks.map((ch) => h('li', { class: 'check' },
          badge(ch.result || 'UNKNOWN'),
          h('span', { class: 'check-kind' }, String(ch.kind ?? 'check')),
          h('span', { class: 'check-sub' }, [ch.detail ? String(ch.detail) : null, ch.issuer ? 'issued by ' + ch.issuer : null].filter(Boolean).join(' · ')))))
        : h('p', { class: 'muted small' }, 'No checks attached.')));
    body.append(h('div', { class: 'card-row' }, h('span', { class: 'k' }, 'Action digest'), h('span', { class: 'v' }, digestButton(c.action_digest, 'Copy action digest'))));
    if (c.from_root || c.to_root) {
      const canDiff = c.from_root && c.to_root && c.from_root !== c.to_root;
      body.append(h('div', { class: 'card-row' }, h('span', { class: 'k' }, 'Transition'),
        h('span', { class: 'v' }, digestButton(c.from_root), ' → ', digestButton(c.to_root), ' ',
          canDiff ? h('button', { type: 'button', class: 'link-btn', onclick: () => openDiff(c.from_root, c.to_root) }, 'View diff') : null)));
    }
    if (c.task_id) body.append(h('div', { class: 'card-row' }, h('span', { class: 'k' }, 'Task'), h('span', { class: 'v' }, taskChip(c.task_id, () => openTaskDetail(c.task_id)))));
    card.append(body);

    const actions = h('div', { class: 'card-actions' });
    const key = 'approval:' + c.approval_id;
    if (status === 'pending') {
      const approve = h('button', { type: 'button', class: 'btn btn-primary', disabled: state.cardBusy.has(key) }, 'Approve');
      const reject = h('button', { type: 'button', class: 'btn btn-danger', disabled: state.cardBusy.has(key) }, 'Reject');
      approve.addEventListener('click', () => cardAction(key, async () => {
        await api('/api/approvals/' + enc(c.approval_id) + '/approve', { method: 'POST' });
        patchCard((x) => x.type === 'approval' && x.approval_id === c.approval_id, { status: 'approved' });
      }, [approve, reject], 'Approving…', approve));
      reject.addEventListener('click', () => cardAction(key, async () => {
        await api('/api/approvals/' + enc(c.approval_id) + '/reject', { method: 'POST' });
        patchCard((x) => x.type === 'approval' && x.approval_id === c.approval_id, { status: 'rejected' });
      }, [approve, reject], 'Rejecting…', reject));
      actions.append(approve, reject, h('span', { class: 'card-note muted' }, 'Approving signs this exact action digest.'));
    } else {
      const note = {
        approved: 'Approved — bound to this exact action digest.',
        rejected: 'Rejected — nothing was changed.',
        stale: 'Stale — the approved tree changed since this was proposed.',
      }[status] || humanize(status);
      actions.append(h('span', { class: 'card-note muted' }, note));
    }
    card.append(actions);
    return card;
  }

  function reportCard(c) {
    const status = String(c.status || '');
    const card = cardShell('report', 'Report', status, null);
    card.append(h('div', { class: 'card-title' }, 'Task ', taskChip(c.task_id, () => openTaskDetail(c.task_id)), ' ', humanize(status.toLowerCase())));
    const body = h('div', { class: 'card-body' });
    const lines = Array.isArray(c.lines) ? c.lines : [];
    if (lines.length) body.append(h('ul', { class: 'report-lines' }, lines.map((l) => h('li', null, fmtText(l)))));
    const lim = Array.isArray(c.limitations) ? c.limitations : [];
    if (lim.length) body.append(h('div', null, h('div', { class: 'section-label' }, 'Limitations'), h('ul', { class: 'limitations' }, lim.map((l) => h('li', null, fmtText(l))))));
    if (c.approved_root) body.append(h('div', { class: 'card-row' }, h('span', { class: 'k' }, 'Approved root'), h('span', { class: 'v' }, digestButton(c.approved_root))));
    card.append(body);
    return card;
  }

  async function loadChat() {
    const data = await api('/api/chat');
    state.chatLoaded = true;
    for (const m of Array.isArray(data.messages) ? data.messages : []) upsertMessage(m, { batch: true });
    afterChatChange(false);
  }

  // ------------------------------------------------------------- composer

  function autosize() {
    const ta = $('composer-input');
    ta.style.height = 'auto';
    ta.style.height = Math.min(ta.scrollHeight, 200) + 'px';
  }

  async function sendMessage() {
    const ta = $('composer-input');
    const text = ta.value.trim();
    if (!text || state.sending || state.expired) return;
    state.sending = true;
    ta.disabled = true;
    $('send-btn').disabled = true;
    $('send-btn').textContent = 'Sending…';
    try {
      const res = await api('/api/chat', { method: 'POST', body: { text } });
      ta.value = '';
      autosize();
      if (res && typeof res.message_id === 'string' && !state.messages.has(res.message_id)) {
        upsertMessage({ id: res.message_id, role: 'user', ts: Date.now(), text, task_id: null, card: null });
      }
      state.pendingReply = Date.now();
      afterChatChange(true);
    } catch (err) {
      reportError(err);
    } finally {
      state.sending = false;
      if (!state.expired) {
        ta.disabled = false;
        $('send-btn').disabled = false;
      }
      $('send-btn').textContent = 'Send';
      ta.focus();
    }
  }

  // ------------------------------------------------------------- activity

  function upsertItem(item, opts = {}) {
    if (!item || typeof item.id !== 'string') return;
    const sc = $('activity-scroll');
    const wasBottom = state.actStick;
    const exists = state.items.has(item.id);
    state.items.set(item.id, item);
    const node = renderItem(item, !exists && !opts.batch);
    if (exists && state.itemNodes.has(item.id)) {
      state.itemNodes.get(item.id).replaceWith(node);
    } else {
      state.itemOrder.push(item.id);
      $('timeline').appendChild(node);
      while (state.itemOrder.length > ACTIVITY_CAP) {
        const old = state.itemOrder.shift();
        state.itemNodes.get(old)?.remove();
        state.itemNodes.delete(old);
        state.items.delete(old);
        state.expandedItems.delete(old);
      }
    }
    state.itemNodes.set(item.id, node);
    if (!opts.batch) {
      afterActivityChange();
      if (wasBottom) sc.scrollTop = sc.scrollHeight;
      else if (!exists && passesFilter(item)) $('jump-latest').hidden = false;
    }
  }

  function afterActivityChange() {
    renderFilters();
    renderNow();
    $('activity-empty').hidden = state.items.size > 0;
    if (state.items.size > 0) {
      const anyVisible = state.itemOrder.some((id) => passesFilter(state.items.get(id)));
      $('activity-empty').hidden = anyVisible;
      if (!anyVisible) $('activity-empty').textContent = 'No activity matches this filter.';
    } else {
      $('activity-empty').textContent = 'No activity yet. When INTELLECTUS plans, writes code, runs tests or asks for approval, each step shows up here live.';
    }
  }

  function passesFilter(item) {
    if (!item) return false;
    const f = state.actFilter;
    if (f.kind && normKind(item.kind) !== f.kind) return false;
    if (f.task && item.task_id !== f.task) return false;
    return true;
  }

  function normKind(k) { return KINDS.includes(k) ? k : 'system'; }
  function normStatus(s) { return ['running', 'ok', 'fail', 'warn', 'info'].includes(s) ? s : 'info'; }

  function renderItem(item, fresh) {
    const kind = normKind(item.kind);
    const status = normStatus(item.status);
    const hasData = item.data && typeof item.data === 'object' && Object.keys(item.data).length > 0;
    const hasDetail = (typeof item.detail === 'string' && item.detail.trim() !== '') || hasData;
    const li = h('li', { class: 'act' + (fresh ? ' fresh' : '') + (passesFilter(item) ? '' : ' hidden-by-filter'), dataset: { kind, status, id: item.id } });
    li.append(h('span', { class: 'kicon', dataset: { kind }, title: KIND_LABEL[kind], 'aria-hidden': 'true' }, KIND_GLYPH[kind]));
    li.append(h('div', { class: 'act-title' }, h('span', { class: 'sr-only' }, KIND_LABEL[kind] + ': '), String(item.title ?? '')));
    li.append(h('time', { class: 'act-time', title: fmtDateTime(item.ts) }, fmtTime(item.ts)));
    const meta = h('div', { class: 'act-meta' }, badge(STATUS_LABEL[status], status === 'running' ? 'run' : status));
    if (item.task_id) {
      meta.append(h('button', {
        type: 'button', class: 'chip', title: 'Show only ' + item.task_id,
        onclick: () => setActFilter({ task: state.actFilter.task === item.task_id ? null : item.task_id }),
      }, String(item.task_id)));
    }
    let detailEl = null;
    if (hasDetail) {
      const expanded = state.expandedItems.has(item.id);
      const detailId = 'act-detail-' + item.id.replace(/[^A-Za-z0-9_-]/g, '_');
      let text = typeof item.detail === 'string' ? item.detail : '';
      if (hasData) {
        const lines = Object.entries(item.data).map(([k, v]) => k + ': ' + (typeof v === 'string' ? v : fmtValue(v)));
        text = text ? text.replace(/\s+$/, '') + '\n\n' + lines.join('\n') : lines.join('\n');
      }
      detailEl = h('pre', { class: 'act-detail', id: detailId, hidden: !expanded }, text);
      const toggle = h('button', { type: 'button', class: 'act-toggle', 'aria-expanded': String(expanded), 'aria-controls': detailId }, 'Details');
      toggle.addEventListener('click', () => {
        const open = !state.expandedItems.has(item.id);
        if (open) state.expandedItems.add(item.id); else state.expandedItems.delete(item.id);
        toggle.setAttribute('aria-expanded', String(open));
        detailEl.hidden = !open;
      });
      meta.append(toggle);
    }
    li.append(meta);
    if (detailEl) li.append(detailEl);
    return li;
  }

  function setActFilter(patch) {
    Object.assign(state.actFilter, patch);
    for (const [id, node] of state.itemNodes) node.classList.toggle('hidden-by-filter', !passesFilter(state.items.get(id)));
    afterActivityChange();
    const sc = $('activity-scroll');
    sc.scrollTop = sc.scrollHeight;
    state.actStick = true;
    $('jump-latest').hidden = true;
  }

  function renderFilters() {
    const bar = clear($('activity-filters'));
    const counts = {};
    for (const it of state.items.values()) {
      if (state.actFilter.task && it.task_id !== state.actFilter.task) continue;
      const k = normKind(it.kind);
      counts[k] = (counts[k] || 0) + 1;
    }
    const total = Object.values(counts).reduce((a, b) => a + b, 0);
    bar.append(h('button', { type: 'button', class: 'fchip', 'aria-pressed': String(!state.actFilter.kind), onclick: () => setActFilter({ kind: null }) },
      'All', h('span', { class: 'n' }, String(total))));
    for (const k of KINDS) {
      if (!counts[k] && state.actFilter.kind !== k) continue;
      bar.append(h('button', {
        type: 'button', class: 'fchip', dataset: { kind: k }, 'aria-pressed': String(state.actFilter.kind === k),
        onclick: () => setActFilter({ kind: state.actFilter.kind === k ? null : k }),
      }, h('span', { class: 'kdot', 'aria-hidden': 'true' }), KIND_LABEL[k], h('span', { class: 'n' }, String(counts[k] || 0))));
    }
    if (state.actFilter.task) {
      const t = state.actFilter.task;
      bar.append(h('span', { class: 'filter-sep', 'aria-hidden': 'true' }),
        h('button', { type: 'button', class: 'fchip task', 'aria-pressed': 'true', title: 'Clear task filter', onclick: () => setActFilter({ task: null }) }, t, ' ×'),
        h('button', { type: 'button', class: 'link-btn small', onclick: () => openTaskDetail(t) }, 'Open task'));
    }
  }

  function renderNow() {
    const strip = clear($('now-strip'));
    const running = state.itemOrder.map((id) => state.items.get(id)).filter((it) => it && it.status === 'running');
    strip.append(h('span', { class: 'now-label' }, 'Now'));
    if (!running.length) {
      const busy = state.status && state.status.busy;
      strip.append(h('span', { class: 'now-idle' }, busy ? 'Working — waiting for the next step…' : 'Idle — nothing is running.'));
      return;
    }
    const latest = running.slice(-3).reverse();
    strip.append(h('ul', { class: 'now-list', 'aria-label': 'Running now' }, latest.map((it) => h('li', { class: 'now-item', dataset: { kind: normKind(it.kind) } },
      h('span', { class: 'spinner', 'aria-hidden': 'true' }),
      h('span', { class: 't', title: String(it.title ?? '') }, String(it.title ?? '')),
      h('span', { class: 'el', dataset: { since: toMs(it.ts) || Date.now() } }, fmtElapsed(Date.now() - (toMs(it.ts) || Date.now())))))));
    if (running.length > 3) strip.lastChild.append(h('li', { class: 'muted small' }, '+' + (running.length - 3) + ' more running'));
  }

  function tickElapsed() {
    for (const el of document.querySelectorAll('#now-strip .el[data-since]')) {
      el.textContent = fmtElapsed(Date.now() - Number(el.dataset.since));
    }
  }

  async function loadActivity() {
    const data = await api('/api/activity?limit=200');
    const items = Array.isArray(data.items) ? data.items : [];
    for (const it of items) upsertItem(it, { batch: true });
    afterActivityChange();
    if (state.actStick) { const sc = $('activity-scroll'); sc.scrollTop = sc.scrollHeight; }
  }

  // ---------------------------------------------------------------- tasks

  let tasksTimer = null;
  function scheduleTasksRefresh(changed) {
    clearTimeout(tasksTimer);
    tasksTimer = setTimeout(() => {
      loadTasks().catch(reportError);
      if (state.openTask && (!changed || changed === state.openTask)) loadTaskDetail(state.openTask).catch(reportError);
    }, 120);
  }

  async function loadTasks() {
    const data = await api('/api/tasks');
    state.tasks = Array.isArray(data.tasks) ? data.tasks : [];
    renderTasks();
  }

  function repairsMeter(used, max) {
    const u = Number(used) || 0;
    const m = Number(max) || 0;
    if (m <= 0 || m > 12) return null;
    const meter = h('span', { class: 'meter', 'aria-hidden': 'true' });
    for (let i = 0; i < m; i++) meter.append(h('i', { class: i < u ? 'on' : null }));
    return meter;
  }

  function phaseBadge(phase) {
    if (!phase) return null;
    const p = String(phase);
    const tone = p === 'awaiting_approval' ? 'warn' : p === 'done' ? 'neutral' : 'run';
    return badge(p, tone, 'plain');
  }

  function renderTasks() {
    const openCount = state.tasks.filter((t) => String(t.status).toUpperCase() === 'OPEN').length;
    const tab = $('tab-tasks');
    tab.replaceChildren('Tasks');
    if (openCount) tab.append(h('span', { class: 'tab-count', 'aria-label': openCount + ' open' }, String(openCount)));

    const list = clear($('tasks-list'));
    list.hidden = !!state.openTask;
    $('task-detail').hidden = !state.openTask;
    if (state.openTask) { renderTaskDetail(); return; }
    if (!state.tasks.length) {
      list.append(h('p', { class: 'empty' }, 'No tasks yet. Approve a proposal in the chat to start one.'));
      return;
    }
    const sorted = [...state.tasks].sort((a, b) => (Number(b.updated_ts) || 0) - (Number(a.updated_ts) || 0));
    list.append(h('ul', { class: 'task-list', 'aria-label': 'Tasks' }, sorted.map((t) => h('li', null,
      h('button', { type: 'button', class: 'task-row', onclick: () => openTaskDetail(t.task_id) },
        h('div', null, h('div', { class: 't-title' }, String(t.title || t.task_id)), h('div', { class: 't-id' }, String(t.task_id))),
        h('div', { class: 't-badges' }, badge(t.status || 'UNKNOWN'), phaseBadge(t.phase)),
        h('div', { class: 't-meta' },
          h('span', null, 'Repairs ', h('b', null, (t.repairs_used ?? 0) + '/' + (t.max_repairs ?? '—')), repairsMeter(t.repairs_used, t.max_repairs)),
          h('span', null, 'Candidates ', h('b', null, String(t.candidates ?? 0)), Number(t.failed_candidates) ? ' (' + t.failed_candidates + ' failed)' : ''),
          h('span', null, 'Updated ', h('b', null, fmtTime(t.updated_ts)))))))));
  }

  async function openTaskDetail(taskId) {
    if (!taskId) return;
    state.openTask = String(taskId);
    state.taskDetail = null;
    state.taskDetailError = null;
    state.expandedCell = null;
    state.confirmCancel = false;
    showWorkTab('tasks');
    renderTasks();
    try { await loadTaskDetail(state.openTask); } catch (err) { reportError(err); }
  }

  function closeTaskDetail() {
    state.openTask = null;
    state.taskDetail = null;
    renderTasks();
    $('tasks-scroll').scrollTop = 0;
  }

  async function loadTaskDetail(taskId) {
    try {
      const d = await api('/api/tasks/' + enc(taskId));
      if (state.openTask !== taskId) return;
      state.taskDetail = d;
      state.taskDetailError = null;
      for (const c of Array.isArray(d.candidates) ? d.candidates : []) {
        if (c.candidate_root) addKnownRoot(c.candidate_root, 'candidate ' + (c.proposal_id || '') + ' · ' + taskId);
      }
      if (d.report && d.report.approved_root) addKnownRoot(d.report.approved_root, 'approved root after ' + taskId);
    } catch (err) {
      if (state.openTask !== taskId) return;
      state.taskDetailError = err.message;
      throw err;
    } finally {
      if (state.openTask === taskId) renderTaskDetail();
    }
  }

  function dsection(title, ...content) {
    return h('section', { class: 'dsection' }, h('h3', null, title), content);
  }

  function renderTaskDetail() {
    const box = clear($('task-detail'));
    const back = h('button', { type: 'button', class: 'btn btn-ghost btn-sm', onclick: closeTaskDetail }, '← All tasks');
    const d = state.taskDetail;
    const summary = d && d.task ? d.task : state.tasks.find((t) => t.task_id === state.openTask) || { task_id: state.openTask };
    const top = h('div', { class: 'detail-top' }, back, h('span', { class: 'grow' }));
    if (String(summary.status).toUpperCase() === 'OPEN') top.append(cancelControls(summary.task_id));
    box.append(top);
    box.append(h('h3', { class: 'detail-title' }, String(summary.title || summary.task_id)));
    box.append(h('div', { class: 'detail-sub' },
      h('span', { class: 'chip' }, String(summary.task_id)),
      summary.status ? badge(summary.status) : null,
      phaseBadge(summary.phase),
      summary.max_repairs !== undefined ? h('span', null, 'Repairs ', h('b', null, (summary.repairs_used ?? 0) + '/' + summary.max_repairs)) : null,
      summary.candidates !== undefined ? h('span', null, 'Candidates ', h('b', null, String(summary.candidates)), Number(summary.failed_candidates) ? ' (' + summary.failed_candidates + ' failed)' : '') : null,
      summary.updated_ts ? h('span', null, 'Updated ', h('b', null, fmtTime(summary.updated_ts))) : null));

    if (!d) {
      box.append(h('p', { class: 'empty' }, state.taskDetailError ? 'Could not load task: ' + state.taskDetailError : 'Loading task…'));
      return;
    }
    if (d.requirement) box.append(dsection('Requirement', h('div', { class: 'box prose' }, fmtText(d.requirement))));
    if (d.entrypoint) {
      box.append(dsection('Entrypoint', h('code', { class: 'mono' }, entrypointText(d.entrypoint)),
        d.entrypoint.language ? h('span', { class: 'muted' }, ' · ' + d.entrypoint.language) : null));
    }
    box.append(dsection('Acceptance tests', testMatrix(d)));
    box.append(dsection('Jev assessments', assessmentsTable(d.assessments)));
    box.append(dsection('Actions', actionsTable(d.actions)));
    if (d.report) box.append(dsection('Report', reportView(d.report)));
  }

  function cancelControls(taskId) {
    const wrap = h('span', { class: 'cancel-controls' });
    if (!state.confirmCancel) {
      wrap.append(h('button', { type: 'button', class: 'btn btn-danger btn-sm', onclick: () => { state.confirmCancel = true; renderTaskDetail(); } }, 'Cancel task'));
      return wrap;
    }
    const yes = h('button', { type: 'button', class: 'btn btn-danger btn-sm' }, 'Confirm cancel');
    const no = h('button', { type: 'button', class: 'btn btn-secondary btn-sm', onclick: () => { state.confirmCancel = false; renderTaskDetail(); } }, 'Keep running');
    yes.addEventListener('click', async () => {
      yes.disabled = true; no.disabled = true; yes.textContent = 'Cancelling…';
      try {
        await api('/api/tasks/' + enc(taskId) + '/cancel', { method: 'POST' });
        toast('Cancellation requested for ' + taskId + '.');
        state.confirmCancel = false;
        scheduleTasksRefresh(taskId);
      } catch (err) {
        reportError(err);
        state.confirmCancel = false;
        renderTaskDetail();
      }
    });
    wrap.append(h('span', { class: 'muted small' }, 'Stop this task? '), ' ', yes, ' ', no);
    return wrap;
  }

  function testMatrix(d) {
    const cands = Array.isArray(d.candidates) ? d.candidates : [];
    let cases = Array.isArray(d.cases) ? d.cases.map((c) => ({ name: String(c.name ?? ''), input: c.input, expect: c.expect })) : [];
    if (!cases.length) {
      const names = new Map();
      for (const c of cands) for (const det of Array.isArray(c.details) ? c.details : []) if (!names.has(det.name)) names.set(det.name, { name: String(det.name), input: det.input, expect: det.expected });
      cases = [...names.values()];
    }
    if (!cases.length) return h('p', { class: 'muted small' }, 'No acceptance tests recorded.');
    const detailOf = (cand, name) => (Array.isArray(cand.details) ? cand.details.find((x) => x.name === name) : null);
    const head = h('tr', null, h('th', { scope: 'col' }, 'Case'), h('th', { scope: 'col' }, 'Input'), h('th', { scope: 'col' }, 'Expected'),
      cands.map((c, i) => h('th', { scope: 'col', class: 'cand', title: c.candidate_root ? String(c.candidate_root) : null },
        String(c.proposal_id || 'candidate ' + (i + 1)), ' ', c.acceptance ? badge(c.acceptance) : null,
        c.candidate_root ? h('span', { class: 'cand-root' }, shortDigest(c.candidate_root)) : null)));
    const tbody = h('tbody');
    for (const cs of cases) {
      const ex = fmtExpect(cs.expect);
      const row = h('tr', null,
        h('td', { class: 'case-name' }, cs.name),
        h('td', { class: 'mono' }, fmtValue(cs.input)),
        h('td', { class: 'mono' }, ex.text));
      cands.forEach((cand, ci) => {
        const det = detailOf(cand, cs.name);
        if (!det) { row.append(h('td', { class: 'cell muted' }, '—')); return; }
        const key = ci + '|' + cs.name;
        const open = state.expandedCell === key;
        row.append(h('td', { class: 'cell' }, h('button', {
          type: 'button', class: 'cell-btn', 'aria-expanded': String(open),
          'aria-label': cs.name + ' on ' + (cand.proposal_id || 'candidate ' + (ci + 1)) + ': ' + det.status + '. Show details',
          onclick: () => { state.expandedCell = open ? null : key; renderTaskDetail(); },
        }, badge(det.status || 'UNKNOWN'))));
      });
      tbody.append(row);
      if (state.expandedCell && state.expandedCell.endsWith('|' + cs.name)) {
        const ci = Number(state.expandedCell.split('|')[0]);
        const cand = cands[ci];
        const det = cand && detailOf(cand, cs.name);
        if (det) tbody.append(caseDetailRow(det, cand, ci, 3 + cands.length));
      }
    }
    return h('div', { class: 'matrix-wrap' }, h('table', { class: 'matrix' }, h('thead', null, head), tbody));
  }

  function caseDetailRow(det, cand, ci, span) {
    const pre = (label, v) => h('div', null, h('div', { class: 'section-label' }, label),
      h('pre', null, v === undefined || v === null || v === '' ? '(empty)' : fmtText(v)));
    return h('tr', { class: 'case-detail' }, h('td', { colspan: String(span) },
      h('div', { class: 'cd-head' }, badge(det.status || 'UNKNOWN'), h('b', { class: 'mono' }, String(det.name)),
        h('span', { class: 'muted' }, 'on ' + (cand.proposal_id || 'candidate ' + (ci + 1))),
        det.duration_ms !== undefined ? h('span', { class: 'muted' }, '· ' + det.duration_ms + ' ms') : null),
      h('div', { class: 'cd-grid' },
        pre('Input', det.input === undefined ? undefined : fmtValue(det.input)),
        pre('Expected', det.expected), pre('Observed', det.observed),
        pre('stdout', det.stdout), pre('stderr', det.stderr))));
  }

  function simpleTable(headers, rows) {
    return h('div', { class: 'table-wrap' }, h('table', { class: 'data' },
      h('thead', null, h('tr', null, headers.map((x) => h('th', { scope: 'col' }, x)))),
      h('tbody', null, rows)));
  }

  function assessmentsTable(list) {
    const rows = (Array.isArray(list) ? list : []).map((a) => {
      const bp = Number(a.confidence_bp);
      return h('tr', null,
        h('td', { class: 'mono' }, a.choice === null || a.choice === undefined ? '—' : String(a.choice)),
        h('td', null, Number.isFinite(bp) && a.confidence_bp !== null ? (bp / 100).toFixed(bp % 100 ? 1 : 0) + '%' : '—'),
        h('td', { class: 'mono' }, a.applied_choice ? String(a.applied_choice) : '—'),
        h('td', null, a.used_advisor ? 'yes' : 'no'),
        h('td', { class: 'mono' }, a.fallback_reason ? String(a.fallback_reason) : '—'),
        h('td', null, a.mode ? badge(a.mode, 'info', 'plain') : '—'));
    });
    if (!rows.length) return h('p', { class: 'muted small' }, 'No Jev assessments recorded for this task.');
    return simpleTable(['Choice', 'Confidence', 'Applied choice', 'Used advisor', 'Fallback reason', 'Mode'], rows);
  }

  function fmtPostconditions(p) {
    if (p === undefined || p === null) return '—';
    if (Array.isArray(p)) return p.map((x) => (typeof x === 'string' ? x : x && typeof x === 'object' ? [x.kind || x.name || x.id, x.result || x.status].filter(Boolean).join(': ') || fmtValue(x) : fmtValue(x))).join('\n') || '—';
    if (typeof p === 'object') return Object.entries(p).map(([k, v]) => k + ': ' + (typeof v === 'string' ? v : fmtValue(v))).join('\n');
    return String(p);
  }

  function actionsTable(list) {
    const rows = (Array.isArray(list) ? list : []).map((a) => h('tr', null,
      h('td', { class: 'mono' }, String(a.action_id ?? '')),
      h('td', { class: 'mono' }, String(a.tool_id ?? '')),
      h('td', null, a.state ? badge(a.state) : '—'),
      h('td', { class: 'mono' }, h('span', { class: 'rich' }, fmtPostconditions(a.postconditions)))));
    if (!rows.length) return h('p', { class: 'muted small' }, 'No actions yet.');
    return simpleTable(['Action', 'Tool', 'State', 'Postconditions'], rows);
  }

  function reportView(r) {
    const wrap = h('div', { class: 'box' });
    const t = r.task && typeof r.task === 'object' ? r.task : {};
    wrap.append(kv([
      ['Status', t.status ? badge(t.status) : r.status ? badge(r.status) : ''],
      ['Completed by', t.completed_by],
      ['Approved root', r.approved_root ? digestButton(r.approved_root) : ''],
      ['Snapshot', r.snapshot_sequence !== undefined ? '#' + r.snapshot_sequence : ''],
      ['State digest', r.state_digest ? digestButton(r.state_digest) : ''],
    ]));
    const ver = Array.isArray(r.verifications) ? r.verifications : [];
    if (ver.length) {
      wrap.append(h('div', { class: 'pop-section' }, h('div', { class: 'section-label' }, 'Verifications'),
        simpleTable(['Check', 'Result', 'Issuer', 'Limits'], ver.map((v) => h('tr', null,
          h('td', { class: 'mono' }, String(v.check_kind ?? '')),
          h('td', null, v.result ? badge(v.result) : '—'),
          h('td', { class: 'mono' }, String(v.issuer ?? '')),
          h('td', { class: 'mono' }, Array.isArray(v.applicability_limits) && v.applicability_limits.length ? v.applicability_limits.join(', ') : '—'))))));
    }
    const lim = Array.isArray(r.limitations) ? r.limitations : [];
    if (lim.length) {
      wrap.append(h('div', { class: 'pop-section' }, h('div', { class: 'section-label' }, 'Limitations'),
        h('ul', { class: 'limitations' }, lim.map((l) => h('li', null, fmtText(l))))));
    }
    let json = '';
    try { json = JSON.stringify(r, null, 2); } catch (_) { json = String(r); }
    wrap.append(h('details', { class: 'raw' }, h('summary', null, 'Raw task report (JSON)'), h('pre', null, json)));
    return wrap;
  }

  // ---------------------------------------------------------------- files

  function addKnownRoot(digest, label) {
    if (!digest) return;
    const d = String(digest);
    if (!state.knownRoots.has(d)) state.knownRoots.set(d, new Set());
    state.knownRoots.get(d).add(label);
    if (state.files.loaded) renderRootSelects();
  }

  function collectRootsFromCard(card) {
    if (!card || typeof card !== 'object') return;
    if (card.type === 'approval') {
      if (card.from_root) addKnownRoot(card.from_root, 'base of ' + (card.approval_id || 'approval'));
      if (card.to_root) addKnownRoot(card.to_root, 'target of ' + (card.approval_id || 'approval'));
    } else if (card.type === 'report' && card.approved_root) {
      addKnownRoot(card.approved_root, 'approved root after ' + (card.task_id || 'task'));
    }
  }

  function rootLabel(d) {
    const labels = state.knownRoots.get(d);
    const approved = state.status && state.status.approved_root === d;
    const parts = [];
    if (approved) parts.push('approved');
    if (labels) for (const l of labels) if (l !== 'approved root') parts.push(l);
    return shortDigest(d) + (parts.length ? ' — ' + [...new Set(parts)].slice(0, 2).join(', ') : '');
  }

  function fillSelect(sel, options, value) {
    const prev = value !== undefined ? value : sel.value;
    clear(sel);
    for (const [v, label] of options) sel.append(h('option', { value: v }, label));
    if (options.some(([v]) => v === prev)) sel.value = prev;
  }

  let rootsSig = '';
  function renderRootSelects() {
    const approved = state.status && state.status.approved_root;
    const sig = [approved, state.files.root, state.files.from, state.files.to, ...[...state.knownRoots].map(([d, l]) => d + ':' + [...l].join(','))].join('|');
    if (sig === rootsSig) return;
    rootsSig = sig;
    const roots = [...state.knownRoots.keys()];
    const treeOpts = [['', 'Approved root' + (approved ? ' (' + shortDigest(approved) + ')' : '')]];
    for (const d of roots) if (d !== approved) treeOpts.push([d, rootLabel(d)]);
    fillSelect($('root-select'), treeOpts, state.files.root);
    const all = roots.map((d) => [d, rootLabel(d)]);
    const ensure = (v) => { if (v && !all.some(([d]) => d === v)) all.push([v, shortDigest(v)]); };
    ensure(state.files.from); ensure(state.files.to);
    fillSelect($('diff-from'), all, state.files.from);
    fillSelect($('diff-to'), all, state.files.to);
  }

  function setFilesMode(mode) {
    state.files.mode = mode;
    for (const b of document.querySelectorAll('[data-files-mode]')) b.setAttribute('aria-pressed', String(b.dataset.filesMode === mode));
    $('root-field').hidden = mode !== 'tree';
    $('export-btn').hidden = mode !== 'tree';
    $('diff-selects').hidden = mode !== 'diff';
    if (mode === 'diff') {
      if (!state.files.from || !state.files.to) {
        const approved = state.status && state.status.approved_root;
        const roots = [...state.knownRoots.keys()];
        state.files.from = state.files.from || approved || roots[0] || '';
        state.files.to = state.files.to || roots.filter((d) => d !== state.files.from).slice(-1)[0] || state.files.from;
      }
      renderRootSelects();
      loadDiff();
    } else {
      renderRootSelects();
      loadTree();
    }
  }

  async function ensureFiles() {
    if (state.files.loaded) return;
    state.files.loaded = true;
    renderRootSelects();
    setFilesMode(state.files.mode);
  }

  async function loadTree() {
    const body = $('files-body');
    state.files.loading = true;
    if (!state.files.tree) appendAll(clear(body), [h('p', { class: 'empty' }, 'Loading files…')]);
    try {
      const q = state.files.root ? '?root=' + enc(state.files.root) : '';
      const data = await api('/api/tree' + q);
      state.files.tree = data;
      state.files.error = null;
      if (data.root) addKnownRoot(data.root, state.files.root ? 'viewed root' : 'approved root');
    } catch (err) {
      state.files.error = err.message;
      state.files.tree = null;
      if (err.status !== 401) toast(err.message, 'fail');
    } finally {
      state.files.loading = false;
      if (state.files.mode === 'tree') renderTree();
    }
  }

  function buildTree(paths) {
    const root = { dirs: new Map(), files: [] };
    for (const p of paths) {
      const parts = p.split('/');
      let node = root;
      for (let i = 0; i < parts.length - 1; i++) {
        if (!node.dirs.has(parts[i])) node.dirs.set(parts[i], { dirs: new Map(), files: [] });
        node = node.dirs.get(parts[i]);
      }
      node.files.push({ name: parts[parts.length - 1], path: p });
    }
    return root;
  }

  function treeList(node) {
    const ul = h('ul');
    for (const name of [...node.dirs.keys()].sort()) {
      ul.append(h('li', null, h('div', { class: 'dir' }, name + '/'), treeList(node.dirs.get(name))));
    }
    for (const f of node.files.sort((a, b) => a.name.localeCompare(b.name))) {
      ul.append(h('li', null, h('button', {
        type: 'button', title: f.path, 'aria-current': String(state.files.selected === f.path),
        onclick: () => { state.files.selected = f.path; renderTree(); },
      }, f.name)));
    }
    return ul;
  }

  function splitLines(text) {
    const s = String(text ?? '');
    if (s === '') return [];
    const lines = s.split('\n');
    if (lines[lines.length - 1] === '') lines.pop();
    return lines;
  }

  function codeView(text) {
    const lines = splitLines(text);
    const nums = lines.map((_, i) => String(i + 1)).join('\n');
    return h('div', { class: 'code-view' },
      h('pre', { class: 'gutter', 'aria-hidden': 'true' }, nums || '1'),
      h('pre', { class: 'code' }, h('code', null, lines.join('\n'))));
  }

  function renderTree() {
    const body = clear($('files-body'));
    const t = state.files.tree;
    if (!t) {
      body.append(h('p', { class: 'empty' }, state.files.error ? 'Could not load files: ' + state.files.error : 'Loading files…'));
      return;
    }
    const files = t.files && typeof t.files === 'object' ? t.files : {};
    const paths = Object.keys(files).sort();
    const head = h('div', { class: 'diff-head' }, 'Root ', digestButton(t.root),
      state.status && t.root === state.status.approved_root ? badge('approved', 'ok') : null,
      h('span', null, '· ' + paths.length + ' file' + (paths.length === 1 ? '' : 's')));
    if (!paths.length) { body.append(head, h('p', { class: 'empty' }, 'This tree is empty.')); return; }
    if (!state.files.selected || !(state.files.selected in files)) {
      state.files.selected = paths.find((p) => /(^|\/)README(\.md)?$/i.test(p)) || paths[0];
    }
    const sel = state.files.selected;
    const content = files[sel];
    const lines = splitLines(content);
    const viewer = h('div', { class: 'viewer' },
      h('div', { class: 'viewer-head' }, h('span', { class: 'path' }, sel),
        h('span', null, lines.length + ' line' + (lines.length === 1 ? '' : 's') + ' · ' + fmtInt(new Blob([String(content ?? '')]).size) + ' bytes')),
      codeView(content));
    body.append(head, h('div', { class: 'files-layout' }, h('nav', { class: 'ftree', 'aria-label': 'Files' }, treeList(buildTree(paths))), viewer));
  }

  // ---- diff

  function openDiff(from, to) {
    state.files.from = String(from);
    state.files.to = String(to);
    addKnownRoot(from, 'diff base');
    addKnownRoot(to, 'diff target');
    state.files.loaded = true;
    showWorkTab('files');
    setFilesMode('diff');
  }

  async function loadDiff() {
    const body = clear($('files-body'));
    const { from, to } = state.files;
    if (!from || !to) {
      body.append(h('p', { class: 'empty' }, 'Pick two roots to compare. Roots appear here as tasks produce candidates and approvals.'));
      return;
    }
    body.append(h('p', { class: 'empty' }, 'Loading diff…'));
    try {
      const data = await api('/api/diff?from=' + enc(from) + '&to=' + enc(to));
      if (state.files.from !== from || state.files.to !== to || state.files.mode !== 'diff') return;
      state.files.diff = data;
      renderDiff(data);
    } catch (err) {
      clear(body).append(h('p', { class: 'empty' }, 'Could not load diff: ' + err.message));
    }
  }

  const DIFF_MAX_LINES = 4000;

  /** Myers O(ND) line diff with common prefix/suffix trimming. Returns ops [{t, a, b}]. */
  function diffLines(a, b) {
    let pre = 0;
    while (pre < a.length && pre < b.length && a[pre] === b[pre]) pre++;
    let suf = 0;
    while (suf < a.length - pre && suf < b.length - pre && a[a.length - 1 - suf] === b[b.length - 1 - suf]) suf++;
    const A = a.slice(pre, a.length - suf);
    const B = b.slice(pre, b.length - suf);
    let mid = myers(A, B);
    if (!mid) mid = [...A.map((_, i) => ({ t: 'del', a: i, b: null })), ...B.map((_, j) => ({ t: 'add', a: null, b: j }))];
    const ops = [];
    for (let i = 0; i < pre; i++) ops.push({ t: 'ctx', a: i, b: i });
    for (const op of mid) ops.push({ t: op.t, a: op.a === null ? null : op.a + pre, b: op.b === null ? null : op.b + pre });
    for (let i = 0; i < suf; i++) ops.push({ t: 'ctx', a: a.length - suf + i, b: b.length - suf + i });
    return ops;
  }

  function myers(A, B) {
    const N = A.length;
    const M = B.length;
    if (N === 0) return B.map((_, j) => ({ t: 'add', a: null, b: j }));
    if (M === 0) return A.map((_, i) => ({ t: 'del', a: i, b: null }));
    const MAX = N + M;
    const LIMIT = Math.min(MAX, 2000);
    const off = MAX + 1;
    const V = new Int32Array(2 * MAX + 3);
    const trace = [];
    let found = -1;
    for (let d = 0; d <= LIMIT && found < 0; d++) {
      trace.push(V.slice(off - d - 1, off + d + 2));
      for (let k = -d; k <= d; k += 2) {
        let x = (k === -d || (k !== d && V[off + k - 1] < V[off + k + 1])) ? V[off + k + 1] : V[off + k - 1] + 1;
        let y = x - k;
        while (x < N && y < M && A[x] === B[y]) { x++; y++; }
        V[off + k] = x;
        if (x >= N && y >= M) { found = d; break; }
      }
    }
    if (found < 0) return null;
    const ops = [];
    let x = N;
    let y = M;
    for (let d = found; d > 0; d--) {
      const v = trace[d];
      const get = (k) => v[k + d + 1];
      const k = x - y;
      const prevK = (k === -d || (k !== d && get(k - 1) < get(k + 1))) ? k + 1 : k - 1;
      const prevX = get(prevK);
      const prevY = prevX - prevK;
      while (x > prevX && y > prevY) { ops.push({ t: 'ctx', a: x - 1, b: y - 1 }); x--; y--; }
      if (x === prevX) { ops.push({ t: 'add', a: null, b: y - 1 }); y--; } else { ops.push({ t: 'del', a: x - 1, b: null }); x--; }
    }
    while (x > 0 && y > 0) { ops.push({ t: 'ctx', a: x - 1, b: y - 1 }); x--; y--; }
    return ops.reverse();
  }

  function hunksOf(ops, ctx = 3) {
    const changes = [];
    ops.forEach((o, i) => { if (o.t !== 'ctx') changes.push(i); });
    if (!changes.length) return [];
    const ranges = [];
    let s = Math.max(0, changes[0] - ctx);
    let e = Math.min(ops.length, changes[0] + ctx + 1);
    for (let j = 1; j < changes.length; j++) {
      const c = changes[j];
      if (c - ctx <= e) e = Math.min(ops.length, c + ctx + 1);
      else { ranges.push([s, e]); s = Math.max(0, c - ctx); e = Math.min(ops.length, c + ctx + 1); }
    }
    ranges.push([s, e]);
    // positions before each op
    const aBefore = new Int32Array(ops.length + 1);
    const bBefore = new Int32Array(ops.length + 1);
    for (let i = 0; i < ops.length; i++) {
      aBefore[i + 1] = aBefore[i] + (ops[i].a !== null ? 1 : 0);
      bBefore[i + 1] = bBefore[i] + (ops[i].b !== null ? 1 : 0);
    }
    return ranges.map(([rs, re]) => {
      const oldCount = aBefore[re] - aBefore[rs];
      const newCount = bBefore[re] - bBefore[rs];
      return {
        header: '@@ -' + (oldCount ? aBefore[rs] + 1 : aBefore[rs]) + ',' + oldCount + ' +' + (newCount ? bBefore[rs] + 1 : bBefore[rs]) + ',' + newCount + ' @@',
        ops: ops.slice(rs, re),
      };
    });
  }

  function diffTable(oldLines, newLines, ops) {
    const tbody = h('tbody');
    for (const hk of hunksOf(ops)) {
      tbody.append(h('tr', { class: 'hunk' }, h('td', { class: 'ln' }), h('td', { class: 'ln' }), h('td', { class: 'code', colspan: '2' }, hk.header)));
      for (const op of hk.ops) {
        const text = op.t === 'add' ? newLines[op.b] : oldLines[op.a];
        tbody.append(h('tr', { class: op.t },
          h('td', { class: 'ln' }, op.a === null ? '' : String(op.a + 1)),
          h('td', { class: 'ln' }, op.b === null ? '' : String(op.b + 1)),
          h('td', { class: 'sg', 'aria-hidden': 'true' }, op.t === 'add' ? '+' : op.t === 'del' ? '−' : ' '),
          h('td', { class: 'code' }, h('span', { class: 'sr-only' }, op.t === 'add' ? 'added: ' : op.t === 'del' ? 'removed: ' : ''), text ?? '')));
      }
    }
    return h('table', { class: 'diff' }, tbody);
  }

  function renderDiff(data) {
    const body = clear($('files-body'));
    const files = Array.isArray(data.files) ? data.files : [];
    body.append(h('div', { class: 'diff-head' }, 'Comparing ', digestButton(data.from), h('span', { class: 'arrow' }, '→'), digestButton(data.to)));
    if (!files.length) { body.append(h('p', { class: 'empty' }, 'No differences between these roots.')); return; }
    let adds = 0;
    let dels = 0;
    const sections = [];
    for (const f of files) {
      const status = String(f.status || 'modified');
      const oldLines = status === 'added' ? [] : splitLines(f.old);
      const newLines = status === 'removed' ? [] : splitLines(f.new);
      const section = h('section', { class: 'dfile', 'aria-label': String(f.path) });
      const headEl = h('div', { class: 'dfile-head' }, badge(status, status === 'added' ? 'ok' : status === 'removed' ? 'fail' : 'info'), h('span', { class: 'path' }, String(f.path ?? '')));
      section.append(headEl);
      const bodyEl = h('div', { class: 'dfile-body' });
      let a = 0;
      let d = 0;
      if (oldLines.length > DIFF_MAX_LINES || newLines.length > DIFF_MAX_LINES) {
        bodyEl.append(h('div', { class: 'diff-note' }, 'File too large to diff (' + fmtInt(Math.max(oldLines.length, newLines.length)) + ' lines) — showing the new version.'));
        bodyEl.append(codeView(status === 'removed' ? f.old : f.new));
        a = newLines.length; d = oldLines.length;
      } else {
        const ops = status === 'added' ? newLines.map((_, j) => ({ t: 'add', a: null, b: j }))
          : status === 'removed' ? oldLines.map((_, i) => ({ t: 'del', a: i, b: null }))
            : diffLines(oldLines, newLines);
        for (const op of ops) { if (op.t === 'add') a++; else if (op.t === 'del') d++; }
        if (a + d === 0) bodyEl.append(h('p', { class: 'diff-note' }, 'Only whitespace or line-ending changes.'));
        else bodyEl.append(diffTable(oldLines, newLines, ops));
      }
      adds += a; dels += d;
      headEl.append(h('span', { class: 'counts', 'aria-label': a + ' added, ' + d + ' removed lines' }, h('span', { class: 'plus' }, '+' + a), h('span', { class: 'minus' }, '−' + d)));
      section.append(bodyEl);
      sections.push(section);
    }
    body.append(h('div', { class: 'diff-summary' },
      h('span', null, h('b', null, String(files.length)), ' file' + (files.length === 1 ? '' : 's') + ' changed'),
      h('span', { class: 'counts' }, h('span', { class: 'plus' }, '+' + adds), h('span', { class: 'minus' }, '−' + dels))));
    body.append(...sections);
  }

  async function proposeExport(btn) {
    btn.disabled = true;
    try {
      const res = await api('/api/export', { method: 'POST' });
      toast('Export proposed' + (res.approval_id ? ' (' + res.approval_id + ')' : '') + ' — approve it in the chat.');
    } catch (err) {
      reportError(err);
    } finally {
      btn.disabled = false;
    }
  }

  // --------------------------------------------------------------- events

  const EVENTS_PAGE = 100;

  function eventRow(ev, isNew) {
    return h('div', { class: 'ev-row' + (isNew ? ' new' : ''), role: 'row' },
      h('span', { class: 'ev-seq', role: 'cell' }, '#' + String(ev.sequence ?? '')),
      h('span', { class: 'ev-type', role: 'cell' }, String(ev.type ?? '')),
      h('span', { class: 'ev-actor', role: 'cell' }, String(ev.actor ?? '')),
      h('span', { class: 'ev-time', role: 'cell', title: fmtDateTime(ev.recorded_at) }, fmtTime(ev.recorded_at)),
      h('span', { class: 'ev-sum', role: 'cell' }, fmtText(ev.summary)));
  }

  function eventsRange() {
    if (!state.events.length) return [0, 0];
    return [Number(state.events[0].sequence) || 0, Number(state.events[state.events.length - 1].sequence) || 0];
  }

  async function fetchEvents(after, limit) {
    const data = await api('/api/events?after=' + Math.max(0, after) + '&limit=' + limit);
    return (Array.isArray(data.events) ? data.events : []).filter((e) => e && e.sequence !== undefined);
  }

  function mergeEvents(list, isNew) {
    const seen = new Set(state.events.map((e) => Number(e.sequence)));
    const fresh = list.filter((e) => !seen.has(Number(e.sequence)));
    state.events = [...state.events, ...fresh].sort((x, y) => Number(x.sequence) - Number(y.sequence));
    const rows = clear($('events-rows'));
    const freshSet = new Set(isNew ? fresh.map((e) => Number(e.sequence)) : []);
    for (const e of state.events) rows.append(eventRow(e, freshSet.has(Number(e.sequence))));
    return fresh.length;
  }

  async function loadEvents() {
    if (state.eventsLoading) return;
    state.eventsLoading = true;
    renderEventsFooter();
    try {
      if (!state.status) await loadStatus().catch(() => {});
      const head = Number(state.status && state.status.head_sequence) || 0;
      const after = head > EVENTS_PAGE ? head - EVENTS_PAGE : 0;
      const list = await fetchEvents(after, EVENTS_PAGE);
      state.eventsLoaded = true;
      mergeEvents(list, false);
    } catch (err) {
      reportError(err);
    } finally {
      state.eventsLoading = false;
      renderEventsFooter();
    }
  }

  async function loadMoreEvents() {
    if (state.eventsLoading) return;
    state.eventsLoading = true;
    renderEventsFooter();
    try {
      const [, last] = eventsRange();
      const sc = $('events-scroll');
      const stick = isNearBottom(sc);
      const n = mergeEvents(await fetchEvents(last, EVENTS_PAGE), true);
      if (!n) toast('No newer events.');
      if (stick) sc.scrollTop = sc.scrollHeight;
    } catch (err) {
      reportError(err);
    } finally {
      state.eventsLoading = false;
      renderEventsFooter();
    }
  }

  async function loadEarlierEvents() {
    if (state.eventsLoading) return;
    const [first] = eventsRange();
    if (first <= 1) return;
    state.eventsLoading = true;
    renderEventsFooter();
    try {
      const after = Math.max(0, first - 1 - EVENTS_PAGE);
      const sc = $('events-scroll');
      const before = sc.scrollHeight;
      mergeEvents(await fetchEvents(after, first - 1 - after), false);
      sc.scrollTop += sc.scrollHeight - before;
    } catch (err) {
      reportError(err);
    } finally {
      state.eventsLoading = false;
      renderEventsFooter();
    }
  }

  function renderEventsFooter() {
    const top = clear($('events-top'));
    const bottom = clear($('events-bottom'));
    if (!state.eventsLoaded) {
      if (state.eventsLoading) bottom.append(h('span', null, 'Loading events…'));
      return;
    }
    const [first, last] = eventsRange();
    const head = Number(state.status && state.status.head_sequence) || last;
    if (first > 1) {
      top.append(h('button', { type: 'button', class: 'btn btn-secondary btn-sm', disabled: state.eventsLoading, onclick: loadEarlierEvents }, 'Load earlier events'),
        h('span', null, (first - 1) + ' earlier event' + (first - 1 === 1 ? '' : 's')));
    }
    const newer = Math.max(0, head - last);
    bottom.append(h('button', { type: 'button', class: 'btn btn-secondary btn-sm', disabled: state.eventsLoading, onclick: loadMoreEvents },
      newer ? 'Load more (' + newer + ' new)' : 'Load more'),
    h('span', null, state.events.length ? 'Showing #' + first + '–#' + last + ' · head #' + head : 'No events recorded yet.'));
  }

  // ------------------------------------------------------------------ tabs

  const WORK_TABS = ['activity', 'tasks', 'files', 'events'];
  const mobileQuery = window.matchMedia('(max-width: 1023px)');

  function setWorkTab(name) {
    if (!WORK_TABS.includes(name)) return;
    state.workTab = name;
    for (const b of document.querySelectorAll('#work-tabs [role="tab"]')) {
      const on = b.dataset.tab === name;
      b.setAttribute('aria-selected', String(on));
      b.tabIndex = on ? 0 : -1;
    }
    for (const t of WORK_TABS) $('view-' + t).hidden = t !== name;
    if (name === 'events' && !state.eventsLoaded) loadEvents();
    if (name === 'files') ensureFiles();
    if (name === 'tasks') renderTasks();
    if (name === 'activity' && state.actStick) {
      const sc = $('activity-scroll');
      sc.scrollTop = sc.scrollHeight;
    }
    if (document.body.dataset.mobileView !== 'chat') setMobileView(name, true);
  }

  function setMobileView(v, fromWork) {
    document.body.dataset.mobileView = v;
    for (const b of document.querySelectorAll('#mobile-tabs [role="tab"]')) {
      const on = b.dataset.mview === v;
      b.setAttribute('aria-selected', String(on));
      b.tabIndex = on ? 0 : -1;
    }
    if (v === 'chat') {
      const sc = $('chat-scroll');
      if (state.chatStick) sc.scrollTop = sc.scrollHeight;
    } else if (!fromWork) {
      setWorkTab(v);
    }
  }

  /** Show a work tab, switching the narrow-screen view too. */
  function showWorkTab(name) {
    if (mobileQuery.matches) setMobileView(name);
    else setWorkTab(name);
  }

  function tablistKeys(container, selector) {
    container.addEventListener('keydown', (e) => {
      if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) return;
      const tabs = [...container.querySelectorAll(selector)];
      const i = tabs.indexOf(document.activeElement);
      if (i < 0) return;
      e.preventDefault();
      let j = i;
      if (e.key === 'ArrowLeft') j = (i - 1 + tabs.length) % tabs.length;
      if (e.key === 'ArrowRight') j = (i + 1) % tabs.length;
      if (e.key === 'Home') j = 0;
      if (e.key === 'End') j = tabs.length - 1;
      tabs[j].focus();
      tabs[j].click();
    });
  }

  // ------------------------------------------------------------------ SSE

  let es = null;
  let reconnectTimer = null;
  let backoff = 1000;
  let lastMessageAt = 0;

  function parseData(e) {
    try { return JSON.parse(e.data); } catch (_) { return null; }
  }

  function connectStream() {
    if (state.expired) return;
    if (es) es.close();
    setConn(state.conn === 'live' ? 'reconnecting' : state.conn === 'offline' ? 'offline' : 'connecting');
    const src = new EventSource('/api/stream');
    es = src;
    const touch = () => { lastMessageAt = Date.now(); };
    src.onopen = () => {
      if (es !== src) return;
      touch();
      backoff = 1000;
      setConn('live');
      refetchAll();
    };
    src.onerror = () => {
      if (es !== src) return;
      if (src.readyState === EventSource.CLOSED) {
        setConn('offline');
        scheduleReconnect();
      } else {
        setConn('reconnecting');
      }
    };
    src.addEventListener('activity', (e) => { touch(); const d = parseData(e); if (d) upsertItem(d); });
    src.addEventListener('chat', (e) => { touch(); const d = parseData(e); if (d) upsertMessage(d); });
    src.addEventListener('status', (e) => { touch(); const d = parseData(e); if (d) { state.status = d; renderStatus(); renderNow(); } });
    src.addEventListener('tasks', (e) => { touch(); const d = parseData(e); scheduleTasksRefresh(d && d.changed ? String(d.changed) : null); });
    src.addEventListener('ping', touch);
    src.onmessage = touch;
  }

  function scheduleReconnect() {
    clearTimeout(reconnectTimer);
    if (state.expired) return;
    reconnectTimer = setTimeout(async () => {
      try {
        await api('/api/status');
      } catch (err) {
        if (err.status === 401) return;
      }
      connectStream();
    }, backoff);
    backoff = Math.min(backoff * 2, 15000);
  }

  function watchdog() {
    // Pings arrive every 15 s; a silent stream for 45 s is treated as dead.
    if (state.conn === 'live' && es && Date.now() - lastMessageAt > 45000) {
      es.close();
      es = null;
      setConn('reconnecting');
      scheduleReconnect();
    }
  }

  let refetching = false;
  async function refetchAll() {
    if (refetching || state.expired) return;
    refetching = true;
    try {
      const results = await Promise.allSettled([loadStatus(), loadChat(), loadActivity(), loadTasks()]);
      const failed = results.find((r) => r.status === 'rejected');
      if (failed && failed.reason && failed.reason.status !== 401) toast('Could not refresh: ' + failed.reason.message, 'fail');
      if (state.openTask) loadTaskDetail(state.openTask).catch(() => {});
    } finally {
      refetching = false;
    }
  }

  // ------------------------------------------------------------------ init

  function init() {
    // Header pills and popovers.
    for (const pill of document.querySelectorAll('.pill[data-pop]')) {
      pill.addEventListener('click', (e) => { e.stopPropagation(); openPopover(pill.dataset.pop, pill); });
    }
    document.addEventListener('pointerdown', (e) => {
      if (!state.popover) return;
      const pop = $('popover');
      if (pop.contains(e.target) || (popAnchor && popAnchor.contains(e.target))) return;
      closePopover(false);
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && state.popover) { e.preventDefault(); closePopover(true); }
    });
    window.addEventListener('resize', positionPopover);

    for (const b of document.querySelectorAll('#jev-control [data-jev-mode]')) b.addEventListener('click', () => setJevMode(b.dataset.jevMode));

    // Composer.
    const ta = $('composer-input');
    $('composer').addEventListener('submit', (e) => { e.preventDefault(); sendMessage(); });
    ta.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && !e.altKey && !e.ctrlKey && !e.metaKey) {
        e.preventDefault();
        sendMessage();
      }
    });
    ta.addEventListener('input', autosize);
    for (const ex of document.querySelectorAll('#examples .example')) {
      ex.addEventListener('click', () => {
        ta.value = ex.textContent.trim();
        autosize();
        ta.focus();
        ta.setSelectionRange(ta.value.length, ta.value.length);
      });
    }

    // Scroll stickiness.
    $('chat-scroll').addEventListener('scroll', () => { state.chatStick = isNearBottom($('chat-scroll')); }, { passive: true });
    $('activity-scroll').addEventListener('scroll', () => {
      const sc = $('activity-scroll');
      if (sc.clientHeight === 0) return;
      state.actStick = isNearBottom(sc);
      if (state.actStick) $('jump-latest').hidden = true;
    }, { passive: true });
    $('jump-latest').addEventListener('click', () => {
      const sc = $('activity-scroll');
      sc.scrollTop = sc.scrollHeight;
      state.actStick = true;
      $('jump-latest').hidden = true;
    });

    // Tabs.
    for (const b of document.querySelectorAll('#work-tabs [role="tab"]')) b.addEventListener('click', () => setWorkTab(b.dataset.tab));
    for (const b of document.querySelectorAll('#mobile-tabs [role="tab"]')) b.addEventListener('click', () => setMobileView(b.dataset.mview));
    tablistKeys($('work-tabs'), '[role="tab"]');
    tablistKeys($('mobile-tabs'), '[role="tab"]');
    // Pick a placeholder that fits on one line of the current textarea width.
    const syncPlaceholder = () => {
      if (state.expired || !ta.clientWidth) return;
      ta.placeholder = ta.clientWidth >= 360 ? 'Describe a change, or ask about the project…' : 'Message INTELLECTUS…';
    };
    if (window.ResizeObserver) new ResizeObserver(syncPlaceholder).observe(ta);
    syncPlaceholder();
    mobileQuery.addEventListener('change', () => {
      if (!mobileQuery.matches) setWorkTab(state.workTab);
    });

    // Files.
    for (const b of document.querySelectorAll('[data-files-mode]')) b.addEventListener('click', () => setFilesMode(b.dataset.filesMode));
    $('root-select').addEventListener('change', (e) => { state.files.root = e.target.value; state.files.tree = null; loadTree(); });
    $('diff-from').addEventListener('change', (e) => { state.files.from = e.target.value; loadDiff(); });
    $('diff-to').addEventListener('change', (e) => { state.files.to = e.target.value; loadDiff(); });
    $('export-btn').addEventListener('click', (e) => proposeExport(e.currentTarget));

    setConn('connecting');
    renderNow();
    afterActivityChange();
    renderTasks();
    refetchAll();
    connectStream();
    setInterval(() => {
      tickElapsed();
      watchdog();
      if (state.pendingReply && Date.now() - state.pendingReply > 90000) { state.pendingReply = 0; $('typing').hidden = true; }
    }, 1000);
  }

  init();
})();
