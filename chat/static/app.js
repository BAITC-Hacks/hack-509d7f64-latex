'use strict';
// Neonic Samurais — chat client: mic PCM over WebSocket, live partial transcript, reply text + voice, trace panel.

const $ = (s, r = document) => r.querySelector(s);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const store = { get(k, d) { try { const v = localStorage.getItem('vs.' + k); return v === null ? d : JSON.parse(v); } catch { return d; } },
                set(k, v) { try { localStorage.setItem('vs.' + k, JSON.stringify(v)); } catch { /* ignore */ } } };

const S = { sid: null, ws: null, wsRetry: null, recording: false, busy: false, ctx: null, stream: null, node: null,
            t0: 0, timer: null, speechMs: 0, silenceSince: null, lastTick: 0, pending: null, lastBot: null, turn: null, ready: false };
const chat = $('#chat'), live = $('#live'), liveText = $('#liveText'), level = $('#level'), timer = $('#timer');
const mic = $('#mic'), hint = $('#hint'), handsFree = $('#handsFree'), autoplay = $('#autoplay');

// ---------------------------------------------------------------- answer language (auto = most used in this conversation)
const LANG_NAMES = { ru: 'Русский', kk: 'Қазақша' };
const langPref = () => ($('input[name="replyLang"]:checked') || {}).value || 'auto';
function langNote(m) {
  const note = $('#langNote'), pref = langPref();
  if (pref !== 'auto') { note.innerHTML = `always <b>${LANG_NAMES[pref]}</b>`; return; }
  const c = m && m.lang_counts;
  if (!c) { note.textContent = 'follows your most-used language'; return; }
  const lead = c.kk > c.ru ? 'kk' : c.ru > c.kk ? 'ru' : null;  // tie: the server uses the latest non-mixed turn
  note.innerHTML = `→ <b>${lead ? LANG_NAMES[lead] : 'latest turn'}</b> <span class="dim">(${c.kk} kk / ${c.ru} ru)</span>`;
}
document.querySelectorAll('input[name="replyLang"]').forEach((r) => {
  r.checked = r.value === store.get('replyLang', 'auto');
  r.onchange = () => { store.set('replyLang', langPref()); langNote(S.turn && S.turn.reply); };
});

// ---------------------------------------------------------------- status crests
function crest(id, cls, text) { const el = $(id); el.className = 'crest ' + cls; if (text) el.textContent = text; }
async function health() {
  try {
    const h = await (await fetch('/health')).json();
    crest('#crestStt', h.stt.ok ? 'ok' : 'bad', 'STT');
    crest('#crestTts', h.tts.ok ? 'ok' : 'bad', 'TTS');
    const r = h.router;
    if (r.mode !== 'mock' && r.url && r.remote_reachable) crest('#crestRouter', 'ok', 'ROUTER · go');
    else if (r.mode === 'remote') crest('#crestRouter', 'bad', 'ROUTER · down');
    else crest('#crestRouter', 'mock', 'ROUTER · mock');
    $('#crestRouter').title = r.url ? `${r.mode}: ${r.url}${r.last_error ? '\n' + r.last_error : ''}` : 'no ROUTER_URL set: keyword mock over scenarios.json';
  } catch { ['#crestStt', '#crestTts', '#crestRouter'].forEach((id) => crest(id, 'bad')); }
}

// ---------------------------------------------------------------- session + websocket
async function connect() {
  try {
    const s = await (await fetch('/api/session', { method: 'POST' })).json();
    S.sid = s.session_id;
  } catch (e) { notice('cannot create session: ' + e.message, true); return; }
  openWs();
}
function openWs() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = new WebSocket(`${proto}://${location.host}/ws?session_id=${encodeURIComponent(S.sid)}`);
  ws.binaryType = 'arraybuffer';
  ws.onopen = () => { crest('#crestWs', 'ok', 'LINK'); S.ready = true; mic.disabled = false; };
  ws.onclose = () => { crest('#crestWs', 'bad', 'LINK'); S.ready = false; mic.disabled = true; if (S.recording) stopRecording(true); S.wsRetry = setTimeout(openWs, 2000); };
  ws.onerror = () => ws.close();
  ws.onmessage = (e) => { try { handle(JSON.parse(e.data)); } catch (err) { console.error(err); } };
  S.ws = ws;
}
const send = (obj) => { if (S.ws && S.ws.readyState === 1) S.ws.send(JSON.stringify(obj)); };

