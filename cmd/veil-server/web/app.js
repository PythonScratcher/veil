'use strict';
const $app = document.getElementById('app');
let csrf = '', users = [], info = {}, timer = null, modalOpen = false;

// ---- tiny DOM helper (no innerHTML anywhere: names/notes are untrusted text)
function h(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === 'class') e.className = v;
    else if (k === 'style') e.style.cssText = v; // CSSOM is CSP-safe; style="" attributes are not
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else if (v === true) e.setAttribute(k, '');
    else if (v !== false && v != null) e.setAttribute(k, v);
  }
  for (const c of kids.flat()) if (c != null && c !== false) e.append(c.nodeType ? c : document.createTextNode(c));
  return e;
}
const GB = 1e9, MB = 1e6, TB = 1e12;
function fmtBytes(n) {
  if (n >= TB) return (n / TB).toFixed(2) + ' TB';
  if (n >= GB) return (n / GB).toFixed(2) + ' GB';
  if (n >= MB) return (n / MB).toFixed(1) + ' MB';
  if (n >= 1e3) return (n / 1e3).toFixed(0) + ' KB';
  return n + ' B';
}
function fmtSpeed(bps) { return bps < 1000 ? '0 b/s' : (bps * 8 >= 1e6 ? (bps * 8 / 1e6).toFixed(1) + ' Mb/s' : (bps * 8 / 1e3).toFixed(0) + ' kb/s'); }
function fmtLeft(ts) {
  if (!ts) return 'never expires';
  const s = ts - Date.now() / 1000;
  if (s <= 0) return 'expired ' + fmtDur(-s) + ' ago';
  return fmtDur(s) + ' left';
}
function fmtDur(s) {
  const d = Math.floor(s / 86400), hh = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60);
  if (d >= 2) return d + ' days';
  if (d >= 1) return d + 'd ' + hh + 'h';
  if (hh >= 1) return hh + 'h ' + m + 'm';
  return Math.max(1, m) + ' min';
}
function ago(ts) { if (!ts) return 'never'; const s = Date.now() / 1000 - ts; return s < 20 ? 'just now' : fmtDur(s) + ' ago'; }
function toast(msg, bad) {
  const t = h('div', { class: 'toast' + (bad ? ' err' : '') }, msg);
  document.body.append(t); setTimeout(() => t.remove(), 2800);
}
async function api(method, path, body) {
  const r = await fetch('api' + path, {
    method, credentials: 'same-origin',
    headers: Object.assign({ 'Content-Type': 'application/json' }, csrf ? { 'X-CSRF-Token': csrf } : {}),
    body: body ? JSON.stringify(body) : undefined,
  });
  if (r.status === 401 && path !== '/login') { showLogin(); throw new Error('Please log in'); }
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}


