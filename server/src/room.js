// RoomDO —— 每个班级一个实例，负责：设备 WebSocket 保活、消息广播、离线补发、回执落地
// 在线判定的时间窗口由 Worker 传入（index.js 的 DeviceOnlineWindowMs 是唯一来源，
// 避免两处 40s 常量不同步改漏）
// D1 心跳镜像节流：lastSeen 的权威值在 DO storage（持久化，不会丢），D1 那份只给运维/后台
// 兜底看，故 5 分钟落一次库就够 —— 25s 一次 WS ping 若逐条落库，一天一个班就是 3456 行写，
// 免费版 D1 10 万行写/天约 29 个常在线班级就见底
const D1_MIRROR_MS = 300000;

export class RoomDO {
  constructor(state, env) {
    this.state = state;
    this.env = env;
    this._mir = null;  // D1 镜像节流状态 {at, dev} 的内存缓存，真身在 storage('mir')
  }

  get storage() { return this.state.storage; }

  async fetch(request) {
    const url = new URL(request.url);
    const path = url.pathname;

    if (path === '/ws') return this.handleWs(request, url);
    if (path === '/push') return this.handlePush(request);
    if (path === '/state') return this.handleState(url);
    if (path === '/kick') return this.handleKick(url);
    if (path === '/seen') return this.handleSeen();
    if (path === '/reschedule') return this.handleReschedule();
    return new Response('not found', { status: 404 });
  }

  // ---------- WebSocket ----------
  async handleWs(request, url) {
    const upgrade = (request.headers.get('Upgrade') || '').toLowerCase();
    if (upgrade !== 'websocket') {
      return new Response('expected websocket', { status: 426 });
    }
    const device = url.searchParams.get('device') || 'unknown';
    // 注意：index.js 转发设备 WS 时用的参数名是 classId=（兼容旧名 class）
    // 曾因只读 'class' 导致这里恒为 0 → 附件里的 classId=0 → WS 心跳永不镜像 D1
    const classId = Number(url.searchParams.get('classId') || url.searchParams.get('class') || 0);

    const pair = new WebSocketPair();
    const client = pair[0];
    const server = pair[1];
    this.state.acceptWebSocket(server, ['dev:' + device]);
    server.serializeAttachment({ device, classId, at: Date.now() });

    await this.touch(classId, device);

    // 产品规则：断线期间的消息一律不补发（发送侧已按"离线即拦"处理）
    return new Response(null, { status: 101, webSocket: client });
  }

  async webSocketMessage(ws, raw) {
    let msg;
    try { msg = JSON.parse(typeof raw === 'string' ? raw : new TextDecoder().decode(raw)); }
    catch { return; }
    const att = ws.deserializeAttachment() || {};
    if (msg.type === 'ping') {
      try { ws.send(JSON.stringify({ type: 'pong', ts: Date.now() })); } catch {}
      await this.touch(att.classId, att.device);
      return;
    }
    if (msg.type === 'hello') {
      await this.touch(att.classId, att.device);
      return;
    }
    if (msg.type === 'ack') {
      const id = Number(msg.id || 0);
      if (!id) return;
      await this.recordAck(att.classId, id);
      return;
    }
    if (msg.type === 'played') {
      const id = Number(msg.id || 0);
      if (!id) return;
      await this.recordPlayed(att.classId, id, msg.ok !== false);
      return;
    }
  }

  async webSocketClose(ws) {
    const att = ws.deserializeAttachment() || {};
    await this.markSeen(att.classId);
  }

  async webSocketError(ws) {
    const att = ws.deserializeAttachment() || {};
    await this.markSeen(att.classId);
  }

  // ---------- 内部接口 ----------
  async handlePush(request) {
    const body = await request.json();
    const now = Date.now();
    const item = {
      id: body.id, text: body.text, ts: now,
      voice: body.voice ? 1 : 0, repeat: body.repeat || 1,
      audio: !!body.audio
    };
    // 不再入待补发队列：教室端离线时发送侧已直接拦下

    const data = JSON.stringify({ type: 'notify', id: item.id, text: item.text, ts: item.ts, voice: item.voice, repeat: item.repeat, hasAudio: item.audio });
    let sent = 0;
    for (const ws of this.state.getWebSockets()) {
      try { ws.send(data); sent++; } catch {}
    }
    return Response.json({ ok: true, sent });
  }

  async handleState(url) {
    const win = Number((url && url.searchParams.get('win')) || 0);
    const sockets = this.state.getWebSockets();
    const lastSeen = (await this.storage.get('lastSeen')) || 0;
    // 在线判定：有活跃 WS，或最近 win 毫秒内有心跳（HTTP 轮询/配置查询也算在线）
    const fresh = win > 0 && lastSeen > 0 && (Date.now() - lastSeen) <= win;
    return Response.json({
      ok: true,
      online: (sockets.length > 0 || fresh) ? 1 : 0,
      sockets: sockets.length,
      lastSeen: lastSeen
    });
  }

  // 设备端心跳（HTTP 轮询/配置查询也算“在线”）：lastSeen 落 DO storage（权威值，
  // 手机端读的就是它），D1 只做节流镜像；mirrored 告诉调用方这次是否真写了 D1
  async handleSeen() {
    const now = Date.now();
    const mirrored = await this.touch(this.classId, null);  // touch 内部已落 lastSeen
    return Response.json({ ok: true, lastSeen: now, mirrored: mirrored });
  }