// ---------------------------------------------------------------- chat rendering
function scrollDown() { chat.scrollTop = chat.scrollHeight; }
function addUser(text, meta = '') {
  const el = document.createElement('div');
  el.className = 'msg user';
  el.innerHTML = `<div class="seal">客</div><div class="paper"><p>${esc(text)}</p>${meta ? `<span class="meta">${meta}</span>` : ''}</div>`;
  chat.appendChild(el); scrollDown(); return el;
}
function addBotPending() {
  const el = document.createElement('div');
  el.className = 'msg bot pending';
  el.innerHTML = `<div class="seal">侍</div><div class="paper"><p><span class="thinking"><i></i><i></i><i></i></span></p></div>`;
  chat.appendChild(el); scrollDown(); S.pending = el; return el;
}
function fillBot(el, text, meta) {
  el.classList.remove('pending');
  $('.paper', el).innerHTML = `<p>${esc(text)}</p><span class="meta">${meta}</span>`;
  S.lastBot = el; scrollDown();
}
function notice(text, err = false) {
  const el = document.createElement('div');
  el.className = 'notice' + (err ? ' err' : '');
  el.textContent = text;
  chat.appendChild(el); scrollDown();
}

// ---------------------------------------------------------------- server events
function handle(m) {
  switch (m.type) {
    case 'ready': break;
    case 'listening': break;
    case 'partial':
      liveText.textContent = m.text || '…'; liveText.classList.toggle('empty', !m.text); break;
    case 'final':
      live.hidden = true;
      if (m.text) addUser(m.text, `🎙 ${m.audio_s ?? '?'} s · lang <b>${esc(m.lang)}</b> · stt ${m.stt_ms} ms`);
      break;
    case 'thinking':
      S.busy = true; addBotPending(); break;
    case 'reply': {
      const el = S.pending || addBotPending();
      const src = m.source === 'mock' ? '<b>mock router</b>' : 'go router';
      const mode = { chosen: 'chosen', auto: 'auto · most used', router: 'router' }[m.lang_mode] || '';
      fillBot(el, m.text, `${src} · ${m.router_ms} ms · lang <b>${esc(m.lang)}</b>${mode ? ` <span class="lm">${mode}</span>` : ''}`);
      S.pending = null;  // answered: a later notice (e.g. TTS down) must not remove this bubble
      S.turn = { reply: m, audio: null, latency: null };
      langNote(m);
      renderTrace();
      break;
    }
    case 'audio': {
      const el = S.lastBot;
      if (el) {
        const a = document.createElement('audio');
        a.controls = true; a.src = m.url; a.preload = 'auto';
        $('.paper', el).appendChild(a);
        const meta = $('.meta', el);
        meta.innerHTML += ` · 🔊 ${m.duration_s} s in ${m.tts_ms} ms (${[...new Set(m.segments.map((s) => s.lang))].join('+')})`;
        a.onended = () => { if (handsFree.checked && !S.recording) startRecording(); };
        if (autoplay.checked) a.play().catch(() => notice('click ▶ to hear the reply (autoplay blocked)'));
      }
      if (S.turn) { S.turn.audio = m; renderTrace(); }
      break;
    }
    case 'turn_done':
      S.busy = false; S.pending = null;
      if (S.turn) { S.turn.latency = m.latency_ms; S.turn.n = m.turn; renderTrace(); }
      if (handsFree.checked && !S.recording && !(S.lastBot && $('audio', S.lastBot) && autoplay.checked)) startRecording();
      break;
    case 'notice': notice(m.message); if (S.pending) { S.pending.remove(); S.pending = null; } S.busy = false; live.hidden = true; break;
    case 'error': notice(m.message, true); if (S.pending) { S.pending.remove(); S.pending = null; } S.busy = false; live.hidden = true; break;
    case 'cancelled': live.hidden = true; break;
    default: break;
  }
}

