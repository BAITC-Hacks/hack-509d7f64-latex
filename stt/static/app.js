'use strict';
// STT Test Console — vanilla JS, talks to the FastAPI server in ../server.py

const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const pct = (x) => (x * 100).toFixed(1) + '%';
const store = {
  get(k, d) { try { const v = localStorage.getItem('stt.' + k); return v === null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem('stt.' + k, JSON.stringify(v)); } catch { /* private mode etc. */ } },
};

const state = { vad: true, live: true, autoStop: true, utterances: [], scenarios: {}, filtered: [], uIdx: 0, samples: [], history: [] };

// ---------------------------------------------------------------- API
async function api(path, opts) {
  const res = await fetch(path, opts);
  let json = {};
  try { json = await res.json(); } catch { /* non-JSON error body */ }
  if (!res.ok) throw new Error(json.detail || res.statusText || `HTTP ${res.status}`);
  return json;
}

async function transcribeBlob(blob, filename, reference) {
  const fd = new FormData();
  fd.append('file', blob, filename);
  if (reference) fd.append('reference', reference);
  const t0 = performance.now();
  const r = await api(`/transcribe?vad=${state.vad}`, { method: 'POST', body: fd });
  r.roundtrip_ms = Math.round(performance.now() - t0);
  return r;
}

async function pollHealth() {
  const el = $('#health');
  try {
    const h = await api('/health');
    if (h.status === 'ok') { el.textContent = `ready · ${h.model} · ${h.device}${h.vad ? ' · vad' : ''}`; el.className = 'pill ok'; return; }
    el.textContent = 'model loading…'; el.className = 'pill';
  } catch { el.textContent = 'server unreachable'; el.className = 'pill err'; }
  setTimeout(pollHealth, 2000);
}

// ---------------------------------------------------------------- rendering
const scoreClass = (x, good, mid) => (x <= good ? 'good' : x <= mid ? 'mid' : 'bad');

function diffHtml(r) {
  const words = r.alignment.map((op) => {
    if (op.op === 'eq') return `<span class="w">${esc(op.hyp)}</span>`;
    if (op.op === 'sub') return `<span class="w sub" title="reference: ${esc(op.ref)}"><s>${esc(op.ref)}</s>${esc(op.hyp)}</span>`;
    if (op.op === 'del') return `<span class="w del" title="missing in transcript">${esc(op.ref)}</span>`;
    return `<span class="w ins" title="not in reference">${esc(op.hyp)}</span>`;
  }).join(' ');
  return `<div class="diff">
    <div class="scores"><span class="score ${scoreClass(r.wer, 0.1, 0.25)}">WER ${pct(r.wer)}</span><span class="score ${scoreClass(r.cer, 0.05, 0.15)}">CER ${pct(r.cer)}</span></div>
    <div class="words">${words}</div>
    <div class="legend"><span class="w sub">substituted</span> <span class="w del">missing</span> <span class="w ins">extra</span> · reference: ${esc(r.reference)}</div>
  </div>`;
}

function renderResult(el, r, heading = '') {
  const t = r.timings_ms || {};
  const lat = r.response_ms != null ? `end of speech → text <b>${r.response_ms} ms</b> · `
    : r.roundtrip_ms != null ? `round-trip <b>${r.roundtrip_ms} ms</b> · ` : '';
  const segs = (r.segments || []).length > 1
    ? `<details><summary>${r.segments.length} segments</summary>${r.segments.map((s) => `<div class="seg">${s.start}s–${s.end}s: ${esc(s.text)}</div>`).join('')}</details>` : '';
  el.innerHTML = `<div class="result">
    ${heading ? `<div class="meta">${heading}</div>` : ''}
    <p class="text">${r.text ? esc(r.text) : '<i class="muted">(no speech recognized)</i>'}</p>
    <div class="meta">${lat}server ${t.total} ms (decode ${t.decode_audio} · vad ${t.vad} · asr ${t.asr}) · audio ${r.duration_s} s · lang~<b>${esc(r.language_hint)}</b> · ${esc(r.device)}</div>
    ${r.alignment ? diffHtml(r) : ''}${segs}
    ${r.text ? `<div class="speakwrap"><button class="link speakBtn" data-text="${esc(r.text)}">🔊 read back with TTS</button></div>` : ''}
  </div>`;
}
const busy = (el, msg = 'transcribing…') => { el.innerHTML = `<div class="result busy">${msg}</div>`; };
const fail = (el, e) => { el.innerHTML = `<div class="result error">${esc(e.message || e)}</div>`; };