// ---- usage charts (hand-rolled SVG, no libraries)
const RANGES = ['1h', '6h', '24h', '7d', '30d'];
const SVGNS = 'http://www.w3.org/2000/svg';
function sv(tag, attrs, ...kids) {
  const e = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs || {})) e.setAttribute(k, v);
  for (const c of kids) if (c != null) e.append(c.nodeType ? c : document.createTextNode(c));
  return e;
}
function niceMax(v) {
  if (v <= 0) return 1e6;
  const e = Math.pow(10, Math.floor(Math.log10(v))), f = v / e;
  return (f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10) * e;
}
function fmtT(t, step) {
  const d = new Date(t * 1000), p = n => String(n).padStart(2, '0');
  const day = d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  if (step < 3600) return p(d.getHours()) + ':' + p(d.getMinutes());
  if (step === 3600) return p(d.getHours()) + ':00';
  if (step < 86400) return day + ' ' + p(d.getHours()) + ':00';
  return day;
}
function chartSVG(points, step, width) {
  const W = Math.max(300, Math.round(width || 720)), H = 230, L = 62, R = 8, T = 10, B = 26, n = points.length;
  const max = niceMax(Math.max(0, ...points.map(p => p.up + p.down)));
  const ph = H - T - B, bw = (W - L - R) / n, gap = Math.min(2, bw * 0.2);
  const svg = sv('svg', { viewBox: `0 0 ${W} ${H}`, width: W, height: H, role: 'img', 'aria-label': 'usage chart' });
  for (let i = 0; i <= 4; i++) {
    const y = T + ph - ph * i / 4;
    svg.append(sv('line', { x1: L, x2: W - R, y1: y, y2: y, class: 'grid' }),
      sv('text', { x: L - 8, y: y + 4, class: 'ax', 'text-anchor': 'end' }, i ? fmtBytes(max * i / 4) : '0'));
  }
  points.forEach((p, i) => {
    const x = L + i * bw + gap / 2, w = Math.max(1, bw - gap);
    const hd = p.down / max * ph, hu = p.up / max * ph;
    const g = sv('g', { class: 'bar' }, sv('title', {}, fmtT(p.t, step) + '  ↓ ' + fmtBytes(p.down) + '  ↑ ' + fmtBytes(p.up)));
    g.append(sv('rect', { x, y: T, width: w, height: ph, class: 'hit' }));
    if (hd > 0) g.append(sv('rect', { x, y: T + ph - hd, width: w, height: hd, class: 'bd' }));
    if (hu > 0) g.append(sv('rect', { x, y: T + ph - hd - hu, width: w, height: hu, class: 'bu' }));
    svg.append(g);
  });
  for (const i of [0, Math.floor(n / 2), n - 1]) {
    const anchor = i === 0 ? 'start' : i === n - 1 ? 'end' : 'middle';
    const x = i === 0 ? L : i === n - 1 ? W - R : L + (i + .5) * bw;
    svg.append(sv('text', { x, y: H - 7, class: 'ax', 'text-anchor': anchor }, fmtT(points[i].t, step)));
  }
  return svg;
}
function usageCard(id, title) {
  let range = '24h', timer;
  const tabs = h('div', { class: 'tabs' });
  const sum = h('div', { class: 'small' });
  const chart = h('div', { class: 'chart' });
  const top = id === 'total' ? h('div', { class: 'top' }) : null;
  const legend = h('div', { class: 'legend' }, h('span', { class: 'dot d' }), 'Download', h('span', { class: 'dot u' }), 'Upload');
  const el = h('div', { class: 'card' }, h('div', { class: 'cardhead' }, h('div', {}, h('div', { class: 'ctitle' }, title), sum), tabs), chart, legend, top);
  const drawTabs = () => tabs.replaceChildren(...RANGES.map(r => h('button', { class: 'tab' + (r === range ? ' on' : ''), onclick: () => { range = r; drawTabs(); load(); } }, r)));
  let last = null, lastW = 0;
  const drawChart = () => { if (!last) return; lastW = chart.clientWidth; chart.replaceChildren(chartSVG(last.points, last.step, lastW)); };
  if (window.ResizeObserver) new ResizeObserver(() => { if (last && Math.abs(chart.clientWidth - lastW) > 16) drawChart(); }).observe(chart);
  async function load() {
    if (!el.isConnected && timer) { clearInterval(timer); return; }
    try {
      const d = await api('GET', '/history?id=' + encodeURIComponent(id) + '&range=' + range);
      last = d; drawChart();
      const up = d.points.reduce((a, p) => a + p.up, 0), dn = d.points.reduce((a, p) => a + p.down, 0);
      sum.textContent = 'Last ' + range + ': ↓ ' + fmtBytes(dn) + '   ↑ ' + fmtBytes(up) + '   (' + fmtBytes(up + dn) + ' total)';
      if (top) {
        const rows = d.per_key || [], mx = Math.max(1, ...rows.map(r => r.up + r.down));
        top.replaceChildren(rows.length ? h('div', { class: 'lbl', style: 'margin:12px 0 6px' }, 'By key') : null,
          ...rows.slice(0, 12).map(r => h('div', { class: 'toprow' },
            h('div', { class: 'tn' }, r.name),
            h('div', { class: 'bar' }, h('i', { style: 'width:' + ((r.up + r.down) / mx * 100) + '%' })),
            h('div', { class: 'tv' }, fmtBytes(r.up + r.down)))));
      }
    } catch (e) { if (e.message !== 'Please log in') sum.textContent = e.message; }
  }
  drawTabs(); load(); timer = setInterval(load, 15000);
  return { el, stop: () => clearInterval(timer) };
}
let totalCard = null;
function usageDialog(u) {
  modal(close => h('div', {}, usageCard(u.id, 'Usage — ' + u.name).el,
    h('div', { class: 'foot' }, h('div', { class: 'grp' }), h('div', { class: 'grp' }, h('button', { class: 'btn primary', onclick: close }, 'Done')))));
}