  async handleKick(url) {
    const reason = (url && url.searchParams.get('reason')) || 'unbind';
    // 解绑/踢下线：立即清掉心跳，手机端马上显示离线、发送侧立刻拦截
    await this.storage.put('lastSeen', 0);
    // 解绑/重置后让下一次心跳立刻把 last_seen 落回 D1（不等节流窗口）
    this._mir = { at: 0, dev: '' };
    await this.storage.put('mir', { at: 0, dev: '' });
    const list = this.state.getWebSockets();
    const n = list.length;
    for (const ws of list) {
      try { ws.send(JSON.stringify({ type: 'bye', reason: reason })); } catch { }
      try { ws.close(4001, reason); } catch { }
    }
    return Response.json({ ok: true, reason: reason, count: n });
  }

  // ---------- 定时任务（alarm 驱动，绕开免费版 cron 名额限制） ----------
  get classId() {
    const name = (this.state.id && this.state.id.name) || '';
    const m = /^class:(\d+)$/.exec(name);
    return m ? Number(m[1]) : 0;
  }

  async handleReschedule() {
    const at = await this.scheduleNextAlarm();
    return Response.json({ ok: true, alarm: at || 0 });
  }

  // 找到本班最近的待执行时间，设置 alarm；没有则清除
  async scheduleNextAlarm() {
    const classId = this.classId;
    if (!classId) return 0;
    let at = 0;
    try {
      const row = await this.env.DB.prepare(
        'SELECT MIN(next_at) AS n FROM schedules WHERE class_id=? AND enabled=1'
      ).bind(classId).first();
      if (row && row.n) at = Number(row.n) * 1000;
    } catch (e) {
      return 0;
    }
    if (!at) {
      try { await this.storage.deleteAlarm(); } catch (e) {}
      return 0;
    }
    const now = Date.now();
    if (at <= now) at = now + 1000;
    try { await this.storage.setAlarm(at); } catch (e) { return 0; }
    return at;
  }

  async alarm() {
    // 到点：通知 Worker 处理到期任务（语音合成、推送、派生下一次时间都在 Worker 侧）
    // 带上本班 id：Worker 只扫本班到期任务，避免 N 个班的 alarm 并发抢同一批任务
    try {
      const base = this.env.PUBLIC_BASE || 'https://your-domain.example.com';
      const cid = this.classId;
      if (base) {
        await fetch(base + '/internal/tick?key=' + encodeURIComponent(this.env.CRON_KEY || '') + (cid ? '&class_id=' + cid : ''), { method: 'POST' });
      }
    } catch (e) {}
    // 处理完再排下一次
    await this.scheduleNextAlarm();
  }

  // ---------- 持久化 ----------
  async recordAck(classId, id) {
    // 设备已收到 → 手机端显示"播报中"（不覆盖已 played 的最终态）
    try {
      await this.env.DB.prepare('UPDATE messages SET ack_at=?, status=? WHERE id=? AND status<>?')
        .bind(Date.now(), 'acked', id, 'played').run();
    } catch (e) {}
    await this.touch(classId, null);
  }

  async recordPlayed(classId, id, ok = true) {
    // 设备播报结束 → 手机端显示"已播报"；设备端播报失败 → "播报失败"
    try {
      await this.env.DB.prepare('UPDATE messages SET played_at=?, status=? WHERE id=?')
        .bind(Date.now(), ok ? 'played' : 'failed', id).run();
    } catch (e) {}
    await this.touch(classId, null);
  }

  async markSeen(classId) {
    if (!classId) return;
    try {
      await this.env.DB.prepare('UPDATE classes SET last_seen=? WHERE id=?').bind(Date.now(), classId).run();
    } catch (e) {}
  }

  // D1 镜像：lastSeen 的权威值在 DO storage / 内存，D1 只是冷启动与后台看板的兜底。
  // 心跳驱动（25s 一次 ping）若不节流，一天一个班就是 3456 行写，免费版 D1 10 万行写/天
  // 约 29 个常在线班级就见底 —— 故按 D1_MIRROR_MS 节流；device_id 变化时立即落库。
  // 节流状态必须持久化：DO 空闲会被驱逐，进程内变量随之归零，否则每次心跳都写 D1（已实测踩到）。
  // 返回 true 表示这次真的写了 D1（调用方据此减少 device_tokens 的旁路写）。
  async touch(classId, device) {
    const now = Date.now();
    await this.storage.put('lastSeen', now);
    if (!classId) return false;
    let mir = this._mir;
    if (!mir) {
      mir = (await this.storage.get('mir')) || {};
      this._mir = mir;
    }
    const devChanged = !!device && device !== (mir.dev || '');
    if (!devChanged && mir.at && now - mir.at < D1_MIRROR_MS) return false;
    mir.at = now;
    if (device) mir.dev = device;
    await this.storage.put('mir', { at: mir.at, dev: mir.dev || '' });
    try {
      if (device) {
        await this.env.DB.prepare('UPDATE classes SET last_seen=?, device_id=? WHERE id=?').bind(now, device, classId).run();
      } else {
        await this.env.DB.prepare('UPDATE classes SET last_seen=? WHERE id=?').bind(now, classId).run();
      }
    } catch (e) { }
    return true;
  }
}