// ---------------------------------------------------------------- recorder
const pickMime = () => ['audio/webm;codecs=opus', 'audio/webm', 'audio/ogg;codecs=opus', 'audio/mp4']
  .find((m) => window.MediaRecorder && MediaRecorder.isTypeSupported(m)) || '';
const extFor = (mime) => (mime.includes('mp4') ? 'm4a' : mime.includes('ogg') ? 'ogg' : 'webm');

class Recorder {
  constructor(handlers) { this.h = handlers; this.active = false; }

  async start() {
    if (!navigator.mediaDevices?.getUserMedia || !window.MediaRecorder) throw new Error('this browser cannot record audio (needs localhost or HTTPS)');
    this.stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true } });
    this.ctx = new (window.AudioContext || window.webkitAudioContext)();
    this.analyser = this.ctx.createAnalyser();
    this.analyser.fftSize = 2048;
    this.ctx.createMediaStreamSource(this.stream).connect(this.analyser);
    this.buf = new Float32Array(this.analyser.fftSize);
    const mime = pickMime();
    this.rec = new MediaRecorder(this.stream, mime ? { mimeType: mime } : {});
    this.chunks = [];
    this.rec.ondataavailable = (e) => { if (e.data.size) this.chunks.push(e.data); };
    this.rec.onstop = () => this._finish();
    this.t0 = this.lastTick = performance.now();
    this.speechMs = 0; this.silenceSince = null; this.active = true;
    this.rec.start(500);
    this._tick();
    if (state.live) this.liveTimer = setInterval(() => this._live(), 1500);
  }

  _tick() {
    if (!this.active) return;
    const now = performance.now(), dt = now - this.lastTick;
    this.lastTick = now;
    this.analyser.getFloatTimeDomainData(this.buf);
    let s = 0;
    for (let i = 0; i < this.buf.length; i++) s += this.buf[i] * this.buf[i];
    const rms = Math.sqrt(s / this.buf.length);
    this.h.onLevel(rms, now - this.t0);
    if (state.autoStop) {
      if (rms > 0.015) { this.speechMs += dt; this.silenceSince = null; }
      else if (this.speechMs > 600) {
        this.silenceSince ??= now;
        if (now - this.silenceSince > 1500) { this.stop(); return; }
      }
    }
    requestAnimationFrame(() => this._tick());
  }

  async _live() {
    if (this.inflight || !this.chunks.length || !this.active) return;
    this.inflight = true;
    try {
      const blob = new Blob(this.chunks, { type: this.rec.mimeType });
      const r = await transcribeBlob(blob, 'live.' + extFor(this.rec.mimeType));
      if (this.active) this.h.onLive(r.text);
    } catch { /* partial container may not decode yet; ignore */ } finally { this.inflight = false; }
  }

  stop() {
    if (this.active && this.rec.state === 'recording') { this.stopAt = performance.now(); this.active = false; this.rec.stop(); }
  }

  _finish() {
    clearInterval(this.liveTimer);
    this.stream.getTracks().forEach((t) => t.stop());
    this.ctx.close().catch(() => {});
    const mime = this.rec.mimeType || 'audio/webm';
    this.h.onDone(new Blob(this.chunks, { type: mime }), extFor(mime), this.stopAt || performance.now());
  }
}

function setupRecPanel(root, { reference = () => '', label }) {
  const btn = $('.micbtn', root), timer = $('.timer', root), meter = $('.meter i', root), live = $('.live', root);
  const out = $('.out', root.closest('.card'));
  let rec = null;

  async function start() {
    if (rec?.active) return;
    live.hidden = !state.live; live.textContent = '…';
    rec = new Recorder({
      onLevel: (rms, ms) => { meter.style.width = Math.min(100, rms * 500) + '%'; timer.textContent = (ms / 1000).toFixed(1) + ' s'; },
      onLive: (text) => { live.textContent = text || '…'; },
      onDone: async (blob, ext, stopAt) => {
        btn.classList.remove('rec'); meter.style.width = '0'; live.hidden = true;
        busy(out);
        try {
          const r = await transcribeBlob(blob, `mic.${ext}`, reference());
          r.response_ms = Math.round(performance.now() - stopAt);
          renderResult(out, r);
          addHistory({ source: label(), blob, filename: `mic.${ext}`, r });
        } catch (e) { fail(out, e); }
      },
    });
    try { await rec.start(); btn.classList.add('rec'); }
    catch (e) { rec = null; fail(out, new Error('Microphone unavailable: ' + e.message)); }
  }

  btn.onclick = () => { btn.blur(); rec?.active ? rec.stop() : start(); };
  return { start, stop: () => rec?.stop(), get active() { return !!rec?.active; } };
}