// ---- change admin password
function passwordDialog() {
  const cur = h('input', { type: 'password', autocomplete: 'current-password' });
  const n1 = h('input', { type: 'password', autocomplete: 'new-password' });
  const n2 = h('input', { type: 'password', autocomplete: 'new-password' });
  const err = h('div', { class: 'err', style: 'margin-top:8px;min-height:1.2em' });
  modal(close => {
    const go = async () => {
      err.textContent = '';
      if (n1.value !== n2.value) { err.textContent = 'New passwords do not match'; return; }
      try { await api('POST', '/password', { current: cur.value, new: n1.value }); close(); toast('Password changed'); }
      catch (e) { err.textContent = e.message; }
    };
    return h('div', {}, h('h2', {}, 'Change admin password'),
      field('Current password', cur), field('New password (min. 10 characters)', n1), field('Repeat new password', n2), err,
      h('div', { class: 'foot' }, h('div', { class: 'grp' }),
        h('div', { class: 'grp' }, h('button', { class: 'btn', onclick: close }, 'Cancel'), h('button', { class: 'btn primary', onclick: go }, 'Change password'))));
  });
}

// ---- login
function showLogin(msg) {
  clearInterval(timer); csrf = ''; if (totalCard) { totalCard.stop(); totalCard = null; }
  const pw = h('input', { class: 'in', type: 'password', placeholder: 'Admin password', autocomplete: 'current-password', autofocus: true });
  const err = h('div', { class: 'err' }, msg || '');
  const go = async () => {
    try { const r = await api('POST', '/login', { password: pw.value }); csrf = r.csrf; start(); }
    catch (e) { err.textContent = e.message; }
  };
  pw.addEventListener('keydown', e => { if (e.key === 'Enter') go(); });
  $app.replaceChildren(h('div', { class: 'login' },
    h('h1', {}, 'Veil'), h('p', {}, 'Server admin'), pw,
    h('div', { style: 'margin-top:12px' }, h('button', { class: 'btn primary', style: 'width:100%', onclick: go }, 'Log in')), err));
  pw.focus();
}

// ---- main view
async function start() {
  await refresh();
  clearInterval(timer);
  timer = setInterval(() => { if (!document.hidden && !modalOpen) refresh(); else if (!document.hidden) refreshQuiet(); }, 3000);
}
async function refreshQuiet() { try { [info, users] = await Promise.all([api('GET', '/server'), api('GET', '/users')]); } catch (e) {} }
async function refresh() {
  try { [info, users] = await Promise.all([api('GET', '/server'), api('GET', '/users')]); render(); } catch (e) { if (e.message !== 'Please log in') toast(e.message, true); }
}
function render() {
  if (!totalCard) totalCard = usageCard('total', 'Traffic — all keys');
  const wrap = h('div', { class: 'wrap' },
    h('header', { class: 'top' },
      h('h1', {}, 'Veil', h('small', {}, info.domain + ':' + info.port)),
      h('button', { class: 'btn primary', onclick: () => editDialog(null) }, '+ New key'),
      h('button', { class: 'btn', onclick: passwordDialog }, 'Password'),
      h('button', { class: 'btn', onclick: async () => { await api('POST', '/logout').catch(() => {}); showLogin(); } }, 'Log out')),
    h('div', { class: 'stats' },
      stat('Keys', info.users, ''),
      stat('Online', info.conns, 'open tunnels'),
      stat('Speed', '↓ ' + fmtSpeed(info.speed_down), '↑ ' + fmtSpeed(info.speed_up)),
      stat('Traffic', fmtBytes(info.total_down + info.total_up), '↓ ' + fmtBytes(info.total_down) + '  ↑ ' + fmtBytes(info.total_up))),
    totalCard.el,
    users.length ? h('div', { class: 'list', style: 'margin-top:14px' }, users.map(rowFor)) :
      h('div', { class: 'empty' }, 'No keys yet. Click “New key” to create one for a client.'));
  $app.replaceChildren(wrap);
}
const stat = (k, v, s) => h('div', { class: 'stat' }, h('div', { class: 'k' }, k), h('div', { class: 'v' }, String(v)), h('div', { class: 's' }, s));

