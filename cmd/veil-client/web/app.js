'use strict';
const $ = id => document.getElementById(id);
let prevState = '', S = null, lastBytes = null, lastT = 0, dirty = false, importing = false;

async function api(method, path, body) {
  const r = await fetch('api' + path, { method, headers: { 'X-Veil': '1', 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined, credentials: 'same-origin' });
  let j = {}; try { j = await r.json(); } catch (e) {}
  if (!r.ok) { const e = new Error(j.error || r.statusText); e.data = j; throw e; }
  return j;
}
function toast(m) { const t = $('toast'); t.textContent = m; t.hidden = false; clearTimeout(toast.t); toast.t = setTimeout(() => t.hidden = true, 2600); }
function bytes(n) { const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0; while (n >= 1024 && i < 4) { n /= 1024; i++; } return (i ? n.toFixed(n < 10 ? 2 : 1) : Math.round(n)) + ' ' + u[i]; }
function dur(s) { s = Math.max(0, s | 0); const h = (s / 3600) | 0, m = ((s % 3600) / 60) | 0; return (h ? h + ':' + String(m).padStart(2, '0') : m) + ':' + String(s % 60).padStart(2, '0'); }
function timeLeft(sec) {
  sec = Math.max(0, sec | 0);
  const d = (sec / 86400) | 0, h = ((sec % 86400) / 3600) | 0, m = ((sec % 3600) / 60) | 0;
  if (d > 0) return d + 'd ' + h + 'h left';
  if (h > 0) return h + 'h ' + m + 'm left';
  if (m > 0) return m + 'm left';
  return 'less than a minute left';
}

function fillSettings(s) {
  $('fCarrier').value = s.carrier || 'tls'; $('fTransport').value = s.transport; $('fFrag').value = s.frag; $('fRotate').value = s.rotate_minutes; $('fDNS').value = s.tun_dns;
  $('fCover').checked = s.cover;
  $('fNetBounce').checked = s.net_bounce;
  if (document.activeElement !== $('fAdminURL')) $('fAdminURL').value = s.admin_url || '';
  $('fAdminTrust').checked = !!s.admin_trust_any;
}
function readSettings() {
  return { carrier: $('fCarrier').value, transport: $('fTransport').value, frag: $('fFrag').value, rotate_minutes: parseInt($('fRotate').value, 10) || 20, tun_dns: $('fDNS').value, cover: $('fCover').checked, net_bounce: $('fNetBounce').checked, admin_url: $('fAdminURL').value, admin_trust_any: $('fAdminTrust').checked };
}

function render() {
  const e = S.engine, has = !!S.profile;
  $('main').hidden = !has || importing; $('onboard').hidden = has && !importing;
  $('cancelImport').hidden = !has;
  $('onBig').textContent = has ? 'Add a server' : 'Add your key';
  if (!has) { return; }
  const st = e.state, running = st === 'connecting' || st === 'connected';
  $('power').disabled = st === 'disconnecting';
  const pill = $('pill'); pill.className = 'pill ' + st;
  $('pillText').textContent = { disconnected: 'Disconnected', connecting: 'Connecting…', connected: e.carrier ? 'Connected' : 'Reconnecting…', disconnecting: 'Disconnecting…', error: 'Error' }[st];
  $('power').className = 'power ' + st;
  $('big').textContent = { disconnected: 'Disconnected', connecting: 'Connecting…', connected: e.carrier ? 'Connected' : 'Reconnecting…', disconnecting: 'Disconnecting…', error: 'Connection failed' }[st];
  $('sub').textContent = st === 'connected' ? 'Your whole computer is protected' : st === 'connecting' ? 'Contacting server…' : st === 'disconnecting' ? 'Removing the VPN adapter and routes… your traffic is going direct again' : prevState === 'disconnecting' || prevState === 'connected' ? 'Not protected - tap to connect' : 'Tap to connect';
  $('err').hidden = !(st === 'error' && e.error); $('err').textContent = e.error || '';
  $('adminNote').hidden = !((S.windows || S.mac) && !S.admin);
  const sel = $('srv'), sig = S.profiles.map(p => p.id + p.name).join('|') + S.selected;
  if (sel.dataset.sig !== sig) { sel.textContent = ''; S.profiles.forEach(p => { const o = document.createElement('option'); o.value = p.id; o.textContent = p.name + ' - ' + p.server; sel.appendChild(o); }); sel.value = S.selected; sel.dataset.sig = sig; if (document.activeElement !== $('nameIn')) $('nameIn').value = S.profile.name; }
  sel.disabled = e.state === 'connecting' || e.state === 'connected' || e.state === 'disconnecting';
  $('remove').disabled = sel.disabled && S.active === S.selected;
  $('kServer').textContent = S.profile.server + (S.profile.sni && S.profile.sni !== S.profile.host ? '  (' + S.profile.sni + ')' : '');
  $('kCert').textContent = S.profile.local_cert ? 'pinned local certificate' : 'public (Let\'s Encrypt)';
  $('sServer').textContent = S.profile.host;
  if (!$('adv').open || !dirty) { if (!dirty) fillSettings(S.settings); }
  // stats
  const now = Date.now() / 1000;
  if (lastBytes && running && now > lastT) { const dt = now - lastT; $('sDown').textContent = bytes(Math.max(0, (e.down - lastBytes.down) / dt)) + '/s'; $('sUp').textContent = bytes(Math.max(0, (e.up - lastBytes.up) / dt)) + '/s'; }
  if (!running) { $('sDown').textContent = '0 B/s'; $('sUp').textContent = '0 B/s'; }
  lastBytes = { up: e.up, down: e.down }; lastT = now;
  if (prevState === 'disconnecting' && st === 'disconnected') toast('Disconnected - traffic is no longer protected');
  prevState = st;
  $('tDown').textContent = bytes(e.down) + ' total'; $('tUp').textContent = bytes(e.up) + ' total';
  $('sTime').textContent = running && e.since ? dur(Date.now() / 1000 - e.since) : '–';

  // account limits (time/data/speed) - only shown for dimensions the server actually limits
  const lim = e.limits, now2 = Date.now() / 1000;
  const hasExpiry = lim && lim.expires_at > 0;
  const hasCap = lim && lim.data_cap > 0;
  const hasRate = lim && lim.rate_mbps > 0;
  $('limitsCard').hidden = !(hasExpiry || hasCap || hasRate);
  $('lExpiryRow').hidden = !hasExpiry;
  if (hasExpiry) $('lExpiry').textContent = timeLeft(lim.expires_at - now2);
  $('lDataRow').hidden = !hasCap;
  if (hasCap) {
    const used = (lim.used_up || 0) + (lim.used_down || 0), pct = Math.min(100, used / lim.data_cap * 100);
    $('lData').textContent = bytes(used) + ' of ' + bytes(lim.data_cap) + ' (' + pct.toFixed(pct < 10 ? 1 : 0) + '%)';
    const fill = $('lDataBar'); fill.style.width = pct + '%';
    fill.className = 'barfill' + (pct >= 90 ? ' crit' : pct >= 75 ? ' warn' : '');
  }
  $('lRateRow').hidden = !hasRate;
  if (hasRate) $('lRate').textContent = lim.rate_mbps + ' Mbps (up and down)';
}

async function refresh() {
  try { S = await api('GET', '/state'); render(); } catch (e) { $('pillText').textContent = 'App closed'; }
  if ($('logCard').open) { try { $('log').textContent = (await api('GET', '/logs')).join('\n'); } catch (e) {} }
}

$('power').onclick = async () => {
  if (!S || !S.profile) return;
  const st = S.engine.state;
  try {
    if (st === 'connecting' || st === 'connected') { $('pillText').textContent = 'Disconnecting…'; $('pill').className = 'pill disconnecting'; $('big').textContent = 'Disconnecting…'; $('power').className = 'power disconnecting'; $('power').disabled = true; await api('POST', '/disconnect'); }
    else {
      await api('POST', '/settings', readSettings()); dirty = false;
      await api('POST', '/connect');
    }
  } catch (e) { toast(e.message); }
  refresh();
};
$('elevate').onclick = async () => { try { await api('POST', '/elevate'); $('sub').textContent = 'Restarting with Administrator rights…'; } catch (e) { toast(e.message); } };
document.querySelectorAll('#adv input,#adv select').forEach(el => el.addEventListener('input', () => dirty = true));
document.querySelectorAll('#mngCard input').forEach(el => el.addEventListener('input', () => dirty = true));
$('save').onclick = async () => { try { await api('POST', '/settings', readSettings()); dirty = false; toast('Saved'); refresh(); } catch (e) { toast(e.message); } };
$('replace').onclick = $('addSrv').onclick = () => { importing = true; $('keyText').value = ''; $('keyName').value = ''; $('impErr').hidden = true; render(); };
$('cancelImport').onclick = () => { importing = false; render(); };
$('remove').onclick = async () => { if (!confirm('Remove "' + S.profile.name + '" from this computer?')) return; try { await api('POST', '/remove', { id: S.selected }); } catch (e) { toast(e.message); } importing = false; refresh(); };
$('srv').onchange = async () => { dirty = false; try { await api('POST', '/select', { id: $('srv').value }); } catch (e) { toast(e.message); } $('srv').dataset.sig = ''; refresh(); };
$('rename').onclick = async () => { try { await api('POST', '/rename', { id: S.selected, name: $('nameIn').value }); toast('Renamed'); $('srv').dataset.sig = ''; refresh(); } catch (e) { toast(e.message); } };
$('pick').onclick = () => $('file').click();
$('file').onchange = async () => { const f = $('file').files[0]; if (f) $('keyText').value = await f.text(); };
$('doImport').onclick = async () => {
  try { await api('POST', '/import', { text: $('keyText').value, name: $('keyName').value }); importing = false; $('impErr').hidden = true; toast('Server added'); refresh(); }
  catch (e) { $('impErr').textContent = e.message; $('impErr').hidden = false; }
};
$('refreshLimits').onclick = async () => { const b = $('refreshLimits'); b.disabled = true; b.textContent = 'Refreshing…'; try { await api('POST', '/refresh-limits'); await refresh(); toast('Limits updated'); } catch (e) { toast(e.message); } b.disabled = false; b.textContent = 'Refresh limits'; };
$('openAdmin').onclick = async () => { try { await api('POST', '/settings', readSettings()); const r = await api('POST', '/manage/open'); dirty = false; window.open(r.url, '_blank'); } catch (e) { toast(e.message); } };
$('repair').onclick = async () => { try { await api('POST', '/repair'); toast('Network settings cleaned up'); } catch (e) { toast(e.message); } };
$('quit').onclick = async () => { try { await api('POST', '/quit'); } catch (e) {} document.body.innerHTML = '<p style="padding:40px;text-align:center;color:#8d93ab">Veil has quit. You can close this window.</p>'; setTimeout(() => window.close(), 500); };
$('logCard').addEventListener('toggle', refresh);

refresh(); setInterval(refresh, 1000);