// ---------------------------------------------------------------- tabs & settings
const TABS = ['mic', 'read', 'upload', 'samples', 'tts', 'history'];
let activeTab = TABS.includes(store.get('tab')) ? store.get('tab') : 'mic';
function showTab(name) {
  activeTab = name;
  $$('#tabs button').forEach((b) => b.classList.toggle('active', b.dataset.tab === name));
  $$('.panel').forEach((p) => p.classList.toggle('active', p.id === 'panel-' + name));
  store.set('tab', name);
}
$$('#tabs button').forEach((b) => { b.onclick = () => showTab(b.dataset.tab); });
showTab(activeTab);

for (const [id, key] of [['optVad', 'vad'], ['optLive', 'live'], ['optAutoStop', 'autoStop']]) {
  const el = $('#' + id);
  el.checked = state[key] = store.get('opt.' + key, state[key]);
  el.onchange = () => { state[key] = el.checked; store.set('opt.' + key, el.checked); };
}

// ---------------------------------------------------------------- read-aloud phrases
const currentUtterance = () => state.filtered[state.uIdx];

async function loadUtterances() {
  try {
    const d = await api('/utterances');
    state.utterances = d.utterances; state.scenarios = d.scenarios; state.responses = d.responses || [];
    fillReplyPicker();
  } catch (e) { $('#uMeta').textContent = 'could not load phrases: ' + e.message; return; }
  if (!state.utterances.length) { $('#uMeta').textContent = 'dev_utterances.json not found — set STT_DATASET_DIR'; return; }
  $('#uLang').value = store.get('uLang', ''); $('#uType').value = store.get('uType', '');
  applyFilter(store.get('uIdx', 0));
}
function applyFilter(idx = 0) {
  const lang = $('#uLang').value, type = $('#uType').value;
  store.set('uLang', lang); store.set('uType', type);
  state.filtered = state.utterances.filter((u) => (!lang || u.lang === lang) && (!type || u.type === type));
  state.uIdx = Math.min(idx, Math.max(state.filtered.length - 1, 0));
  showUtterance();
}
function showUtterance() {
  const u = currentUtterance();
  if (!u) { $('#uMeta').textContent = 'no phrases match the filter'; $('#uText').textContent = ''; $('#uScen').textContent = ''; return; }
  $('#uMeta').innerHTML = `<b>${esc(u.id)}</b> · ${esc(u.lang)} · ${esc(u.type)} · ${state.uIdx + 1} / ${state.filtered.length}`;
  $('#uText').textContent = u.text;
  $('#uScen').innerHTML = 'expected scenario: ' + (u.expected || []).map((id) => `<b>${esc(id)}</b> ${esc(state.scenarios[id] || '')}`).join(' → ');
  store.set('uIdx', state.uIdx);
}
const step = (d) => { if (state.filtered.length) { state.uIdx = (state.uIdx + d + state.filtered.length) % state.filtered.length; showUtterance(); } };
$('#uLang').onchange = $('#uType').onchange = () => applyFilter(0);
$('#uPrev').onclick = () => step(-1);
$('#uNext').onclick = () => step(1);
$('#uRandom').onclick = () => { if (state.filtered.length) { state.uIdx = Math.floor(Math.random() * state.filtered.length); showUtterance(); } };

// ---------------------------------------------------------------- microphone panels + push-to-talk
const panels = {
  mic: setupRecPanel($('#micRec'), { label: () => 'microphone' }),
  read: setupRecPanel($('#readRec'), { reference: () => currentUtterance()?.text || '', label: () => `read-aloud ${currentUtterance()?.id || ''}` }),
};
let spaceHeld = false;
document.addEventListener('keydown', (e) => {
  if (e.code !== 'Space' || e.repeat || /INPUT|TEXTAREA|SELECT|BUTTON/.test(e.target.tagName)) return;
  const p = panels[activeTab];
  if (!p) return;
  e.preventDefault();
  if (!p.active) { spaceHeld = true; p.start(); }
});
document.addEventListener('keyup', (e) => {
  if (e.code === 'Space' && spaceHeld) { spaceHeld = false; panels[activeTab]?.stop(); }
});