function rowFor(u) {
  const used = u.used_up + u.used_down;
  const pct = u.data_cap_bytes ? Math.min(100, used / u.data_cap_bytes * 100) : 0;
  return h('div', { class: 'row' + (u.status === 'disabled' ? ' off' : '') },
    h('div', {},
      h('div', { class: 'name' }, u.name, h('span', { class: 'badge ' + u.status }, u.status)),
      h('div', { class: 'note' }, u.note || ' '),
      h('div', { class: 'small' }, u.conns ? '● online · ' + u.ips.join(', ') : 'last seen ' + ago(u.last_seen))),
    h('div', {},
      h('div', { class: 'lbl' }, 'Data'),
      u.data_cap_bytes ? h('div', { class: 'bar' }, h('i', { class: pct > 90 ? 'hot' : '', style: 'width:' + pct + '%' })) : null,
      h('div', { class: 'small' }, fmtBytes(used) + (u.data_cap_bytes ? ' / ' + fmtBytes(u.data_cap_bytes) : ' · unlimited') + (u.reset_days ? ' · resets every ' + u.reset_days + 'd' : ''))),
    h('div', {},
      h('div', { class: 'lbl' }, 'Time'), h('div', { class: 'small', style: 'color:var(--text)' }, fmtLeft(u.expires_at)),
      h('div', { class: 'small' }, u.expires_at ? new Date(u.expires_at * 1000).toLocaleString() : ' ')),
    h('div', {},
      h('div', { class: 'lbl' }, 'Speed'),
      h('div', { class: 'small', style: 'color:var(--text)' }, '↓ ' + fmtSpeed(u.speed_down) + ' ↑ ' + fmtSpeed(u.speed_up)),
      h('div', { class: 'small' }, (u.rate_mbps ? 'cap ' + u.rate_mbps + ' Mb/s' : 'no speed cap') + (u.max_devices ? ' · ' + u.max_devices + ' device' + (u.max_devices > 1 ? 's' : '') + ' max' : ''))),
    h('div', { class: 'actions' },
      h('button', { class: 'btn sm', onclick: () => usageDialog(u) }, 'Usage'),
      h('button', { class: 'btn sm', onclick: () => configDialog(u) }, 'Config'),
      h('button', { class: 'btn sm', onclick: () => editDialog(u) }, 'Edit')));
}

// ---- dialogs
function modal(content) {
  modalOpen = true;
  const close = () => { scrim.remove(); modalOpen = false; render(); };
  const scrim = h('div', { class: 'scrim', onmousedown: e => { if (e.target === scrim) close(); } }, h('div', { class: 'modal' }, content(close)));
  document.body.append(scrim);
  return close;
}
const field = (label, ...kids) => h('div', { class: 'f' }, h('label', { class: 't' }, label), ...kids);

function toLocalInput(ts) {
  const d = new Date(ts * 1000), p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' + p(d.getHours()) + ':' + p(d.getMinutes());
}