// ---------------------------------------------------------------- trace panel (folding screen)
function renderTrace() {
  const t = S.turn; if (!t) return;
  const tr = t.reply.trace || {};
  const tags = (list, hi) => (list || []).map((s) => `<span class="tag ${hi ? 'hi' : ''}">${esc(s.scenario_id || s.id || s)}${s.name ? ' ' + esc(s.name) : ''}${s.confidence != null ? ' · ' + Number(s.confidence).toFixed(2) : ''}</span>`).join('') || '<span class="dim">—</span>';
  const lat = t.latency || {};
  const rows = [
    ['turn', `${t.n ?? '…'} · ${t.reply.source === 'mock' ? 'mock router' : 'go router'}`],
    ['language', esc(tr.language || t.reply.lang || '—')],
    ['reply lang', `${esc(t.reply.lang || '—')} · ${esc(t.reply.lang_mode || '—')}${t.reply.lang_counts ? ` <span class="dim">(kk ${t.reply.lang_counts.kk} / ru ${t.reply.lang_counts.ru})</span>` : ''}`],
    ['scenarios', tags(tr.scenarios, true)],
    ['alternatives', tags(tr.alternatives, false)],
    ['reason', esc(tr.reason || '—')],
    ['latency ms', `stt ${lat.stt ?? '–'} · router ${lat.router ?? t.reply.router_ms} · tts ${lat.tts ?? '–'} · <b>total ${lat.total ?? '…'}</b>`],
    ['voice', t.audio ? t.audio.segments.map((s) => `${s.lang}→${s.engine.split('_')[0]} (${s.voice}, ${s.ms} ms)`).join('<br>') : '<span class="dim">—</span>'],
    ['raw', `<details><summary class="dim">trace json</summary><code>${esc(JSON.stringify(tr, null, 1))}</code></details>`],
  ];
  $('#traceBody').innerHTML = rows.map(([k, v]) => `<div class="trow"><b>${k}</b><div>${v}</div></div>`).join('');
}
$('#traceBtn').onclick = () => {
  const panel = $('#trace'), on = panel.hidden;
  panel.hidden = !on; $('.layout').classList.toggle('with-trace', on); $('#traceBtn').classList.toggle('on', on); store.set('trace', on);
};
if (store.get('trace', false)) $('#traceBtn').click();

