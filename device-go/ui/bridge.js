/* 云点传话筒 · 教室端（Go 版）前端桥
 * 作用：把 index.html 里的 window.pywebview.api.* 调用转发给本机 Go 服务，
 * 并把 Go 侧推送转发进页面的 window.__push —— 页面代码无需改动。
 */
(function () {
  'use strict';
  var BASE = location.origin;

  function rpc(name, args) {
    return fetch(BASE + '/rpc/' + name, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ args: Array.prototype.slice.call(args || []) })
    }).then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    }).catch(function (e) {
      try { console.warn('[bridge] ' + name + ' 调用失败', e); } catch (x) { }
      return null;
    });
  }

  var METHODS = ['init', 'ui_ready', 'set_config', 'bind', 'unbind', 'get_audio',
    'played', 'ack', 'view_done', 'extend', 'min_window', 'hide_window',
    'show_window', 'open_log', 'open_dir', 'set_autostart', 'quit_app', 'ui_log', 'close_choice'];

  var api = {};
  METHODS.forEach(function (m) {
    api[m] = function () { return rpc(m, arguments); };
  });
  window.pywebview = window.pywebview || {};
  window.pywebview.api = api;

  /* 提示音：用 WebAudio 现场合成，免额外音频文件 */
  var beepCtx = null;
  function beep(times) {
    try {
      var AC = window.AudioContext || window.webkitAudioContext;
      if (!AC) return;
      beepCtx = beepCtx || new AC();
      var n = Math.max(1, Math.min(5, times || 1));
      var t0 = beepCtx.currentTime;
      for (var i = 0; i < n; i++) {
        var t = t0 + i * 0.22;
        var osc = beepCtx.createOscillator();
        var g = beepCtx.createGain();
        osc.type = 'sine';
        osc.frequency.value = 880;
        g.gain.setValueAtTime(0.0001, t);
        g.gain.exponentialRampToValueAtTime(0.35, t + 0.02);
        g.gain.exponentialRampToValueAtTime(0.0001, t + 0.16);
        osc.connect(g);
        g.connect(beepCtx.destination);
        osc.start(t);
        osc.stop(t + 0.18);
      }
      if (beepCtx.state === 'suspended') beepCtx.resume();
    } catch (e) { }
  }

  function deliver(p) {
    if (typeof window.__push === 'function') {
      window.__push(p);
      return;
    }
    setTimeout(function () { deliver(p); }, 400);
  }

  /* Go 侧推送（SSE）→ 页面 __push */
  function connect() {
    var es = new EventSource(BASE + '/events');
    es.onmessage = function (ev) {
      var p;
      try { p = JSON.parse(ev.data); } catch (e) { return; }
      if (!p) return;
      if (p.t === 'beep') { beep(p.times); return; }
      deliver(p);
    };
    es.onerror = function () { /* EventSource 自动重连 */ };
  }

  connect();

  function ready() {
    try { window.dispatchEvent(new Event('pywebviewready')); } catch (e) { }
  }
  ready();
  setTimeout(ready, 60);
  setTimeout(ready, 600);
})();