function editDialog(u0) {
  const isNew = !u0;
  let u = u0 ? JSON.parse(JSON.stringify(u0)) : { name: '', note: '', enabled: true, expires_at: 0, data_cap_bytes: 0, rate_mbps: 0, max_devices: 0, reset_days: 0, used_up: 0, used_down: 0 };
  modal(close => {
    const name = h('input', { class: 'in', value: u.name, placeholder: 'e.g. alice-laptop', maxlength: 64 });
    const note = h('input', { class: 'in', value: u.note || '', placeholder: 'optional note', maxlength: 300 });
    const enabled = h('input', { type: 'checkbox' }); enabled.checked = u.enabled;

    const expNever = h('input', { type: 'checkbox' });
    const expDate = h('input', { class: 'in', type: 'datetime-local' });
    const setExp = () => { expNever.checked = !u.expires_at; expDate.disabled = expNever.checked; if (u.expires_at) expDate.value = toLocalInput(u.expires_at); else expDate.value = ''; expInfo.textContent = fmtLeft(u.expires_at); };
    const expInfo = h('span', { class: 'small' });
    expNever.addEventListener('change', () => { expDate.disabled = expNever.checked; if (!expNever.checked && !expDate.value) expDate.value = toLocalInput(Date.now() / 1000 + 30 * 86400); });
    const addDays = d => async () => {
      if (isNew) { const base = Math.max(u.expires_at || 0, Date.now() / 1000); u.expires_at = Math.floor(base + d * 86400); setExp(); return; }
      try { u = Object.assign(u, await api('PATCH', '/users/' + u.id, { add_days: d })); setExp(); toast('Time updated'); } catch (e) { toast(e.message, true); }
    };

    const capUnlim = h('input', { type: 'checkbox' });
    const capNum = h('input', { class: 'in', type: 'number', min: 0, step: 'any' });
    const capUnit = h('select', { class: 'in', style: 'max-width:90px' }, ['MB', 'GB', 'TB'].map(x => h('option', { value: x }, x)));
    const mult = () => ({ MB, GB, TB }[capUnit.value]);
    const usage = h('div', { class: 'small' });
    const setCap = () => {
      capUnlim.checked = !u.data_cap_bytes; capNum.disabled = capUnit.disabled = capUnlim.checked;
      const c = u.data_cap_bytes; capUnit.value = c >= TB ? 'TB' : c >= GB || !c ? 'GB' : 'MB';
      capNum.value = c ? +(c / mult()).toFixed(3) : '';
      usage.textContent = isNew ? '' : 'Used so far: ' + fmtBytes(u.used_up + u.used_down) + ' (↓ ' + fmtBytes(u.used_down) + ' ↑ ' + fmtBytes(u.used_up) + ')';
    };
    capUnlim.addEventListener('change', () => { capNum.disabled = capUnit.disabled = capUnlim.checked; if (!capUnlim.checked && !capNum.value) capNum.value = 10; });
    const addData = n => async () => {
      if (isNew) { u.data_cap_bytes = (u.data_cap_bytes || 0) + n; setCap(); return; }
      try { u = Object.assign(u, await api('PATCH', '/users/' + u.id, { add_data_bytes: n })); setCap(); toast('Data cap updated'); } catch (e) { toast(e.message, true); }
    };

    const rateUnlim = h('input', { type: 'checkbox' });
    const rate = h('input', { class: 'in', type: 'number', min: 0, step: 'any' });
    const setRate = () => { rateUnlim.checked = !u.rate_mbps; rate.disabled = rateUnlim.checked; rate.value = u.rate_mbps || ''; };
    rateUnlim.addEventListener('change', () => { rate.disabled = rateUnlim.checked; if (!rateUnlim.checked && !rate.value) rate.value = 20; });
    const dev = h('input', { class: 'in', type: 'number', min: 0, value: u.max_devices || 0 });
    const reset = h('input', { class: 'in', type: 'number', min: 0, value: u.reset_days || 0 });

    const body = () => {
      const o = { name: name.value.trim(), note: note.value, enabled: enabled.checked,
        expires_at: expNever.checked ? 0 : Math.floor(new Date(expDate.value).getTime() / 1000) || 0,
        data_cap_bytes: capUnlim.checked ? 0 : Math.round(parseFloat(capNum.value || '0') * mult()),
        rate_mbps: rateUnlim.checked ? 0 : parseFloat(rate.value || '0'),
        max_devices: parseInt(dev.value || '0', 10), reset_days: parseInt(reset.value || '0', 10) };
      return o;
    };
    const save = async () => {
      try {
        const o = body(); if (!o.name) { toast('Give the key a name', true); return; }
        if (!expNever.checked && !o.expires_at) { toast('Pick an expiry date or tick “never”', true); return; }
        const r = isNew ? await api('POST', '/users', o) : await api('PATCH', '/users/' + u.id, o);
        toast(isNew ? 'Key created' : 'Saved'); close();
        if (isNew) setTimeout(() => configDialog(r), 50);
      } catch (e) { toast(e.message, true); }
    };

    setExp(); setCap(); setRate();
    const chip = (t, f, cls) => h('button', { class: 'btn sm ' + (cls || ''), type: 'button', onclick: f }, t);
    return h('div', {},
      h('h2', {}, isNew ? 'New key' : 'Edit “' + u0.name + '”'),
      h('div', { class: 'sub' }, isNew ? 'Create an access key for one client. You can change any limit later.' : 'Changes apply instantly, including to open connections.'),
      field('Name', name), field('Note', note),
      h('div', { class: 'f' }, h('label', { class: 'sw' }, enabled, 'Enabled')),
      field('Expiry', h('div', { class: 'inline' }, h('label', { class: 'sw' }, expNever, 'Never'), expDate, expInfo),
        h('div', { class: 'chips' }, h('span', { class: 'small', style: 'align-self:center' }, 'Add time:'),
          chip('+1 day', addDays(1)), chip('+7 days', addDays(7)), chip('+30 days', addDays(30)), chip('+90 days', addDays(90)), chip('+1 year', addDays(365)))),
      field('Data cap (up + down)', h('div', { class: 'inline' }, h('label', { class: 'sw' }, capUnlim, 'Unlimited'), capNum, capUnit), usage,
        h('div', { class: 'chips' }, h('span', { class: 'small', style: 'align-self:center' }, 'Top up:'),
          chip('+1 GB', addData(GB)), chip('+10 GB', addData(10 * GB)), chip('+100 GB', addData(100 * GB)),
          isNew ? null : chip('Reset usage', async () => { if (!confirm('Zero the usage counter for this key?')) return; try { u = Object.assign(u, await api('POST', '/users/' + u.id + '/reset-usage')); setCap(); toast('Usage reset'); } catch (e) { toast(e.message, true); } }))),
      field('Bandwidth cap (each direction)', h('div', { class: 'inline' }, h('label', { class: 'sw' }, rateUnlim, 'Unlimited'), rate, h('span', { class: 'small' }, 'Mb/s'))),
      h('div', { class: 'inline', style: 'align-items:flex-start' },
        h('div', { class: 'f', style: 'flex:1;min-width:150px' }, h('label', { class: 't' }, 'Max devices (0 = no limit)'), dev),
        h('div', { class: 'f', style: 'flex:1;min-width:150px' }, h('label', { class: 't' }, 'Auto-reset usage every N days (0 = never)'), reset)),
      h('div', { class: 'foot' },
        h('div', { class: 'grp' }, isNew ? null : [
          chip('Config', () => { close(); configDialog(u); }),
          chip('New key…', async () => { if (!confirm('Generate a new secret for this key? The old client config stops working immediately.')) return; try { u = Object.assign(u, await api('POST', '/users/' + u.id + '/regen-key')); toast('New secret generated'); close(); configDialog(u); } catch (e) { toast(e.message, true); } }),
          chip('Delete', async () => { if (!confirm('Delete “' + u.name + '”? Its client will be cut off.')) return; try { await api('DELETE', '/users/' + u.id); toast('Deleted'); close(); } catch (e) { toast(e.message, true); } }, 'danger')]),
        h('div', { class: 'grp' }, chip('Cancel', close), chip(isNew ? 'Create' : 'Save', save, 'primary'))));
  });
}