// ---------------------------------------------------------------- microphone
function levelMeter(rms, dt) {
  level.style.width = Math.min(100, rms * 600) + '%';
  if (rms > 0.012) { S.speechMs += dt; S.silenceSince = null; }
  else if (handsFree.checked && S.speechMs > 600) {
    S.silenceSince ??= performance.now();
    if (performance.now() - S.silenceSince > 1200) stopRecording();
  }
}
function downsample(f32, from, to) {  // used only when the browser refused a 16 kHz AudioContext
  const ratio = from / to, n = Math.floor(f32.length / ratio), out = new Int16Array(n);
  for (let i = 0; i < n; i++) { const v = Math.max(-1, Math.min(1, f32[Math.floor(i * ratio)])); out[i] = v < 0 ? v * 32768 : v * 32767; }
  return out;
}
async function ensureMic() {
  if (S.ctx) { if (S.ctx.state === 'suspended') await S.ctx.resume(); return; }
  S.stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true, channelCount: 1 } });
  try { S.ctx = new AudioContext({ sampleRate: 16000 }); } catch { S.ctx = new AudioContext(); }
  await S.ctx.audioWorklet.addModule('/static/worklet.js');
  const src = S.ctx.createMediaStreamSource(S.stream);
  S.node = new AudioWorkletNode(S.ctx, 'pcm-capture');
  S.node.port.onmessage = (e) => {
    if (!S.recording) return;
    const now = performance.now(), dt = now - S.lastTick; S.lastTick = now;
    levelMeter(e.data.rms, dt);
    if (S.ws && S.ws.readyState === 1) {
      if (S.ctx.sampleRate === 16000) S.ws.send(e.data.pcm);
      else S.ws.send(downsample(new Float32Array(new Int16Array(e.data.pcm), 0).map((v) => v / 32768), S.ctx.sampleRate, 16000).buffer);
    }
  };
  src.connect(S.node);
}
async function startRecording() {
  if (S.recording || S.busy || !S.ready) return;
  try { await ensureMic(); } catch (e) { notice('microphone unavailable: ' + e.message + ' (needs localhost or HTTPS)', true); return; }
  S.recording = true; S.t0 = S.lastTick = performance.now(); S.speechMs = 0; S.silenceSince = null;
  send({ type: 'start', reply_language: langPref() });
  mic.classList.add('rec'); live.hidden = false; liveText.textContent = '…'; liveText.classList.add('empty'); level.style.width = '0';
  S.timer = setInterval(() => { timer.textContent = ((performance.now() - S.t0) / 1000).toFixed(1) + ' s'; }, 100);
  hint.textContent = handsFree.checked ? 'Listening… stops by itself after a pause, or tap the ring.' : 'Listening… release to send.';
}
function stopRecording(cancel = false) {
  if (!S.recording) return;
  S.recording = false;
  clearInterval(S.timer); mic.classList.remove('rec'); level.style.width = '0';
  send({ type: cancel ? 'cancel' : 'end' });
  if (cancel) live.hidden = true; else { liveText.textContent = liveText.textContent === '…' ? 'recognizing…' : liveText.textContent; }
  hint.textContent = handsFree.checked ? 'Hands-free: tap the ring to start; it stops after a pause and resumes after each reply.' : 'Hold the ring (or Space) and speak. Release to send.';
}
let pressAt = 0;
mic.addEventListener('pointerdown', (e) => { e.preventDefault(); mic.setPointerCapture(e.pointerId); pressAt = performance.now();
  if (handsFree.checked) { S.recording ? stopRecording() : startRecording(); } else startRecording(); });
mic.addEventListener('pointerup', () => { if (!handsFree.checked) stopRecording(); });
mic.addEventListener('pointercancel', () => { if (!handsFree.checked) stopRecording(); });
document.addEventListener('keydown', (e) => { if (e.code === 'Space' && !e.repeat && !/INPUT|TEXTAREA/.test(e.target.tagName)) { e.preventDefault(); startRecording(); } });
document.addEventListener('keyup', (e) => { if (e.code === 'Space' && !/INPUT|TEXTAREA/.test(e.target.tagName) && !handsFree.checked) stopRecording(); });
handsFree.checked = store.get('handsFree', false); autoplay.checked = store.get('autoplay', true);
handsFree.onchange = () => { store.set('handsFree', handsFree.checked); stopRecording(true); hint.textContent = handsFree.checked ? 'Hands-free: tap the ring to start; it stops after a pause and resumes after each reply.' : 'Hold the ring (or Space) and speak. Release to send.'; };
autoplay.onchange = () => store.set('autoplay', autoplay.checked);

// ---------------------------------------------------------------- typed turns
$('#form').onsubmit = (e) => {
  e.preventDefault();
  const text = $('#text').value.trim();
  if (!text || S.busy || !S.ready) return;
  $('#text').value = '';
  addUser(text, '✎ typed');
  send({ type: 'text', text, reply_language: langPref() });
};

// ---------------------------------------------------------------- init
mic.disabled = true;
health(); setInterval(health, 10000);
connect();
