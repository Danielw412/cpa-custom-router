'use strict';
/* Quota router panel. Data and actions use plugin Management API routes,
   which CLIProxyAPI protects with the management key. This page itself is a
   static resource and carries no data. */
(() => {
  const API = '/v0/management/custom-quota-router';
  const CONFIG = '/v8/management/config/plugins/configs/custom-quota-router';
  const REFRESH_MS = 5000;
  const KEY_STORE = 'cqr.key';
  const $ = (id) => document.getElementById(id);

  let key = '';
  let state = null;
  let timer = 0;
  let showAllHistory = false;
  let prefsDirty = false;
  let draftOrder = [];
  let draftRank = [];
  let lastRouting = null;
  let busy = false;
  let pauseDirty = false;
  let slowDirty = false;

  // ---------------------------------------------------------------- theme
  function parentTheme() {
    try {
      if (window.parent && window.parent !== window) {
        return window.parent.document.documentElement.getAttribute('data-theme') || '';
      }
    } catch (_) { /* cross-origin parent */ }
    return null;
  }
  function applyTheme() {
    const t = parentTheme();
    let dark;
    if (t === 'dark') dark = true;
    else if (t) dark = false;
    else dark = window.matchMedia('(prefers-color-scheme: dark)').matches;
    document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light');
  }
  function watchTheme() {
    applyTheme();
    window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', applyTheme);
    try {
      if (window.parent && window.parent !== window) {
        new MutationObserver(applyTheme).observe(window.parent.document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
      }
    } catch (_) { /* ignore */ }
  }

  // ---------------------------------------------------------------- key
  // When "Remember password" is ticked, the CLIProxyAPI dashboard keeps its
  // management key in this origin's localStorage, XOR-obfuscated ("enc::v1::").
  // Reusing it gives single sign-on inside the dashboard; otherwise the panel
  // asks once. Newer dashboards persist {state:{managementKey}} under
  // "cli-proxy-auth"; older ones used a plain "managementKey" entry.
  function deobfuscate(raw) {
    if (!raw || !raw.startsWith('enc::v1::')) return raw;
    const pad = new TextEncoder().encode(`cli-proxy-api-webui::secure-storage|${location.host}|${navigator.userAgent}`);
    const bin = atob(raw.slice(9));
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i) ^ pad[i % pad.length];
    return new TextDecoder().decode(bytes);
  }
  function dashboardKey() {
    try {
      const auth = JSON.parse(deobfuscate(localStorage.getItem('cli-proxy-auth')) || 'null');
      const k = auth && auth.state && auth.state.managementKey;
      if (typeof k === 'string' && k) return k;
    } catch (_) { /* fall through */ }
    try {
      const text = deobfuscate(localStorage.getItem('managementKey'));
      if (!text) return '';
      try {
        const v = JSON.parse(text);
        return typeof v === 'string' ? v : '';
      } catch (_) {
        return text;
      }
    } catch (_) {
      return '';
    }
  }
  function savedKey() {
    return sessionStorage.getItem(KEY_STORE) || localStorage.getItem(KEY_STORE) || dashboardKey();
  }
  function rememberKey(k, persist) {
    sessionStorage.setItem(KEY_STORE, k);
    if (persist) localStorage.setItem(KEY_STORE, k);
  }
  function forgetKey() {
    sessionStorage.removeItem(KEY_STORE);
    localStorage.removeItem(KEY_STORE);
  }

  // ---------------------------------------------------------------- http
  async function request(path, method = 'GET', body) {
    const headers = { Authorization: `Bearer ${key}` };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    const res = await fetch(path, {
      method, headers, cache: 'no-store', credentials: 'same-origin',
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch (_) { data = null; }
    if (res.status === 401) {
      const err = new Error('The management key was rejected.');
      err.auth = true;
      throw err;
    }
    if (!res.ok) {
      const msg = (data && (data.error || data.message)) || `Request failed with HTTP ${res.status}.`;
      throw new Error(typeof msg === 'string' ? msg : `Request failed with HTTP ${res.status}.`);
    }
    return data;
  }

  // ---------------------------------------------------------------- format
  const fmtTime = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' });
  const fmtDay = new Intl.DateTimeFormat(undefined, { weekday: 'short', hour: '2-digit', minute: '2-digit' });
  const isSet = (iso) => iso && !iso.startsWith('0001-');
  function clock(iso) {
    const d = new Date(iso);
    const now = new Date();
    return d.toDateString() === now.toDateString() ? fmtTime.format(d) : fmtDay.format(d);
  }
  function ago(iso) {
    if (!isSet(iso)) return 'never';
    const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
    if (s < 45) return 'just now';
    if (s < 3600) return `${Math.round(s / 60)} min ago`;
    if (s < 86400) return `${Math.round(s / 3600)} h ago`;
    return clock(iso);
  }
  function until(iso) {
    const s = (new Date(iso).getTime() - Date.now()) / 1000;
    if (s <= 60) return 'in a minute';
    if (s < 3600) return `in ${Math.round(s / 60)} min`;
    if (s < 6 * 3600) {
      const h = Math.floor(s / 3600);
      const m = Math.round((s % 3600) / 60);
      return m ? `in ${h} h ${m} min` : `in ${h} h`;
    }
    return clock(iso);
  }
  const pct = (v) => `${Math.round(v)}%`;

  // ---------------------------------------------------------------- dom
  function el(tag, attrs, ...kids) {
    const node = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v === undefined || v === null || v === false) continue;
        if (k === 'class') node.className = v;
        else if (k === 'text') node.textContent = v;
        else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
        else node.setAttribute(k, v === true ? '' : String(v));
      }
    }
    for (const kid of kids) if (kid !== null && kid !== undefined && kid !== false) node.append(kid);
    return node;
  }
  function toast(message, isError) {
    const t = $('toast');
    t.textContent = message;
    t.classList.toggle('error', !!isError);
    t.hidden = false;
    clearTimeout(toast.timer);
    toast.timer = setTimeout(() => { t.hidden = true; }, isError ? 6000 : 2600);
  }
  function labelOf(id) {
    const a = state && state.accounts.find((x) => x.id === id);
    return a ? a.label : (id || '');
  }

  // ---------------------------------------------------------------- render
  function render() {
    if (!state) return;
    const manual = state.mode === 'manual';
    $('mode-auto').setAttribute('aria-pressed', String(!manual));
    $('mode-manual').setAttribute('aria-pressed', String(manual));

    // Route band
    const route = $('route');
    const routing = state.routing || '';
    let routeState = 'auto';
    if (!routing) routeState = 'none';
    else if (manual && routing === state.pinned) routeState = 'manual';
    else if (manual) routeState = 'fallback';
    route.dataset.state = routeState;
    $('route-account').textContent = routing ? labelOf(routing) : 'No account available';
    $('route-why').textContent = state.routing_note || (routing ? '' : 'Waiting for CLIProxyAPI to report Codex accounts.');
    const elsewhere = routing ? state.accounts.filter((a) => a.id !== routing && a.busy_sessions > 0) : [];
    const ew = $('route-elsewhere');
    ew.hidden = elsewhere.length === 0;
    if (elsewhere.length) {
      const n = elsewhere.reduce((t, a) => t + a.busy_sessions, 0);
      const one = n === 1;
      const where = elsewhere.map((a) => (elsewhere.length > 1 ? `${a.label} (${a.busy_sessions})` : a.label)).join(', ');
      ew.textContent = `${n} busy session${one ? ' is' : 's are'} still running on ${where}.` + (manual ? '' :
        ` ${one ? 'It stays' : 'They stay'} there to keep ${one ? 'its' : 'their'} prompt cache and ${one ? 'moves' : 'move'} here after ${state.settings.cache_warm_minutes} min idle.`);
    }
    if (lastRouting !== null && lastRouting !== routing) {
      route.classList.remove('changed');
      void route.offsetWidth;
      route.classList.add('changed');
    }
    lastRouting = routing;

    const notes = [...(state.warnings || [])];
    if (state.last_error) notes.push(state.last_error);
    $('notice').hidden = notes.length === 0;
    $('notice').textContent = notes.join(' ');

    renderFlow();
    renderAccounts(manual);
    renderWindows();
    renderHistory();
    if (!prefsDirty) fillPrefs();

    const poll = state.settings.quota_poll_minutes;
    $('foot-status').textContent =
      `${state.picks} requests routed since the plugin started. ` +
      (poll > 0 ? `Idle accounts are checked every ${poll} min` : 'Quota comes from live traffic only') +
      (isSet(state.last_poll) ? `, last check ${ago(state.last_poll)}.` : '.');
  }

  // ---------------------------------------------------------------- pause
  function renderFlow() {
    const p = state.pause || null;
    const slow = state.slow_down_seconds || 0;
    const waiting = state.waiting || 0;
    $('flow').dataset.paused = String(!!p);
    $('flow').dataset.slow = String(!p && slow > 0);
    $('route').dataset.paused = String(!!p);
    $('route-lead-text').textContent = p ? 'Paused. When requests resume, they go to' : 'New requests go to';

    const waitingText = waiting === 1 ? '1 request is waiting' : `${waiting} requests are waiting`;
    const detail = [];
    let head;
    if (p) {
      head = isSet(p.until) ? `Paused until ${clock(p.until)} (${until(p.until)})` : 'Paused until you resume';
      detail.push(`Since ${clock(p.since)}. ${waiting ? waitingText : 'No requests are waiting'}.`);
      if (p.auto_resume_minutes) {
        detail.push(`Auto-resume every ${p.auto_resume_minutes} min lets each waiting conversation take one step, then pauses it again. Next ${clock(p.next_auto_resume)}.`);
      } else {
        detail.push('Auto-resume is off. A turn that waits a long time can stop with an error in Codex; type "continue" there after you resume, and its cache is still used.');
      }
      if (slow) detail.push(`After you resume, each request waits ${slow} s.`);
    } else if (slow) {
      head = `Slowed down: each request waits ${slow} s before it is sent`;
      if (waiting) detail.push(`${waitingText}.`);
    } else {
      head = 'Running at full speed';
    }
    $('flow-state').textContent = head;
    $('flow-detail').textContent = detail.join(' ');

    // While paused, resuming is the main action.
    $('pause-go').textContent = p ? 'Update pause' : 'Pause';
    $('pause-go').className = p ? 'btn' : 'primary';
    $('resume-go').className = p ? 'primary' : 'btn';
    $('resume-go').hidden = !p;
    const form = $('pause-form');
    if (p && !pauseDirty && !form.contains(document.activeElement)) {
      $('pause-auto').checked = p.auto_resume_minutes > 0;
      if (p.auto_resume_minutes) $('pause-auto-minutes').value = p.auto_resume_minutes;
    }
    $('pause-auto-minutes').disabled = !$('pause-auto').checked;

    $('slow-off').hidden = slow === 0;
    if (!slowDirty && !$('slow-form').contains(document.activeElement)) $('slow-seconds').value = slow;
  }

  async function pauseNow(ev) {
    ev.preventDefault();
    const minutes = Number($('pause-minutes').value);
    let auto = 0;
    if ($('pause-auto').checked) {
      auto = minutesFrom('pause-auto-minutes', 1, 24);
      if (auto == null) { toast('Auto-resume must be every 1 to 24 minutes.', true); return; }
    }
    const was = !!(state && state.pause);
    const label = $('pause-minutes').selectedOptions[0].textContent;
    pauseDirty = false;
    await act('pause', { minutes, auto_resume_minutes: auto },
      was ? 'Pause updated' : (minutes ? `Paused for ${label}` : 'Paused until you resume'));
  }
  const resumeNow = () => { pauseDirty = false; return act('resume', {}, 'Resumed: waiting requests continue'); };
  async function slowDown(ev) {
    ev.preventDefault();
    const s = minutesFrom('slow-seconds', 0, 240);
    if (s == null) { toast('Enter 0 to 240 seconds.', true); return; }
    slowDirty = false;
    await act('slowdown', { seconds: s }, s ? `Each request now waits ${s} s` : 'Slow down is off');
  }
  const slowOff = () => { slowDirty = false; return act('slowdown', { seconds: 0 }, 'Slow down is off'); };

  function meter(name, w, threshold, active) {
    const known = !!w;
    const remaining = known ? w.remaining_percent : 0;
    const fill = el('span', { class: 'fill' });
    fill.style.width = `${Math.max(0, Math.min(100, remaining))}%`;
    if (known && remaining < threshold) fill.classList.add('stop');
    else if (known && remaining < Math.max(threshold * 2, 15) && !active) fill.classList.add('caution');
    if (known && w.estimated) fill.classList.add('estimated');
    const tick = el('span', { class: 'tick', title: `Switch threshold ${pct(threshold)}` });
    tick.style.left = `${threshold}%`;
    const label = !known ? '—' : (w.estimated ? `~${pct(remaining)}` : pct(remaining));
    const desc = !known ? `${name}: not observed yet` : `${name}: ${pct(remaining)} remaining`;
    return el('div', { class: 'meter' },
      el('span', { class: 'meter-label', text: name }),
      el('span', { class: 'bar', role: 'meter', 'aria-label': desc, 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(Math.round(remaining)) }, fill, tick),
      el('span', { class: known ? 'pct' : 'pct unknown', text: label }));
  }

  function accountSub(a) {
    const bits = [];
    if (a.plan) bits.push(`${a.plan[0].toUpperCase()}${a.plan.slice(1)} plan`);
    if (a.role === 'reserve') bits.push('reserve');
    const busy = a.busy_sessions || 0;
    const idle = a.idle_sessions || 0;
    if (busy) bits.push(`${busy} session${busy === 1 ? '' : 's'} working now`);
    if (idle) bits.push(busy ? `${idle} idle` : `${idle} idle session${idle === 1 ? '' : 's'}`);
    if (idle && !busy && isSet(a.last_used)) bits.push(`last used ${clock(a.last_used)}`);
    const sub = el('span', { class: 'acct-sub' }, bits.join(', '));
    const stateText = {
      low: 'below threshold', exhausted: 'exhausted', unavailable: 'unavailable', off: 'off', unknown: 'no quota data yet',
    }[a.state];
    if (stateText) {
      if (bits.length) sub.append(', ');
      sub.append(el('span', { class: `state-${a.state}`, text: stateText }));
    }
    return sub;
  }

  function accountDetail(a) {
    const parts = [];
    if (a.state === 'exhausted' || a.state === 'unavailable' || a.state === 'off') parts.push(a.detail);
    const resets = [];
    if (a.short && isSet(a.short.reset_at)) resets.push(`5-hour window resets ${until(a.short.reset_at)}`);
    if (a.long && isSet(a.long.reset_at)) resets.push(`weekly window ${until(a.long.reset_at)}`);
    if (resets.length) parts.push(resets.join(', '));
    if ((a.short && a.short.estimated) || (a.long && a.long.estimated)) parts.push('A window has reset; waiting for fresh numbers');
    if (isSet(a.observed_at)) parts.push(`${a.stale ? 'Quota data is old, from' : 'Updated'} ${ago(a.observed_at)}`);
    if (a.poll_error) parts.push(`Quota check failed: ${a.poll_error}`);
    const sentence = (t) => (t ? t[0].toUpperCase() + t.slice(1) : t);
    return parts.length ? `${parts.map(sentence).join('. ')}.` : '';
  }

  function renderAccounts(manual) {
    const list = $('accounts');
    const focused = document.activeElement && document.activeElement.dataset ? { ...document.activeElement.dataset } : null;
    list.replaceChildren();
    if (!state.accounts.length) {
      list.append(el('li', { class: 'empty', text: 'No Codex accounts found in CLIProxyAPI yet. Add one under Auth files in the dashboard.' }));
      return;
    }
    const threshold = state.settings.switch_threshold_percent;
    for (const a of state.accounts) {
      const isRoute = a.id === state.routing;
      const actions = el('div', { class: 'acct-actions' });
      if (manual && a.pinned) {
        actions.append(el('button', { type: 'button', class: 'btn on', 'aria-pressed': 'true', 'data-action': 'unpin', 'data-id': a.id, title: 'Return to automatic routing', onclick: () => setAuto() }, 'Pinned'));
      } else {
        actions.append(el('button', {
          type: 'button', class: 'btn', 'data-action': 'use', 'data-id': a.id,
          disabled: !manual && isRoute ? true : undefined,
          title: manual ? 'Leave manual mode and continue automatically from this account'
            : (isRoute ? 'New sessions go here' : 'Send new sessions here now; automatic routing continues'),
          onclick: () => selectAccount(a.id),
        }, !manual && isRoute ? 'Current' : 'Use now'));
        actions.append(el('button', { type: 'button', class: 'btn', 'aria-pressed': 'false', 'data-action': 'pin', 'data-id': a.id, title: 'Route every request to this account until you unpin it', onclick: () => pin(a.id) }, 'Pin'));
      }
      const row = el('li', { class: 'acct', 'data-state': a.state, 'data-active': isRoute ? true : undefined },
        el('div', { class: 'acct-id' },
          el('span', { class: 'lamp', 'aria-hidden': 'true' }),
          el('div', { class: 'acct-text' },
            el('span', { class: 'acct-name', text: a.label, title: a.id }),
            accountSub(a))),
        el('div', { class: 'meters' }, meter('5h', a.short, threshold, isRoute), meter('Week', a.long, threshold, isRoute)),
        actions);
      const detail = accountDetail(a);
      if (detail) row.append(el('p', { class: 'acct-detail', text: detail }));
      list.append(row);
    }
    if (focused && focused.action && focused.id) {
      const again = list.querySelector(`button[data-action="${CSS.escape(focused.action)}"][data-id="${CSS.escape(focused.id)}"]`);
      if (again && !again.disabled) again.focus();
    }
  }

  function renderHistory() {
    const list = $('history');
    list.replaceChildren();
    const items = state.history || [];
    if (!items.length) {
      list.append(el('li', { class: 'empty', text: 'No switches yet. Routing events will appear here.' }));
    }
    const shown = showAllHistory ? items : items.slice(0, 8);
    for (const h of shown) {
      const what = el('span', { class: 'what' }, h.reason);
      if (h.count > 1) what.append(el('span', { class: 'count', text: `${h.count}×` }));
      list.append(el('li', { 'data-kind': h.kind },
        el('time', { datetime: h.at, title: new Date(h.at).toLocaleString(), text: clock(h.at) }),
        el('span', { class: 'glyph', 'aria-hidden': 'true' }),
        what));
    }
    const toggle = $('history-toggle');
    toggle.hidden = items.length <= 8;
    toggle.textContent = showAllHistory ? 'Show fewer' : `Show all ${items.length}`;
  }

  // ---------------------------------------------------------------- windows
  // editing: { id, mode: 'in' | 'at', minutes, time: "HH:MM", daily } while a
  // row's scheduler is open.
  let editing = null;
  let lastMinutes = 30;

  function nextOccurrence(hhmm) {
    const [h, m] = hhmm.split(':').map(Number);
    const d = new Date();
    d.setHours(h, m, 0, 0);
    if (d.getTime() < Date.now() + 30000) d.setDate(d.getDate() + 1);
    return d;
  }
  function planText(p, a) {
    const t = new Date(p.at).getTime();
    const soon = t - Date.now() < 6 * 3600 * 1000;
    let text = `Starts ${clock(p.at)}${soon ? ` (${until(p.at)})` : ''}${p.daily ? ', every day' : ''}`;
    if (a && a.window === 'running' && isSet(a.window_reset_at) && t < new Date(a.window_reset_at).getTime()) {
      text += '. The current window is still running then, so nothing will be sent';
    }
    return text;
  }
  const num = (v) => Number(v).toLocaleString();
  const pctNum = (v) => `${Math.round(v * 10) / 10}%`;

  function renderWindows(force) {
    const s = state.starter || { plans: [], log: [] };
    $('win-intro').textContent = `A 5-hour window only starts at an account's first request. Start it on purpose with one tiny ${s.model} (${s.effort} reasoning) request, so it resets when you want.`;
    $('win-off').disabled = !(s.plans && s.plans.length);
    renderCost(s);
    const list = $('win-list');
    // Don't rebuild under the user's cursor while they edit a start time.
    const openEditor = list.querySelector('.win-editor');
    if (!force && editing && openEditor && openEditor.contains(document.activeElement)) return;
    const focused = document.activeElement && document.activeElement.dataset ? { ...document.activeElement.dataset } : null;
    list.replaceChildren();
    if (!state.accounts.length) {
      list.append(el('li', { class: 'empty', text: 'No Codex accounts yet.' }));
      return;
    }
    for (const a of state.accounts) {
      const stateText = a.window === 'running' ? `Running, resets ${clock(a.window_reset_at)}`
        : a.window === 'idle' ? 'Not started' : 'Not checked yet';
      const status = el('div', { class: 'win-status' },
        el('span', { class: 'win-state', 'data-window': a.window },
          el('span', { class: 'lamp', 'aria-hidden': 'true' }), stateText));
      for (const p of a.plans || []) {
        status.append(el('span', { class: 'win-plan' }, planText(p, a),
          el('button', { type: 'button', class: 'link', 'data-action': 'win-cancel', 'data-id': p.id,
            'aria-label': `Cancel scheduled start for ${a.label}`, onclick: () => cancelPlan(p.id) }, 'Cancel')));
      }
      const running = a.window === 'running';
      const open = !!(editing && editing.id === a.id);
      const row = el('li', { class: 'win-row' },
        el('div', null, el('div', { class: 'win-name', text: a.label, title: a.id }), status),
        el('div', { class: 'win-btns' },
          el('button', { type: 'button', class: 'btn', 'data-action': 'win-start', 'data-id': a.id,
            disabled: running ? true : undefined,
            title: running ? 'This window is already running' : 'Send one tiny request now',
            onclick: () => startOne(a.id) }, 'Start now'),
          el('button', { type: 'button', class: open ? 'btn on' : 'btn', 'data-action': 'win-edit', 'data-id': a.id,
            'aria-expanded': String(open), onclick: () => toggleEditor(a) }, 'Schedule')));
      if (open) row.append(editorFor(a));
      list.append(row);
    }
    if (focused && focused.action && focused.id) {
      const again = list.querySelector(`[data-action="${CSS.escape(focused.action)}"][data-id="${CSS.escape(focused.id)}"]`);
      if (again && !again.disabled) again.focus();
    }
  }

  function renderCost(s) {
    const c = s.last_cost;
    if (!c) {
      $('win-cost').textContent = 'Cost per start: not measured yet. It is recorded the first time a window is started.';
      return;
    }
    const change = (b, a) => (b == null || a == null ? 'not reported' : `${pctNum(b)} → ${pctNum(a)}`);
    $('win-cost').textContent =
      `Last measured start (${labelOf(c.account)}, ${clock(c.at)}): 5-hour usage ${change(c.short_used_before, c.short_used_after)}, ` +
      `weekly ${change(c.long_used_before, c.long_used_after)}; ${num(c.input_tokens)} input tokens ` +
      `(${num(c.cached_tokens || 0)} cached), ${num(c.output_tokens)} output (${num(c.reasoning_tokens || 0)} reasoning). ` +
      'Codex reports usage in whole percents.';
  }

  function toggleEditor(a) {
    if (editing && editing.id === a.id) {
      editing = null;
    } else {
      const manual = (a.plans || []).find((p) => p.source === 'manual');
      let time;
      if (manual) {
        const d = new Date(manual.at);
        time = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
      } else {
        const d = new Date(Date.now() + 3600 * 1000);
        time = `${String(d.getHours()).padStart(2, '0')}:00`;
      }
      editing = { id: a.id, mode: manual ? 'at' : 'in', minutes: lastMinutes, time, daily: manual ? !!manual.daily : false };
    }
    renderWindows(true);
    focusEditor();
  }

  function focusEditor() {
    if (!editing) return;
    const f = $(editing.mode === 'in' ? 'win-minutes' : 'win-time');
    if (f) {
      f.focus();
      if (editing.mode === 'in' && f.select) f.select();
    }
  }

  const validMinutes = (v) => Number.isInteger(Number(v)) && String(v).trim() !== '' && Number(v) >= 1 && Number(v) <= 1440;

  function editorFor(a) {
    const preview = el('span', { class: 'preview', id: 'win-preview', 'aria-live': 'polite' });
    const update = () => {
      let at;
      if (editing.mode === 'in') {
        if (!validMinutes(editing.minutes)) {
          preview.textContent = 'Enter 1 to 1440 minutes.';
          return;
        }
        at = new Date(Date.now() + Number(editing.minutes) * 60000);
      } else {
        if (!/^\d{2}:\d{2}$/.test(editing.time || '')) {
          preview.textContent = 'Pick a time.';
          return;
        }
        at = nextOccurrence(editing.time);
      }
      const resets = new Date(at.getTime() + 5 * 3600 * 1000);
      preview.textContent = `Starts ${clock(at.toISOString())}, resets ${clock(resets.toISOString())}` +
        `${editing.mode === 'at' && editing.daily ? ', every day' : ''}.`;
    };
    const modeButton = (mode, label) => el('button', { type: 'button', 'aria-pressed': String(editing.mode === mode),
      onclick: () => { editing.mode = mode; renderWindows(true); focusEditor(); } }, label);
    const fields = [];
    if (editing.mode === 'in') {
      fields.push(
        el('label', { for: 'win-minutes', text: 'Start in' }),
        el('input', { type: 'number', id: 'win-minutes', min: '1', max: '1440', step: '1', value: String(editing.minutes), required: true,
          oninput: (ev) => { editing.minutes = ev.target.value; update(); } }),
        el('span', { class: 'unit', text: 'min' }));
    } else {
      const daily = el('input', { type: 'checkbox', id: 'win-daily', onchange: (ev) => { editing.daily = ev.target.checked; update(); } });
      daily.checked = editing.daily;
      fields.push(
        el('label', { for: 'win-time', text: 'Start at' }),
        el('input', { type: 'time', id: 'win-time', value: editing.time, required: true,
          oninput: (ev) => { editing.time = ev.target.value; update(); } }),
        el('span', { class: 'field-check' }, daily, el('label', { for: 'win-daily', text: 'Every day' })));
    }
    update();
    return el('form', { class: 'win-editor', 'aria-label': `Schedule ${a.label}`, novalidate: true,
      onsubmit: (ev) => { ev.preventDefault(); saveEditor(a); } },
      el('div', { class: 'mode small', role: 'group', 'aria-label': 'When to start' },
        modeButton('in', 'In minutes'), modeButton('at', 'At a time')),
      ...fields,
      preview,
      el('button', { type: 'submit', class: 'primary' }, editing.mode === 'in' ? 'Schedule' : 'Save'),
      el('button', { type: 'button', class: 'link', onclick: () => { editing = null; renderWindows(true); } }, 'Cancel'));
  }

  async function saveEditor(a) {
    if (!editing) return;
    if (editing.mode === 'in') {
      if (!validMinutes(editing.minutes)) {
        toast('Enter 1 to 1440 minutes.', true);
        return;
      }
      const m = Number(editing.minutes);
      lastMinutes = m;
      editing = null;
      const at = new Date(Date.now() + m * 60000).toISOString();
      await act('windows/start', { account: a.id, delay_minutes: m }, `${a.label} will start at ${clock(at)} (in ${m} min)`);
      return;
    }
    if (!/^\d{2}:\d{2}$/.test(editing.time || '')) {
      toast('Pick a start time first.', true);
      return;
    }
    const at = nextOccurrence(editing.time);
    const body = { account: a.id, at: at.toISOString(), daily: editing.daily };
    editing = null;
    await act('windows/schedule', body, `${a.label} will start at ${clock(at.toISOString())}${body.daily ? ', every day' : ''}`);
  }

  function minutesFrom(id, lo, hi) {
    const input = $(id);
    const v = Number(input.value);
    const ok = input.value.trim() !== '' && Number.isInteger(v) && v >= lo && v <= hi;
    input.setAttribute('aria-invalid', String(!ok));
    return ok ? v : null;
  }
  const settleRefresh = () => { setTimeout(() => refresh().catch(() => {}), 7000); setTimeout(() => refresh().catch(() => {}), 14000); };
  async function startAllNow() {
    await act('windows/start', { delay_minutes: 0, stagger_minutes: 0 }, 'Starting windows. Results appear here in a few seconds.');
    settleRefresh();
  }
  async function startOne(id) {
    await act('windows/start', { account: id }, `Starting ${labelOf(id)}'s window`);
    settleRefresh();
  }
  async function startDelayed(ev) {
    ev.preventDefault();
    const m = minutesFrom('win-delay', 1, 1440);
    if (m == null) { toast('Enter 1 to 1440 minutes.', true); return; }
    const at = new Date(Date.now() + m * 60000).toISOString();
    await act('windows/start', { delay_minutes: m }, `All windows will start at ${clock(at)}`);
  }
  async function startStaggered(ev) {
    ev.preventDefault();
    const m = minutesFrom('win-stagger', 1, 720);
    if (m == null) { toast('Enter 1 to 720 minutes.', true); return; }
    await act('windows/start', { stagger_minutes: m }, `Starting one window now and the rest ${m} min apart`);
    settleRefresh();
  }
  const cancelPlan = (id) => act('windows/cancel', { id }, 'Scheduled start cancelled');
  const turnOff = () => act('windows/cancel', {}, 'All scheduled window starts are off');

  // ---------------------------------------------------------------- prefs
  const numberFields = {
    switch_threshold_percent: [0, 90], switch_back_margin_percent: [0, 90], min_dwell_minutes: [0, 1440],
    cache_warm_minutes: [0, 1440], quota_poll_minutes: [0, 1440],
  };
  function fillPrefs() {
    const s = state.settings;
    const form = $('prefs-form');
    for (const name of Object.keys(numberFields)) {
      form.elements[name].value = s[name];
      form.elements[name].removeAttribute('aria-invalid');
    }
    form.elements.manual_fallback.value = s.manual_fallback;
    form.elements.switch_back.checked = !!s.switch_back;
    syncSwitchBack();
    draftOrder = state.accounts.map((a) => ({ id: a.id, label: a.label, role: a.role }));
    renderOrder();
    const given = (s.rank_by || []).filter((c) => RANK[c]);
    draftRank = given.concat(Object.keys(RANK).filter((c) => !given.includes(c)));
    renderRank();
  }
  function syncSwitchBack() {
    const on = $('f-switchback').checked;
    $('f-margin').disabled = !on;
    $('f-dwell').disabled = !on;
  }
  // Ranking criteria, in the server's vocabulary (rank_by).
  const RANK = {
    account_order: ['Account order', 'Higher in your account order below.'],
    weekly_reset: ['Weekly quota resets soonest', 'Use weekly quota before it resets. Less than a day apart counts as a tie.'],
    five_hour_reset: ['5-hour window resets soonest', 'Use a running window before its headroom resets. Windows that haven\'t started go last.'],
    most_remaining: ['Most quota left', 'Within 5 points counts as a tie.'],
  };
  function moveButtons(label, i, count, onMove) {
    return el('span', { class: 'moves' },
      el('button', { type: 'button', class: 'btn', 'aria-label': `Move ${label} up`, disabled: i === 0 ? true : undefined, onclick: () => onMove(i, -1) }, '↑'),
      el('button', { type: 'button', class: 'btn', 'aria-label': `Move ${label} down`, disabled: i === count - 1 ? true : undefined, onclick: () => onMove(i, 1) }, '↓'));
  }
  function renderOrder() {
    const list = $('order-list');
    list.replaceChildren();
    draftOrder.forEach((a, i) => {
      const role = el('select', { 'aria-label': `Role for ${a.label}`, onchange: (ev) => { a.role = ev.target.value; markDirty(); } },
        el('option', { value: 'normal', text: 'Normal' }),
        el('option', { value: 'reserve', text: 'Reserve' }),
        el('option', { value: 'off', text: 'Off' }));
      role.value = a.role;
      list.append(el('li', null,
        el('span', { class: 'pos', text: String(i + 1) }),
        el('span', { class: 'name', text: a.label, title: a.id }),
        role,
        moveButtons(a.label, i, draftOrder.length, (at, d) => shift(draftOrder, at, d, renderOrder, 'order-list'))));
    });
  }
  function renderRank() {
    const list = $('rank-list');
    list.replaceChildren();
    draftRank.forEach((c, i) => {
      const [what, why] = RANK[c];
      list.append(el('li', null,
        el('span', { class: 'pos', text: String(i + 1) }),
        el('span', null, el('span', { class: 'what', text: what }), el('span', { class: 'why', text: why })),
        moveButtons(what, i, draftRank.length, (at, d) => shift(draftRank, at, d, renderRank, 'rank-list'))));
    });
  }
  // shift swaps an item with its neighbour, re-renders the list, and keeps
  // keyboard focus on the moved item's button.
  function shift(items, i, delta, rerender, listId) {
    const j = i + delta;
    if (j < 0 || j >= items.length) return;
    [items[i], items[j]] = [items[j], items[i]];
    markDirty();
    rerender();
    const btns = $(listId).children[j].querySelectorAll('.moves .btn');
    const target = delta < 0 ? btns[0] : btns[1];
    (target.disabled ? (delta < 0 ? btns[1] : btns[0]) : target).focus();
  }
  function markDirty() { prefsDirty = true; }

  function collectPrefs() {
    const form = $('prefs-form');
    const out = {};
    let ok = true;
    for (const [name, [lo, hi]] of Object.entries(numberFields)) {
      const input = form.elements[name];
      const v = Number(input.value);
      const valid = input.value.trim() !== '' && Number.isFinite(v) && v >= lo && v <= hi;
      input.setAttribute('aria-invalid', String(!valid));
      if (!valid) ok = false;
      out[name] = name.endsWith('percent') ? v : Math.round(v);
    }
    if (!ok) return null;
    out.rank_by = draftRank.slice();
    out.manual_fallback = form.elements.manual_fallback.value;
    out.switch_back = form.elements.switch_back.checked;
    // Keep entries for accounts not listed right now (e.g. temporarily
    // removed from CLIProxyAPI) so their order and role survive a save.
    const listed = new Set(draftOrder.map((a) => a.id));
    const keep = (ids) => (ids || []).filter((id) => !listed.has(id));
    const s = state.settings;
    out.account_order = draftOrder.map((a) => a.id).concat(keep(s.account_order));
    out.reserve_accounts = draftOrder.filter((a) => a.role === 'reserve').map((a) => a.id).concat(keep(s.reserve_accounts));
    out.disabled_accounts = draftOrder.filter((a) => a.role === 'off').map((a) => a.id).concat(keep(s.disabled_accounts));
    return out;
  }

  async function savePrefs(ev) {
    ev.preventDefault();
    const prefs = collectPrefs();
    if (!prefs) {
      toast('Fix the highlighted values first.', true);
      return;
    }
    const btn = $('prefs-save');
    btn.disabled = true;
    try {
      // Persist through CLIProxyAPI's own config API: it writes config.yaml
      // and hot-applies the change to the plugin.
      await request(CONFIG, 'PATCH', prefs);
      prefsDirty = false;
      await sleep(400);
      await refresh();
      toast('Preferences saved');
    } catch (err) {
      handleError(err, 'Preferences were not saved');
    } finally {
      btn.disabled = false;
    }
  }

  // ---------------------------------------------------------------- actions
  async function act(path, body, done) {
    if (busy) return;
    busy = true;
    try {
      state = await request(`${API}/${path}`, 'POST', body);
      render();
      if (done) toast(done);
    } catch (err) {
      handleError(err, 'That change did not apply');
    } finally {
      busy = false;
    }
  }
  const pin = (id) => act('mode', { mode: 'manual', account: id }, `Pinned ${labelOf(id)}`);
  const setAuto = () => act('mode', { mode: 'auto' }, 'Automatic routing resumed');
  const selectAccount = (id) => act('select', { account: id }, `New sessions now use ${labelOf(id)}`);

  function handleError(err, prefix) {
    if (err && err.auth) {
      forgetKey();
      showConnect('The management key was rejected. Enter it again.');
      return;
    }
    toast(`${prefix}: ${err && err.message ? err.message : 'unknown error'}`, true);
  }

  // ---------------------------------------------------------------- loop
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  async function refresh() {
    state = await request(`${API}/state?history=200`);
    render();
  }
  function schedule() {
    clearTimeout(timer);
    timer = setTimeout(async () => {
      if (document.visibilityState === 'visible' && !busy) {
        try { await refresh(); } catch (err) { if (err.auth) { handleError(err); return; } }
      }
      schedule();
    }, REFRESH_MS);
  }

  function showConnect(message) {
    clearTimeout(timer);
    $('app').hidden = true;
    $('connect').hidden = false;
    $('connect-error').hidden = !message;
    $('connect-error').textContent = message || '';
    $('connect-key').focus();
  }
  async function connect(ev) {
    ev.preventDefault();
    const candidate = $('connect-key').value.trim();
    if (!candidate) return;
    key = candidate;
    try {
      await refresh();
      rememberKey(candidate, $('connect-remember').checked);
      $('connect-key').value = '';
      $('connect').hidden = true;
      $('app').hidden = false;
      schedule();
    } catch (err) {
      $('connect-error').hidden = false;
      $('connect-error').textContent = err.auth ? 'That key was rejected. Use the key you sign in to the CLIProxyAPI dashboard with.' : err.message;
    }
  }

  async function boot() {
    watchTheme();
    $('connect-form').addEventListener('submit', connect);
    $('mode-auto').addEventListener('click', () => { if (state && state.mode !== 'auto') setAuto(); });
    $('mode-manual').addEventListener('click', () => {
      if (state && state.mode !== 'manual') {
        const target = state.routing || state.active || (state.accounts[0] && state.accounts[0].id);
        if (target) pin(target);
        else toast('No account to pin yet.', true);
      }
    });
    $('history-toggle').addEventListener('click', () => { showAllHistory = !showAllHistory; renderHistory(); });
    $('refresh').addEventListener('click', () => act('refresh', {}, 'Checking quota for all accounts'));
    $('win-now').addEventListener('click', startAllNow);
    $('win-delay-form').addEventListener('submit', startDelayed);
    $('win-stagger-form').addEventListener('submit', startStaggered);
    $('win-off').addEventListener('click', turnOff);
    $('pause-form').addEventListener('submit', pauseNow);
    $('pause-form').addEventListener('input', () => { pauseDirty = true; });
    $('pause-auto').addEventListener('change', () => { $('pause-auto-minutes').disabled = !$('pause-auto').checked; });
    $('resume-go').addEventListener('click', resumeNow);
    $('slow-form').addEventListener('submit', slowDown);
    $('slow-form').addEventListener('input', () => { slowDirty = true; });
    $('slow-off').addEventListener('click', slowOff);
    $('prefs-form').addEventListener('submit', savePrefs);
    $('prefs-form').addEventListener('input', markDirty);
    $('f-switchback').addEventListener('change', syncSwitchBack);
    $('prefs-reset').addEventListener('click', () => { prefsDirty = false; fillPrefs(); });
    document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible' && state) refresh().catch(() => {}); });

    key = savedKey();
    if (!key) {
      showConnect('');
      return;
    }
    $('app').hidden = false; // render the shell at once; data fills in
    try {
      await refresh();
      schedule();
    } catch (err) {
      if (err.auth) {
        forgetKey();
        showConnect('');
      } else {
        $('notice').hidden = false;
        $('notice').textContent = `Could not load routing state: ${err.message}`;
        schedule();
      }
    }
  }

  document.addEventListener('DOMContentLoaded', boot);
})();