async function configDialog(u) {
  let text = '';
  try { const r = await fetch('api/users/' + u.id + '/config', { credentials: 'same-origin' }); text = await r.text(); } catch (e) { toast(e.message, true); return; }
  modal(close => h('div', {},
    h('h2', {}, 'Client config — ' + u.name),
    h('div', { class: 'sub' }, 'Save as client.json next to veil-client.exe on the client machine. Contains the secret key: send it privately.'),
    h('div', { class: 'small', style: 'margin:10px 0 4px' }, 'Scan with the Veil Android app (Add server → Scan QR):'),
    h('img', { src: 'api/users/' + u.id + '/qr.png?t=' + Date.now(), alt: 'QR code', style: 'width:220px;height:220px;background:#fff;padding:8px;border-radius:8px;display:block;margin:0 auto 12px' }),
    h('pre', { class: 'cfg' }, text),
    h('div', { class: 'small', style: 'margin-top:10px' }, 'Run: veil-client.exe -c client.json    (add -tun in an Administrator shell for full-device VPN)'),
    h('div', { class: 'foot' }, h('div', { class: 'grp' }),
      h('div', { class: 'grp' },
        h('button', { class: 'btn', onclick: async () => { try { await navigator.clipboard.writeText(text); toast('Copied'); } catch (e) { toast('Select the text and copy manually', true); } } }, 'Copy'),
        h('a', { class: 'btn', href: 'api/users/' + u.id + '/config?download=1', download: 'client.json', style: 'text-decoration:none' }, 'Download'),
        h('button', { class: 'btn primary', onclick: close }, 'Done')))));
}

// ---- boot
(async () => {
  try { const r = await api('GET', '/session'); csrf = r.csrf; start(); } catch (e) { showLogin(); }
})();