// ---------------------------------------------------------------- upload
const drop = $('#drop'), fileIn = $('#fileIn');
$('#browse').onclick = () => fileIn.click();
['dragenter', 'dragover'].forEach((ev) => drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.add('over'); }));
['dragleave', 'drop'].forEach((ev) => drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.remove('over'); }));
drop.addEventListener('drop', (e) => handleFiles(e.dataTransfer.files));
fileIn.onchange = () => { handleFiles(fileIn.files); fileIn.value = ''; };

async function handleFiles(files) {
  const out = $('#upOut');
  for (const f of files) {
    const box = document.createElement('div');
    out.prepend(box);
    busy(box, `transcribing ${esc(f.name)}…`);
    try {
      const r = await transcribeBlob(f, f.name, $('#upRef').value.trim());
      renderResult(box, r, esc(f.name));
      addHistory({ source: `upload ${f.name}`, blob: f, filename: f.name, r });
    } catch (e) { fail(box, e); }
  }
}

// ---------------------------------------------------------------- bundled samples
async function loadSamples() {
  const tb = $('#samples tbody');
  try { state.samples = await api('/samples'); }
  catch (e) { tb.innerHTML = `<tr><td class="muted">${esc(e.message)}</td></tr>`; return; }
  if (!state.samples.length) { tb.innerHTML = '<tr><td class="muted">no samples — run <code>python stt/fetch_samples.py</code></td></tr>'; return; }
  tb.innerHTML = state.samples.map((m, i) => `<tr>
    <td class="act"><span class="pill">${esc(m.lang)}</span></td>
    <td><div><b>${esc(m.file)}</b> <span class="muted small">${(m.num_samples / 16000).toFixed(1)} s</span></div>
        <div class="ref">reference: <b>${esc(m.transcription)}</b></div><div id="s${i}"></div></td>
    <td class="act"><button class="secondary" data-i="${i}">Transcribe</button>
        <audio controls preload="none" src="/samples/${encodeURIComponent(m.file)}"></audio></td></tr>`).join('');
  $$('button[data-i]', tb).forEach((b) => { b.onclick = () => runSample(+b.dataset.i); });
}
async function runSample(i) {
  const m = state.samples[i], box = $(`#s${i}`);
  busy(box);
  try {
    const t0 = performance.now();
    const r = await api(`/transcribe?sample=${encodeURIComponent(m.file)}&vad=${state.vad}`, { method: 'POST' });
    r.roundtrip_ms = Math.round(performance.now() - t0);
    renderResult(box, r);
    addHistory({ source: `sample ${m.file}`, url: `/samples/${encodeURIComponent(m.file)}`, r });
    return r;
  } catch (e) { fail(box, e); return null; }
}
$('#allBtn').onclick = async () => {
  const btn = $('#allBtn');
  btn.disabled = true;
  const rs = [];
  for (let i = 0; i < state.samples.length; i++) rs.push(await runSample(i));
  const ok = rs.filter(Boolean);
  const avg = (k) => ok.reduce((a, r) => a + r[k], 0) / Math.max(ok.length, 1);
  $('#allSummary').textContent = ok.length ? `avg WER ${pct(avg('wer'))} · avg CER ${pct(avg('cer'))} over ${ok.length} clips` : 'no results';
  btn.disabled = false;
};

// ---------------------------------------------------------------- history
function addHistory({ source, blob = null, filename = null, url = null, r }) {
  state.history.unshift({ time: new Date(), source, blob, filename, url: url || (blob ? URL.createObjectURL(blob) : null), r });
  renderHistory();
}
function renderHistory() {
  const tb = $('#history tbody');
  $('#histCount').textContent = state.history.length;
  const scored = state.history.filter((h) => h.r.wer != null);
  $('#histSummary').textContent = state.history.length
    ? `${state.history.length} transcription${state.history.length > 1 ? 's' : ''}`
      + (scored.length ? ` · avg WER ${pct(scored.reduce((a, h) => a + h.r.wer, 0) / scored.length)} over ${scored.length} scored` : '')
    : 'Nothing transcribed yet in this session.';
  tb.innerHTML = state.history.map((h, i) => `<tr>
    <td class="act muted small">${h.time.toLocaleTimeString()}<br>${esc(h.source)}</td>
    <td class="htext">${h.r.text ? esc(h.r.text) : '<i class="muted">(empty)</i>'}${h.r.reference ? `<div class="ref">ref: <b>${esc(h.r.reference)}</b></div>` : ''}</td>
    <td class="act small muted">${h.r.duration_s} s · ${h.r.response_ms ?? h.r.roundtrip_ms} ms<br>lang~${esc(h.r.language_hint)}${h.r.wer != null ? `<br><span class="score ${scoreClass(h.r.wer, 0.1, 0.25)}">WER ${pct(h.r.wer)}</span>` : ''}</td>
    <td class="act">${h.url ? `<audio controls preload="none" src="${h.url}"></audio>` : ''}${h.blob ? `<button class="link small" data-dl="${i}">download</button>` : ''}</td></tr>`).join('');
  $$('button[data-dl]', tb).forEach((b) => {
    b.onclick = () => {
      const h = state.history[+b.dataset.dl];
      const a = document.createElement('a');
      a.href = h.url;
      a.download = `${h.time.toISOString().slice(11, 19).replace(/:/g, '')}_${h.filename || 'audio'}`;
      a.click();
    };
  });
}
$('#histExport').onclick = () => {
  const data = state.history.map((h) => ({
    time: h.time.toISOString(), source: h.source, text: h.r.text, language_hint: h.r.language_hint, duration_s: h.r.duration_s,
    response_ms: h.r.response_ms ?? null, roundtrip_ms: h.r.roundtrip_ms ?? null, timings_ms: h.r.timings_ms,
    reference: h.r.reference ?? null, wer: h.r.wer ?? null, cer: h.r.cer ?? null, device: h.r.device, model: h.r.model,
  }));
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }));
  a.download = `stt_session_${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')}.json`;
  a.click();
};
$('#histClear').onclick = () => {
  state.history.forEach((h) => { if (h.blob) URL.revokeObjectURL(h.url); });
  state.history = [];
  renderHistory();
};

// ---------------------------------------------------------------- text-to-speech
async function pollTtsHealth() {
  const el = $('#ttsHealth');
  try {
    const h = await api('/tts/health');
    const ru = h.engines?.ru?.loaded, kk = h.engines?.kk?.loaded;
    if (h.status === 'ok') {
      el.textContent = `tts · ru ${ru ? '✓' : '✗'} · kk ${kk ? '✓' : '✗'}`;
      el.className = 'pill ok';
      el.title = [h.engines.ru.error && `ru: ${h.engines.ru.error}`, h.engines.kk.error && `kk: ${h.engines.kk.error}`].filter(Boolean).join('\n') || 'TTS service ready';
      loadVoices();
      return;
    }
    el.textContent = h.status === 'loading' ? 'tts loading…' : 'tts error'; el.className = h.status === 'error' ? 'pill err' : 'pill';
    el.title = JSON.stringify(h.engines);
  } catch (e) { el.textContent = 'tts offline'; el.className = 'pill err'; el.title = e.message; }
  setTimeout(pollTtsHealth, 3000);
}
async function loadVoices() {
  try {
    const v = await api('/tts/voices');
    const fill = (sel, list, def) => { sel.innerHTML = list.map((x) => `<option value="${esc(x)}"${x === def ? ' selected' : ''}>${esc(x)}</option>`).join('') || '<option value="">(engine not loaded)</option>'; };
    fill($('#ttsVoiceRu'), v.ru, store.get('voiceRu', v.default.ru));
    fill($('#ttsVoiceKk'), v.kk, store.get('voiceKk', v.default.kk));
  } catch { /* TTS offline */ }
}
$('#ttsVoiceRu').onchange = () => store.set('voiceRu', $('#ttsVoiceRu').value);
$('#ttsVoiceKk').onchange = () => store.set('voiceKk', $('#ttsVoiceKk').value);
function fillReplyPicker() {
  const sel = $('#ttsPick');
  const groups = { kk: [], ru: [] };
  state.responses.forEach((r, i) => (groups[r.lang] || (groups[r.lang] = [])).push(`<option value="${i}">${esc(r.scenario_id)} ${esc(r.kind)} — ${esc(r.text.slice(0, 70))}${r.text.length > 70 ? '…' : ''}</option>`));
  sel.innerHTML = '<option value="">— pick a reply from scenarios.json —</option>'
    + Object.entries(groups).map(([lang, opts]) => `<optgroup label="${lang} (${opts.length})">${opts.join('')}</optgroup>`).join('');
  sel.onchange = () => { const r = state.responses[+sel.value]; if (r) { $('#ttsText').value = r.text; $('#ttsLang').value = 'auto'; } };
}
const b64ToBlob = (b64, type) => { const bin = atob(b64), arr = new Uint8Array(bin.length); for (let i = 0; i < bin.length; i++) arr[i] = bin.charCodeAt(i); return new Blob([arr], { type }); };

async function synthesize(text, lang = 'auto') {
  const t0 = performance.now();
  const r = await api('/tts/synthesize', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ text, lang, voice_ru: $('#ttsVoiceRu').value || null, voice_kk: $('#ttsVoiceKk').value || null, format: 'wav' }) });
  r.roundtrip_ms = Math.round(performance.now() - t0);
  r.blob = b64ToBlob(r.audio_base64, r.content_type);
  r.url = URL.createObjectURL(r.blob);
  delete r.audio_base64;
  return r;
}
function renderSynthesis(el, r, text) {
  const segs = r.segments.map((sg) => `<tr><td class="act"><span class="pill">${esc(sg.lang)}</span></td><td>${esc(sg.text)}</td><td class="act">${esc(sg.voice)}<br>${sg.ms} ms · ${sg.duration_s} s</td></tr>`).join('');
  el.innerHTML = `<div class="result">
    <audio controls autoplay src="${r.url}"></audio>
    <div class="meta">audio <b>${r.duration_s} s</b> · synthesis <b>${r.timings_ms.total} ms</b> (RTF ${r.timings_ms.rtf ?? '–'}) · round-trip ${r.roundtrip_ms} ms · ${r.sample_rate} Hz
      · <a href="${r.url}" download="tts_${Date.now()}.wav">download wav</a></div>
    <table class="segtbl"><tbody>${segs}</tbody></table>
  </div>`;
}
$('#ttsSpeak').onclick = async () => {
  const text = $('#ttsText').value.trim(), out = $('#ttsOut'), btn = $('#ttsSpeak');
  if (!text) return;
  btn.disabled = true; busy(out, 'synthesizing…');
  try {
    const r = await synthesize(text, $('#ttsLang').value);
    renderSynthesis(out, r, text);
    addHistory({ source: 'tts', url: r.url, blob: r.blob, filename: 'tts.wav',
      r: { text, language_hint: r.segments.map((s) => s.lang).filter((v, i, a) => a.indexOf(v) === i).join('+'), duration_s: r.duration_s, roundtrip_ms: r.roundtrip_ms, timings_ms: r.timings_ms, device: 'tts', model: r.segments.map((s) => s.engine).filter((v, i, a) => a.indexOf(v) === i).join('+') } });
  } catch (e) { fail(out, e); }
  btn.disabled = false;
};
// "read back" buttons on STT result cards: transcript -> TTS -> play inline
document.addEventListener('click', async (e) => {
  const b = e.target.closest('.speakBtn');
  if (!b) return;
  const wrap = b.parentElement;
  b.disabled = true; b.textContent = '🔊 synthesizing…';
  try {
    const r = await synthesize(b.dataset.text, 'auto');
    wrap.insertAdjacentHTML('beforeend', `<audio controls autoplay src="${r.url}"></audio><span class="muted small">${r.duration_s} s · ${r.timings_ms.total} ms · ${r.segments.map((s) => s.lang).join('+')}</span>`);
    b.textContent = '🔊 read back again';
  } catch (err) { wrap.insertAdjacentHTML('beforeend', `<span class="small" style="color:var(--err)">${esc(err.message)}</span>`); b.textContent = '🔊 read back with TTS'; }
  b.disabled = false;
});

// ---------------------------------------------------------------- init
pollHealth();
pollTtsHealth();
loadUtterances();
loadSamples();
renderHistory();
