// 云点传话筒 服务端（Cloudflare Workers + Durable Objects + D1 + KV + Workers AI）
import { RoomDO } from './room.js';
export { RoomDO };

const VERSION = '1.1.1';
const MAX_TEXT = 160;
const SEND_LIMIT_PER_HOUR = 120;
// 教室端在线判定窗口：WS 在线 或 最近一次心跳在此毫秒窗口内
const DeviceOnlineWindowMs = 40000;
// 兜底失败判定：接受后迟迟没有回执的消息，手机端必须给“播报失败”，且不再补发
// （created_at 是秒；ack_at 是毫秒）
const StaleSentSec = 60;      // 连“已收到”都没回执超过 60 秒 → 判失败
const StaleAckedMs = 600000;   // 已回执“播报中”但 10 分钟还没播完 → 判失败（长文本+重复的兜底）

// ---- 卡密制（2026-10 定稿）----
// 无卡密不能注册；卡密只有「3 天试用」「1 年」两种；所有卡密统一 1 个班。
// 到期即停发（服务端硬校验 expire_at，与前端提示无关）。
const CARD_QUOTA = 1;
const TRIAL_CARD_DAYS = 3;
const YEAR_CARD_DAYS = 365;
const EXPIRED_MSG = '账号已到期，请使用激活码续期后再发送';

// 账号是否已到期（管理员不受限）
function isExpired(user) {
  if (!user) return true;
  if (user.role === 'admin') return false;
  return !!(user.expire_at && user.expire_at <= nowSec());
}

// 设备所属班级的账号是否已到期（与发送校验同源；管理员豁免）
async function classOwnerExpired(env, classId) {
  try {
    const row = await env.DB.prepare(
      'SELECT u.expire_at AS expire_at, u.role AS role FROM classes c JOIN users u ON u.id=c.user_id WHERE c.id=?'
    ).bind(classId).first();
    if (!row) return { expired: false, expireAt: 0 };
    const expired = row.role === 'admin' ? false : !!(row.expire_at && row.expire_at <= nowSec());
    return { expired, expireAt: row.expire_at || 0 };
  } catch (e) {
    return { expired: false, expireAt: 0 };
  }
}

const DEVICE_EXPIRED_TEXT = '账号已到期，请联系老师续期';

// 按卡密天数决定套餐标识（3 天内=体验，其余=标准；不降级已有 pro）
function planForDays(days, userPlan) {
  if (Number(days) <= TRIAL_CARD_DAYS) {
    return userPlan === 'pro' || userPlan === 'basic' ? userPlan : 'trial';
  }
  return userPlan === 'pro' ? 'pro' : 'basic';
}

const json = (data, status = 200, extra = {}) =>
  new Response(JSON.stringify(data), {
    status,
    headers: { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store', ...extra }
  });

const ok = (data = {}) => json({ ok: true, ...data });
const bad = (msg, code = 400) => json({ ok: false, error: msg }, code);

const enc = new TextEncoder();

function randomHex(bytes = 16) {
  const b = new Uint8Array(bytes);
  crypto.getRandomValues(b);
  return [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
}

// 去掉易混字符的绑定码（8 位）
const CODE_ALPHABET = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789';
function pairCode(len = 8) {
  const b = new Uint8Array(len);
  crypto.getRandomValues(b);
  let s = '';
  for (let i = 0; i < len; i++) s += CODE_ALPHABET[b[i] % CODE_ALPHABET.length];
  return s;
}

// 临时密码字符集：去掉 0/O/1/I/l 等易混字符，方便老师抄写/转述
const TEMP_PW_ALPHABET = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789';
function randomTempPassword(len = 12) {
  const limit = 256 - (256 % TEMP_PW_ALPHABET.length);
  let s = '';
  while (s.length < len) {
    const b = new Uint8Array(len * 2);
    crypto.getRandomValues(b);
    for (const x of b) {
      if (x >= limit) continue;
      s += TEMP_PW_ALPHABET[x % TEMP_PW_ALPHABET.length];
      if (s.length >= len) break;
    }
  }
  return s;
}

async function hashPassword(password, saltHex) {
  const salt = enc.encode(saltHex);
  const key = await crypto.subtle.importKey('raw', enc.encode(password), 'PBKDF2', false, ['deriveBits']);
  const bits = await crypto.subtle.deriveBits({ name: 'PBKDF2', salt, iterations: 60000, hash: 'SHA-256' }, key, 256);
  return [...new Uint8Array(bits)].map((x) => x.toString(16).padStart(2, '0')).join('');
}

function nowSec() { return Math.floor(Date.now() / 1000); }

function clientIp(request) {
  return request.headers.get('cf-connecting-ip') || request.headers.get('x-forwarded-for') || '';
}

async function readBody(request) {
  try { return await request.json(); } catch { return {}; }
}

// ---------------- 鉴权 ----------------
async function currentUser(request, env) {
  const h = request.headers.get('authorization') || '';
  const token = h.startsWith('Bearer ') ? h.slice(7).trim() : '';
  if (!token) return null;
  const row = await env.DB.prepare(
    'SELECT u.* FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token = ? AND s.expire_at > ?'
  ).bind(token, nowSec()).first();
  if (!row) return null;
  if (row.status !== 'active') return null;
  return row;
}

async function requireUser(request, env) {
  const user = await currentUser(request, env);
  if (!user) return { error: bad('登录已过期，请重新登录', 401) };
  return { user };
}

async function logAction(env, userId, action, detail, request) {
  try {
    await env.DB.prepare('INSERT INTO logs (user_id, action, detail, ip, created_at) VALUES (?,?,?,?,?)')
      .bind(userId || null, action, detail || '', clientIp(request || { headers: new Headers() }), nowSec()).run();
  } catch (e) {}
}

function planQuota(plan) {
  if (plan === 'pro') return 10;
  if (plan === 'basic') return 2;
  if (plan === 'trial') return 1;
  return 1;
}

// ---------------- 通知发送（核心） ----------------
async function pushToRoom(env, classId, payload) {
  const id = env.ROOM.idFromName('class:' + classId);
  const stub = env.ROOM.get(id);
  const res = await stub.fetch('https://room/push', { method: 'POST', body: JSON.stringify(payload) });
  return await res.json().catch(() => ({ ok: false }));
}

// 在线窗口（DeviceOnlineWindowMs）是唯一来源：透传给 RoomDO，避免 DO 里再写一份 40s 常量
async function roomState(env, classId) {
  const id = env.ROOM.idFromName('class:' + classId);
  const stub = env.ROOM.get(id);
  const res = await stub.fetch('https://room/state?win=' + DeviceOnlineWindowMs);
  return await res.json().catch(() => ({ ok: false, online: 0 }));
}

// 教室端心跳：让 RoomDO 记住“这台设备还活着”（HTTP 轮询/配置查询也算在线，用于发送侧离线拦截）
// 返回 DO 的判定结果（mirrored=true 表示这次心跳真的把 last_seen 镜像进了 D1，旁路写据此节流）
async function roomSeen(env, classId) {
  if (!classId) return null;
  try {
    const id = env.ROOM.idFromName('class:' + classId);
    const stub = env.ROOM.get(id);
    const res = await stub.fetch('https://room/seen', { method: 'POST' });
    return await res.json();
  } catch (e) { return null; }
}

// 让 RoomDO 依据该班最近的定时任务重排 alarm（cron 触发器在免费版名额已满，改用 DO alarm 驱动）
async function rescheduleRoom(env, classId) {
  if (!classId) return;
  try {
    const id = env.ROOM.idFromName('class:' + classId);
    const stub = env.ROOM.get(id);
    await stub.fetch('https://room/reschedule', { method: 'POST' });
  } catch (e) {}
}

async function synthAudio(env, text) {
  if (!env.AI) return null;
  const clean = String(text).replace(/\s+/g, ' ').trim().slice(0, MAX_TEXT);
  if (!clean) return null;
  const out = await env.AI.run('@cf/myshell-ai/melotts', { prompt: clean, lang: 'zh' });
  const b64 = out && (out.audio || (out.result && out.result.audio));
  if (!b64) return null;
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

async function sendNotify(env, ctx, user, cls, text, opts = {}) {
  text = String(text || '').trim();
  if (!text) return { ok: false, error: '通知内容不能为空' };
  if (text.length > MAX_TEXT) return { ok: false, error: `通知最多 ${MAX_TEXT} 字` };
  // 到期硬拦：到期后一律不能发送（手动喊话 / 定时任务 / 接口全部经这里）
  if (isExpired(user)) return { ok: false, error: EXPIRED_MSG, expired: true };

  const voice = opts.voice === 0 || opts.voice === false ? 0 : 1;
  const repeat = Math.min(5, Math.max(1, Number(opts.repeat) || 1));
  const kind = opts.kind || 'manual';

  const hourAgo = nowSec() - 3600;
  const cnt = await env.DB.prepare('SELECT COUNT(*) AS n FROM messages WHERE user_id=? AND created_at>?')
    .bind(user.id, hourAgo).first();
  if ((cnt && cnt.n) > SEND_LIMIT_PER_HOUR) return { ok: false, error: '发送过于频繁，请稍后再试' };

  // 教室端离线 → 直接拦下（不排队、不补发；定时任务同样丢弃）
  const live = await roomState(env, cls.id).catch(() => ({ online: 0, lastSeen: 0 }));
  const seenAgo = Date.now() - Number((live && live.lastSeen) || 0);
  const deviceOnline = (Number(live && live.online) || 0) > 0
    || (((live && live.lastSeen) || 0) > 0 && seenAgo <= DeviceOnlineWindowMs);
  if (!deviceOnline) {
    const isSched = kind === 'scheduled' || kind === 'schedule';
    if (isSched) return { ok: false, offline: true, dropped: true, error: '教室端离线，定时喊话已丢弃' };
    return { ok: false, offline: true, error: '教室端离线，暂不能喊话' };
  }

  const res = await env.DB.prepare(
    'INSERT INTO messages (user_id, class_id, text, voice, repeat, kind, status, created_at) VALUES (?,?,?,?,?,?,?,?)'
  ).bind(user.id, cls.id, text, voice, repeat, kind, 'sent', nowSec()).run();
  const msgId = res.meta && res.meta.last_row_id;

  // 文本先上屏：语音不再在发送路径里预合成。
  // 原来每条语音喊话都无条件跑一次 melotts + 220KB KV 写 + 一次 WS 补推 + 一次 D1 写，
  // 但教室端 Go 版有本地 TTS（微软中文语音），只有极少数情况才需要云端音频。
  // 现在音频改为完全按需：谁要 /api/audio/{id} 谁触发合成（Go 端本地合成失败时才来取，
  // 浏览器教室端每次播放都来取），apiAudio 里 KV 未命中即现场合成。
  const pushed = await pushToRoom(env, cls.id, { id: msgId, text, voice, repeat, audio: false });
  if (voice) {
    // 仅打一个"这条有语音"的标记（手机端历史列表的喇叭图标用），不合成、不占 KV
    const mark = env.DB.prepare('UPDATE messages SET audio_key=? WHERE id=?').bind('a:' + msgId, msgId).run().catch(() => {});
    if (ctx && ctx.waitUntil) ctx.waitUntil(mark); else await mark;
  }
  return { ok: true, id: msgId, pushed };
}

// ---------------- 账号 ----------------
async function apiRegister(request, env, ctx) {
  const b = await readBody(request);
  const username = String(b.username || '').trim();
  const password = String(b.password || '');
  if (!/^[A-Za-z0-9_@.\-]{3,32}$/.test(username)) return bad('账号需 3-32 位字母数字');
  if (password.length < 6) return bad('密码至少 6 位');

  const exists = await env.DB.prepare('SELECT id FROM users WHERE username=?').bind(username).first();
  if (exists) return bad('该账号已存在');

  const salt = randomHex(16);
  const hash = await hashPassword(password, salt);

  // 无卡密不能注册：卡密既是入场券，也决定有效期（3 天试用 / 365 天）
  const code = String(b.code || '').trim().toUpperCase();
  if (!code) return bad('注册需要激活码，请先获取激活码再注册');
  const codeRow = await env.DB.prepare('SELECT * FROM codes WHERE code=?').bind(code).first();
  if (!codeRow) return bad('激活码无效');
  if (codeRow.used_by) return bad('该激活码已被使用');
  if (!(Number(codeRow.days) > 0)) return bad('该激活码已停用，请联系客服换取新码');
  const plan = planForDays(codeRow.days, '');
  const quota = CARD_QUOTA;
  const expireAt = nowSec() + Number(codeRow.days) * 86400;

  const ins = await env.DB.prepare(
    'INSERT INTO users (username, pass_hash, salt, role, plan, class_quota, expire_at, status, created_at) VALUES (?,?,?,?,?,?,?,?,?)'
  ).bind(username, hash, salt, 'user', plan, quota, expireAt, 'active', nowSec()).run();
  const uid = ins.meta.last_row_id;

  if (codeRow) {
    await env.DB.prepare('UPDATE codes SET used_by=?, used_at=? WHERE code=?').bind(uid, nowSec(), code).run();
  }
  await logAction(env, uid, 'register', plan, request);
  const token = await createSession(env, uid);
  return ok({ token, user: await userPublic(env, uid) });
}

async function createSession(env, userId) {
  const token = randomHex(32);
  const exp = nowSec() + 30 * 86400;
  await env.DB.prepare('INSERT INTO sessions (token, user_id, created_at, expire_at) VALUES (?,?,?,?)')
    .bind(token, userId, nowSec(), exp).run();
  return token;
}

async function userPublic(env, uid) {
  const u = await env.DB.prepare('SELECT * FROM users WHERE id=?').bind(uid).first();
  if (!u) return null;
  const c = await env.DB.prepare('SELECT COUNT(*) AS n FROM classes WHERE user_id=?').bind(uid).first();
  return {
    id: u.id, username: u.username, role: u.role, plan: u.plan,
    classQuota: u.class_quota, classUsed: c ? c.n : 0,
    expireAt: u.expire_at, createdAt: u.created_at
  };
}

async function apiLogin(request, env) {
  const b = await readBody(request);
  const username = String(b.username || '').trim();
  const password = String(b.password || '');
  const u = await env.DB.prepare('SELECT * FROM users WHERE username=?').bind(username).first();
  if (!u) return bad('账号或密码错误');
  if (u.status !== 'active') return bad('账号已被停用，请联系客服');
  const hash = await hashPassword(password, u.salt);
  if (hash !== u.pass_hash) return bad('账号或密码错误');
  await env.DB.prepare('UPDATE users SET last_login=? WHERE id=?').bind(nowSec(), u.id).run();
  const token = await createSession(env, u.id);
  await logAction(env, u.id, 'login', '', request);
  return ok({ token, user: await userPublic(env, u.id) });
}

async function apiLogout(request, env) {
  const h = request.headers.get('authorization') || '';
  const token = h.startsWith('Bearer ') ? h.slice(7).trim() : '';
  if (token) await env.DB.prepare('DELETE FROM sessions WHERE token=?').bind(token).run();
  return ok({});
}

async function apiRedeem(request, env) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  const b = await readBody(request);
  const code = String(b.code || '').trim().toUpperCase();
  if (!code) return bad('请输入激活码');
  const row = await env.DB.prepare('SELECT * FROM codes WHERE code=?').bind(code).first();
  if (!row) return bad('激活码无效');
  if (row.used_by && row.used_by !== user.id) return bad('该激活码已被其他账号使用');

  if (!(Number(row.days) > 0)) return bad('该激活码已停用，请联系客服换取新码');
  const days = Number(row.days);
  const plan = planForDays(days, user.plan);
  const quota = Math.max(CARD_QUOTA, user.class_quota || 0); // 老账号已有名额不回收
  const cur = user.expire_at && user.expire_at > nowSec() ? user.expire_at : nowSec();
  const expireAt = cur + days * 86400; // 未到期顺延，已到期从当前时刻起算
  await env.DB.prepare('UPDATE users SET plan=?, class_quota=?, expire_at=?, status=? WHERE id=?')
    .bind(plan, quota, expireAt, 'active', user.id).run();
  await env.DB.prepare('UPDATE codes SET used_by=?, used_at=? WHERE code=?').bind(user.id, nowSec(), code).run();
  await logAction(env, user.id, 'redeem', code, request);
  return ok({ user: await userPublic(env, user.id) });
}

// ---------------- 班级 ----------------
function classPublic(c, online, lastSeen) {
  return {
    id: c.id, name: c.name, note: c.note || '', pairCode: c.pair_code,
    deviceName: c.device_name || '', deviceVer: c.device_ver || '',
    lastSeen: Number(lastSeen || 0) || (c.last_seen || 0), online: !!online, createdAt: c.created_at
  };
}

async function apiClasses(request, env) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  const list = await env.DB.prepare('SELECT * FROM classes WHERE user_id=? ORDER BY sort, id').bind(user.id).all();
  const out = [];
  for (const c of list.results || []) {
    let online = false;
    let lastSeen = 0;
    try {
      const st = await roomState(env, c.id);
      online = (st.online || 0) > 0;
      lastSeen = Number(st.lastSeen || 0);
    } catch (e) {}
    const today = await env.DB.prepare(
      'SELECT COUNT(*) AS n FROM messages WHERE class_id=? AND created_at>?'
    ).bind(c.id, Math.floor(new Date().setHours(0, 0, 0, 0) / 1000)).first();
    out.push({ ...classPublic(c, online, lastSeen), todayCount: today ? today.n : 0 });
  }
  return ok({ classes: out, quota: user.class_quota, used: out.length });
}

async function apiClassCreate(request, env) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  const b = await readBody(request);
  const name = String(b.name || '').trim().slice(0, 20) || '未命名班级';
  const used = await env.DB.prepare('SELECT COUNT(*) AS n FROM classes WHERE user_id=?').bind(user.id).first();
  if (used && used.n >= (user.class_quota || 1)) return bad('班级名额已满（每张卡 1 个班级），如需多个班级请联系客服');
  let code = pairCode(8);
  for (let i = 0; i < 5; i++) {
    const dup = await env.DB.prepare('SELECT id FROM classes WHERE pair_code=?').bind(code).first();
    if (!dup) break;
    code = pairCode(8);
  }
  const res = await env.DB.prepare(
    'INSERT INTO classes (user_id, name, note, pair_code, sort, created_at) VALUES (?,?,?,?,?,?)'
  ).bind(user.id, name, '', code, 0, nowSec()).run();
  await logAction(env, user.id, 'class_create', name, request);
  return ok({ id: res.meta.last_row_id, pairCode: code });
}

async function apiClassUpdate(request, env) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  const b = await readBody(request);
  const id = Number(b.id || 0);
  const cls = await env.DB.prepare('SELECT * FROM classes WHERE id=? AND user_id=?').bind(id, user.id).first();
  if (!cls) return bad('班级不存在');
  const name = String(b.name === undefined ? cls.name : b.name).trim().slice(0, 20);
  const note = String(b.note === undefined ? cls.note : b.note).slice(0, 40);
  await env.DB.prepare('UPDATE classes SET name=?, note=? WHERE id=?').bind(name, note, id).run();
  return ok({});
}

async function apiClassDelete(request, env) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  const b = await readBody(request);
  const id = Number(b.id || 0);
  const cls = await env.DB.prepare('SELECT * FROM classes WHERE id=? AND user_id=?').bind(id, user.id).first();
  if (!cls) return bad('班级不存在');
  await env.DB.prepare('UPDATE classes SET device_id=NULL, device_name=NULL, last_seen=0 WHERE id=?').bind(id).run();
  await env.DB.prepare('DELETE FROM classes WHERE id=?').bind(id).run();
  await env.DB.prepare('DELETE FROM messages WHERE class_id=?').bind(id).run();
  await env.DB.prepare('DELETE FROM schedules WHERE class_id=?').bind(id).run();
  await logAction(env, user.id, 'class_delete', cls.name, request);
  return ok({});
}

// 踢掉某班当前在线设备：先推 bye（告知原因），再断 ws
async function kickRoom(env, classId, reason) {
  try {
    const stub = env.ROOM.get(env.ROOM.idFromName('class:' + classId));
    const r = await stub.fetch('https://room/kick?reason=' + encodeURIComponent(reason || 'unbind'));
    return await r.json().catch(() => ({}));
  } catch (e) {
    return {};
  }
}

async function apiClassResetCode(request, env) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  const b = await readBody(request);
  const id = Number(b.id || 0);
  const cls = await env.DB.prepare('SELECT * FROM classes WHERE id=? AND user_id=?').bind(id, user.id).first();
  if (!cls) return bad('班级不存在');
  const code = pairCode(8);
  await env.DB.prepare('UPDATE classes SET pair_code=?, device_id=NULL, device_name=NULL WHERE id=?').bind(code, id).run();
  await env.DB.prepare('DELETE FROM device_tokens WHERE class_id=?').bind(id).run();
  await kickRoom(env, id, 'reset');
  await logAction(env, user.id, 'class_resetcode', cls.name, request);
  return ok({ pairCode: code });
}

// 解除绑定教室电脑（保留绑定码，可立即用原码重新绑定）
async function apiClassUnbind(request, env) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  const b = await readBody(request);
  const id = Number(b.id || 0);
  const cls = await env.DB.prepare('SELECT * FROM classes WHERE id=? AND user_id=?').bind(id, user.id).first();
  if (!cls) return bad('班级不存在');
  await env.DB.prepare('DELETE FROM device_tokens WHERE class_id=?').bind(id).run();
  await env.DB.prepare('UPDATE classes SET device_id=NULL, device_name=NULL WHERE id=?').bind(id).run();
  const k = await kickRoom(env, id, 'unbind');
  await logAction(env, user.id, 'class_unbind', cls.name + ' / ' + (cls.device_name || ''), request);
  return ok({ unbound: true, deviceName: cls.device_name || '', kicked: Number(k.count || 0) });
}

// ---------------- 发送 / 记录 ----------------
async function apiNotify(request, env, ctx) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  const b = await readBody(request);
  const classId = Number(b.classId || 0);
  const cls = await env.DB.prepare('SELECT * FROM classes WHERE id=? AND user_id=?').bind(classId, user.id).first();
  if (!cls) return bad('班级不存在');
  const r = await sendNotify(env, ctx, user, cls, b.text, { voice: b.voice, repeat: b.repeat });
  if (!r.ok) return bad(r.error);
  return ok({ id: r.id });
}

async function apiMessages(request, env, url) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  const classId = Number(url.searchParams.get('classId') || 0);
  const limit = Math.min(50, Number(url.searchParams.get('limit') || 20));
  const rows = classId
    ? await env.DB.prepare('SELECT * FROM messages WHERE user_id=? AND class_id=? ORDER BY id DESC LIMIT ?').bind(user.id, classId, limit).all()
    : await env.DB.prepare('SELECT * FROM messages WHERE user_id=? ORDER BY id DESC LIMIT ?').bind(user.id, limit).all();
  // 兜底：长时间没回执的消息判失败（手机端显示“播报失败”），并在本次返回结果里同步状态
  {
    const rowsAll = rows.results || [];
    const nowS = nowSec();
    const nowMs = Date.now();
    const staleIds = rowsAll.filter((m) =>
      (m.status === 'sent' && nowS - (m.created_at || 0) > StaleSentSec)
      || (m.status === 'acked' && (m.ack_at || 0) > 0 && nowMs - m.ack_at > StaleAckedMs)
    ).map((m) => m.id);
    if (staleIds.length) {
      try {
        await env.DB.prepare("UPDATE messages SET status='failed' WHERE id IN (" + staleIds.map(() => '?').join(',') + ")")
          .bind(...staleIds).run();
        for (const m of rowsAll) if (staleIds.indexOf(m.id) >= 0) m.status = 'failed';
      } catch (e) { }
    }
  }
  const cls = await env.DB.prepare('SELECT id,name FROM classes WHERE user_id=?').bind(user.id).all();
  const nameMap = {};
  for (const c of cls.results || []) nameMap[c.id] = c.name;
  const list = (rows.results || []).map((m) => ({
    id: m.id, classId: m.class_id, className: nameMap[m.class_id] || '',
    text: m.text, voice: m.voice, repeat: m.repeat, kind: m.kind,
    status: m.status, acked: !!m.ack_at, ackAt: m.ack_at,
    played: !!m.played_at, playedAt: m.played_at,
    hasAudio: !!m.audio_key, createdAt: m.created_at
  }));
  return ok({ messages: list });
}

const DEFAULT_TEMPLATES = [
  { title: '收材料', text: '请各组组长收齐材料后交到办公室。' },
  { title: '课代表', text: '请课代表下课到办公室。' },
  { title: '值日提醒', text: '请今天的值日同学完成教室卫生，关好门窗。' },
  { title: '放学通知', text: '请同学们收拾好书包，按顺序放学，路上注意安全。' },
  { title: '安静提醒', text: '请同学们保持安静，认真自习。' },
  { title: '交作业', text: '请各组同学把作业交到前排，课代表整理。' }
];

async function apiTemplates(request, env, ctx) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  if (request.method === 'GET') {
    const rows = await env.DB.prepare('SELECT * FROM templates WHERE user_id=? ORDER BY sort, id').bind(user.id).all();
    return ok({ templates: rows.results || [], defaults: DEFAULT_TEMPLATES });
  }
  const b = await readBody(request);
  if (b.action === 'delete') {
    await env.DB.prepare('DELETE FROM templates WHERE id=? AND user_id=?').bind(Number(b.id || 0), user.id).run();
    return ok({});
  }
  const title = String(b.title || '').trim().slice(0, 12);
  const text = String(b.text || '').trim().slice(0, MAX_TEXT);
  if (!title || !text) return bad('请填写模板名称和内容');
  const res = await env.DB.prepare('INSERT INTO templates (user_id, title, text, sort, created_at) VALUES (?,?,?,?,?)')
    .bind(user.id, title, text, 0, nowSec()).run();
  return ok({ id: res.meta.last_row_id });
}

function nextRunAt(hhmm, weekdays, mode, onceAt, fromTs) {
  const [hh, mm] = String(hhmm).split(':').map((x) => parseInt(x, 10));
  if (mode === 'once') {
    return onceAt && onceAt > fromTs ? onceAt : null;
  }
  const days = String(weekdays || '').split(',').map((x) => parseInt(x, 10)).filter((x) => x >= 0 && x <= 6);
  if (!days.length) return null;
  const base = new Date(fromTs * 1000);
  for (let i = 0; i < 8; i++) {
    const d = new Date(base.getTime() + i * 86400000);
    const dow = d.getUTCDay();
    if (!days.includes(dow)) continue;
    const cand = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate(), hh - 8, mm, 0);
    if (cand / 1000 > fromTs) return cand / 1000;
  }
  return null;
}

// 单次任务修改时间时，保留原执行日期，仅替换当天的 HH:MM（按东八区）
function shiftOnceAtTime(onceAt, hhmm) {
  const [hh, mm] = String(hhmm).split(':').map((x) => parseInt(x, 10));
  const cn = new Date((Number(onceAt) + 8 * 3600) * 1000);
  return Date.UTC(cn.getUTCFullYear(), cn.getUTCMonth(), cn.getUTCDate(), hh - 8, mm, 0) / 1000;
}

async function apiSchedules(request, env, ctx) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  if (request.method === 'GET') {
    const rows = await env.DB.prepare('SELECT * FROM schedules WHERE user_id=? ORDER BY next_at').bind(user.id).all();
    const seen = new Set();
    for (const r of rows.results || []) {
      if (r.enabled && r.class_id && !seen.has(r.class_id)) { seen.add(r.class_id); await rescheduleRoom(env, r.class_id); }
    }
    return ok({ schedules: rows.results || [] });
  }
  const b = await readBody(request);
  if (b.action === 'delete') {
    const old = await env.DB.prepare('SELECT * FROM schedules WHERE id=? AND user_id=?').bind(Number(b.id || 0), user.id).first();
    await env.DB.prepare('DELETE FROM schedules WHERE id=? AND user_id=?').bind(Number(b.id || 0), user.id).run();
    if (old) await rescheduleRoom(env, old.class_id);
    return ok({});
  }
  if (b.action === 'toggle') {
    const id = Number(b.id || 0);
    const s = await env.DB.prepare('SELECT * FROM schedules WHERE id=? AND user_id=?').bind(id, user.id).first();
    if (!s) return bad('任务不存在');
    const enabled = s.enabled ? 0 : 1;
    const next = enabled ? nextRunAt(s.time_hhmm, s.weekdays, s.mode, s.once_at, nowSec()) : null;
    await env.DB.prepare('UPDATE schedules SET enabled=?, next_at=? WHERE id=?').bind(enabled, next || nowSec() + 86400, id).run();
    await rescheduleRoom(env, s.class_id);
    return ok({ enabled });
  }
  if (b.action === 'update') {
    const id = Number(b.id || 0);
    const old = await env.DB.prepare('SELECT * FROM schedules WHERE id=? AND user_id=?').bind(id, user.id).first();
    if (!old) return bad('任务不存在');
    const classId = Number(b.classId || 0);
    const cls = await env.DB.prepare('SELECT id FROM classes WHERE id=? AND user_id=?').bind(classId, user.id).first();
    if (!cls) return bad('班级不存在');
    const text = String(b.text || '').trim().slice(0, MAX_TEXT);
    if (!text) return bad('请填写通知内容');
    const time = String(b.time || '08:00');
    if (!/^\d{1,2}:\d{2}$/.test(time)) return bad('时间格式应为 HH:MM');
    const [hh, mm] = time.split(':').map((x) => parseInt(x, 10));
    if (!(hh >= 0 && hh <= 23 && mm >= 0 && mm <= 59)) return bad('时间格式应为 HH:MM');
    const mode = b.mode === 'once' ? 'once' : 'weekly';
    const weekdays = String(b.weekdays || old.weekdays || '1,2,3,4,5');
    let onceAt = mode === 'once' ? Number(b.onceAt || old.once_at || 0) : null;
    if (mode === 'once' && onceAt) onceAt = shiftOnceAtTime(onceAt, time);
    const next = nextRunAt(time, weekdays, mode, onceAt, nowSec());
    if (!next) return bad(mode === 'once' ? '该单次任务已过执行时间，请删除后重新添加' : '无法计算下一次执行时间，请检查设置');
    await env.DB.prepare(
      'UPDATE schedules SET class_id=?, text=?, time_hhmm=?, weekdays=?, mode=?, once_at=?, next_at=? WHERE id=? AND user_id=?'
    ).bind(classId, text, time, weekdays, mode, onceAt, next, id, user.id).run();
    if (Number(old.class_id) !== classId) await rescheduleRoom(env, old.class_id);
    await rescheduleRoom(env, classId);
    await logAction(env, user.id, 'edit_sched', '#' + id, request);
    return ok({ id, nextAt: next });
  }
  const classId = Number(b.classId || 0);
  const cls = await env.DB.prepare('SELECT * FROM classes WHERE id=? AND user_id=?').bind(classId, user.id).first();
  if (!cls) return bad('班级不存在');
  const text = String(b.text || '').trim().slice(0, MAX_TEXT);
  if (!text) return bad('请填写通知内容');
  const time = String(b.time || '08:00');
  if (!/^\d{1,2}:\d{2}$/.test(time)) return bad('时间格式应为 HH:MM');
  const mode = b.mode === 'once' ? 'once' : 'weekly';
  const weekdays = String(b.weekdays || '1,2,3,4,5');
  const onceAt = mode === 'once' ? Number(b.onceAt || 0) : null;
  const next = nextRunAt(time, weekdays, mode, onceAt, nowSec());
  if (!next) return bad('无法计算下一次执行时间，请检查设置');
  const res = await env.DB.prepare(
    'INSERT INTO schedules (user_id, class_id, text, time_hhmm, weekdays, mode, once_at, enabled, next_at, created_at) VALUES (?,?,?,?,?,?,?,1,?,?)'
  ).bind(user.id, classId, text, time, weekdays, mode, onceAt, next, nowSec()).run();
  await rescheduleRoom(env, classId);
  return ok({ id: res.meta.last_row_id, nextAt: next });
}

async function apiPreview(request, env) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  const b = await readBody(request);
  const bytes = await synthAudio(env, String(b.text || ''));
  if (!bytes) return bad('语音合成失败');
  return new Response(bytes, { headers: { 'content-type': 'audio/wav', 'cache-control': 'no-store' } });
}

async function apiAudio(request, env, url) {
  const id = Number(url.pathname.split('/').pop() || 0);
  if (!id) return bad('参数错误');
  const msg = await env.DB.prepare('SELECT * FROM messages WHERE id=?').bind(id).first();
  if (!msg) return new Response('not found', { status: 404 });
  // 校验：教师本人 或 该班级的绑定设备
  let authed = false;
  const user = await currentUser(request, env);
  if (user && user.id === msg.user_id) authed = true;
  if (!authed) {
    const h = request.headers.get('authorization') || '';
    const tok = h.startsWith('Bearer ') ? h.slice(7).trim() : (url.searchParams.get('token') || '');
    if (tok) {
      const row = await env.DB.prepare('SELECT * FROM device_tokens WHERE token=?').bind(tok).first();
      if (row && Number(row.class_id) === Number(msg.class_id)) authed = true;
    }
  }
  if (!authed) return new Response('forbidden', { status: 403 });
  let bytes = await env.CACHE.get('a:' + id, 'arrayBuffer');
  if (!bytes) {
    const fresh = await synthAudio(env, msg.text);
    if (!fresh) return new Response('no audio', { status: 404 });
    bytes = fresh.buffer ? fresh.buffer.slice(fresh.byteOffset, fresh.byteOffset + fresh.byteLength) : fresh;
    // TTL 3 天（原 7 天）：压低 KV 存储占用；客户端另有一层本地文件缓存兜底
    await env.CACHE.put('a:' + id, bytes, { expirationTtl: 259200 });
  }
  return new Response(bytes, { headers: { 'content-type': 'audio/wav', 'cache-control': 'private, max-age=259200' } });
}

// ---------------- 教室端设备 ----------------
function deviceAuth(request) {
  const h = request.headers.get('authorization') || '';
  return h.startsWith('Bearer ') ? h.slice(7).trim() : '';
}

async function apiDeviceActivate(request, env) {
  const b = await readBody(request);
  const code = String(b.code || '').trim().toUpperCase();
  if (!code) return bad('请输入绑定码');
  const cls = await env.DB.prepare('SELECT * FROM classes WHERE pair_code=?').bind(code).first();
  if (!cls) return bad('绑定码不存在，请核对后重试');
  const user = await env.DB.prepare('SELECT * FROM users WHERE id=?').bind(cls.user_id).first();
  if (!user || user.status !== 'active') return bad('该绑定码已停用');
  const token = randomHex(32);
  // 同班级旧的设备令牌作废：一台教室电脑只保留一个有效凭证
  await env.DB.prepare('DELETE FROM device_tokens WHERE class_id=?').bind(cls.id).run();
  await env.DB.prepare('INSERT INTO device_tokens (token, class_id, user_id, name, created_at, last_seen) VALUES (?,?,?,?,?,?)')
    .bind(token, cls.id, user.id, String(b.name || '').slice(0, 32), nowSec(), nowSec()).run();
  await env.DB.prepare('UPDATE classes SET device_name=?, device_id=? WHERE id=?')
    .bind(String(b.name || '').slice(0, 32), String(b.deviceId || '').slice(0, 64), cls.id).run();
  return ok({ token, classId: cls.id, className: cls.name, config: deviceConfig(cls), ...deviceHints(env) });
}

// 教室端自助解绑：作废自己的凭证（并通知在线连接退出）
async function apiDeviceUnbind(request, env) {
  const b = await readBody(request);
  const token = deviceAuth(request) || String(b.token || '').trim();
  if (!token) return bad('缺少设备凭证');
  const row = await env.DB.prepare('SELECT * FROM device_tokens WHERE token=?').bind(token).first();
  if (!row) return ok({ unbound: false, note: '该设备凭证已失效' });
  await env.DB.prepare('DELETE FROM device_tokens WHERE token=?').bind(token).run();
  const left = await env.DB.prepare('SELECT COUNT(*) AS n FROM device_tokens WHERE class_id=?').bind(row.class_id).first();
  if (!left || !Number(left.n)) {
    await env.DB.prepare('UPDATE classes SET device_id=NULL, device_name=NULL WHERE id=?').bind(row.class_id).run();
  }
  await kickRoom(env, row.class_id, 'unbind');
  await logAction(env, row.user_id, 'device_unbind', (row.name || '') + ' #' + row.class_id, request);
  return ok({ unbound: true });
}

function deviceConfig(cls) {
  return {
    displaySeconds: cls.display_seconds || 20,
    fontSize: cls.font_size || 'auto',
    theme: cls.theme || 'blue',
    repeat: cls.repeat || 1,
    voiceRate: cls.voice_rate || 1,
    showClock: cls.show_clock === 0 ? 0 : 1,
    beep: cls.beep === 0 ? 0 : 1
  };
}

// 教室端自检信息：随设备应答增量下发（老客户端会忽略未知字段）
// base   = 对外基础地址，域名迁移只需改 wrangler.toml [vars] PUBLIC_BASE 一处
// latest = 教室端最新版本，仅用于界面提示「发现新版本」，不强制升级
function deviceHints(env) {
  const out = {};
  const b = String((env && env.PUBLIC_BASE) || '').trim().replace(/\/+$/, '');
  if (b) out.base = b;
  const v = String((env && env.DEVICE_LATEST) || '').trim();
  if (v) out.latest = v;
  return out;
}

async function apiDeviceConfig(request, env, url) {
  const token = (url && url.searchParams.get('token')) || deviceAuth(request);
  const row = await env.DB.prepare('SELECT * FROM device_tokens WHERE token=?').bind(token).first();
  if (!row) return json({ ok: false, error: 'invalid token' }, 401);
  const cls = await env.DB.prepare('SELECT * FROM classes WHERE id=?').bind(row.class_id).first();
  if (!cls) return json({ ok: false, error: 'class gone' }, 404);
  // device_tokens.last_seen 只是运维留痕（全库无读取点）：跟着 DO 的节流镜像一起写，
  // 不再每次配置/轮询都写一行（教室端断了 WS 会 5s 一轮轮询，一天就是上万行写）
  const seen = await roomSeen(env, cls.id);
  if (seen && seen.mirrored) {
    await env.DB.prepare('UPDATE device_tokens SET last_seen=? WHERE token=?').bind(nowSec(), token).run();
  }
  const ex = await classOwnerExpired(env, cls.id);
  return ok({
    classId: cls.id, className: cls.name, config: deviceConfig(cls),
    expired: ex.expired, expireAt: ex.expireAt, expiredText: ex.expired ? DEVICE_EXPIRED_TEXT : '',
    ...deviceHints(env)
  });
}

// 教室端轻量状态查询：断线/轮询时也能拿到「账号已到期」提示
async function apiDeviceStatus(request, env, url) {
  const token = url.searchParams.get('token') || deviceAuth(request);
  const row = await env.DB.prepare('SELECT * FROM device_tokens WHERE token=?').bind(token).first();
  if (!row) return json({ ok: false, error: 'invalid token' }, 401);
  const cls = await env.DB.prepare('SELECT * FROM classes WHERE id=?').bind(row.class_id).first();
  if (!cls) return json({ ok: false, error: 'class gone' }, 404);
  await roomSeen(env, cls.id);
  const ex = await classOwnerExpired(env, cls.id);
  return ok({
    className: cls.name, expired: ex.expired, expireAt: ex.expireAt,
    expiredText: ex.expired ? DEVICE_EXPIRED_TEXT : '',
    ...deviceHints(env)
  });
}

// 教室端 HTTP 轮询兜底（WS 断开时使用）：返回 since 之后的新消息
async function apiDevicePoll(request, env, url) {
  const token = url.searchParams.get('token') || deviceAuth(request);
  const since = Number(url.searchParams.get('since') || 0);
  const row = await env.DB.prepare('SELECT * FROM device_tokens WHERE token=?').bind(token).first();
  if (!row) return json({ ok: false, error: 'invalid token' }, 401);
  const cls = await env.DB.prepare('SELECT * FROM classes WHERE id=?').bind(row.class_id).first();
  if (!cls) return json({ ok: false, error: 'class gone' }, 404);
  const cls2 = await env.DB.prepare(
    'SELECT id, text, voice, repeat, audio_key, created_at FROM messages WHERE class_id=? AND id>? AND created_at>=? ORDER BY id ASC LIMIT 20'
  ).bind(row.class_id, since, nowSec() - StaleSentSec).all();
  const msgs = (cls2.results || []).map((m) => ({
    id: m.id, text: m.text, voice: m.voice, repeat: m.repeat, hasAudio: !!m.audio_key,
    createdAt: m.created_at, offline: true
  }));
  const last = msgs.length ? msgs[msgs.length - 1].id : since;
  const seen = await roomSeen(env, row.class_id);
  if (seen && seen.mirrored) {
    await env.DB.prepare('UPDATE device_tokens SET last_seen=? WHERE token=?').bind(nowSec(), token).run();
  }
  const ex = await classOwnerExpired(env, cls.id);
  return ok({
    messages: msgs, lastId: last, config: deviceConfig(cls), className: cls.name,
    expired: ex.expired, expireAt: ex.expireAt, expiredText: ex.expired ? DEVICE_EXPIRED_TEXT : '',
    ...deviceHints(env)
  });
}

// 教室端回执（HTTP 兜底路径）
async function apiDeviceAck(request, env) {
  const b = await readBody(request);
  const token = String(b.token || deviceAuth(request) || '');
  const id = Number(b.id || 0);
  if (!token || !id) return bad('参数错误');
  const row = await env.DB.prepare('SELECT * FROM device_tokens WHERE token=?').bind(token).first();
  if (!row) return json({ ok: false, error: 'invalid token' }, 401);
  await env.DB.prepare("UPDATE messages SET ack_at=?, status=? WHERE id=? AND class_id=? AND status<>'played'")
    .bind(Date.now(), 'acked', id, row.class_id).run();
  return ok({ acked: id });
}

// 教室端播报结束回执（HTTP 兜底路径；WS 断开时用）
async function apiDevicePlayed(request, env) {
  const b = await readBody(request);
  const token = String(b.token || deviceAuth(request) || '');
  const id = Number(b.id || 0);
  if (!token || !id) return bad('参数错误');
  const okPlay = b.ok === false ? 0 : 1;
  const row = await env.DB.prepare('SELECT * FROM device_tokens WHERE token=?').bind(token).first();
  if (!row) return json({ ok: false, error: 'invalid token' }, 401);
  await env.DB.prepare('UPDATE messages SET played_at=?, status=? WHERE id=? AND class_id=?')
    .bind(Date.now(), okPlay ? 'played' : 'failed', id, row.class_id).run();
  return ok({ played: id, ok: !!okPlay });
}

async function apiDeviceWs(request, env, url) {
  const token = url.searchParams.get('token') || deviceAuth(request);
  const row = await env.DB.prepare('SELECT * FROM device_tokens WHERE token=?').bind(token).first();
  if (!row) return new Response('invalid token', { status: 401 });
  const cls = await env.DB.prepare('SELECT * FROM classes WHERE id=?').bind(row.class_id).first();
  if (!cls) return new Response('class gone', { status: 404 });
  await env.DB.prepare('UPDATE device_tokens SET last_seen=? WHERE token=?').bind(nowSec(), token).run();
  const stub = env.ROOM.get(env.ROOM.idFromName('class:' + row.class_id));
  const headers = new Headers(request.headers);
  headers.set('x-class-id', String(row.class_id));
  headers.set('x-device-id', String(cls.device_id || ''));
  return stub.fetch('https://room/ws?classId=' + row.class_id + '&device=' + encodeURIComponent(cls.device_id || ''), {
    method: request.method, headers
  });
}

// ---------------- 班级配置 ----------------
async function apiClassConfig(request, env) {
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  const b = await readBody(request);
  const id = Number(b.id || 0);
  const cls = await env.DB.prepare('SELECT * FROM classes WHERE id=? AND user_id=?').bind(id, user.id).first();
  if (!cls) return bad('班级不存在');
  const num = (v, d, lo, hi) => {
    const n = Number(v);
    return isNaN(n) ? d : Math.min(hi, Math.max(lo, Math.round(n)));
  };
  await env.DB.prepare(
    'UPDATE classes SET display_seconds=?, font_size=?, theme=?, repeat=?, voice_rate=?, show_clock=?, beep=? WHERE id=?'
  ).bind(
    num(b.displaySeconds, 20, 5, 600), String(b.fontSize || 'auto').slice(0, 16), String(b.theme || 'blue').slice(0, 16),
    num(b.repeat, 1, 1, 5), num(b.voiceRate, 1, 5, 20) / 10, b.showClock ? 1 : 0, b.beep ? 1 : 0, id
  ).run();
  const fresh = await env.DB.prepare('SELECT * FROM classes WHERE id=?').bind(id).first();
  await pushToRoom(env, id, { id: 0, kind: 'config', config: deviceConfig(fresh) });
  return ok({ config: deviceConfig(fresh) });
}

// ---------------- 管理后台 ----------------
async function apiAdmin(request, env, url) {
  const b = request.method === 'POST' ? await readBody(request) : {};
  if (url.pathname === '/api/admin/login') {
    const u = await env.DB.prepare('SELECT * FROM users WHERE username=? AND role=?')
      .bind(String(b.username || '').trim(), 'admin').first();
    if (!u) return bad('账号或密码错误');
    const h = await hashPassword(String(b.password || ''), u.salt);
    if (h !== u.pass_hash) return bad('账号或密码错误');
    const token = await createSession(env, u.id);
    await logAction(env, u.id, 'admin_login', '', request);
    return ok({ token, username: u.username, role: 'admin' });
  }
  const { user, error } = await requireUser(request, env);
  if (error) return error;
  if (user.role !== 'admin') return bad('无权限', 403);

  if (url.pathname === '/api/admin/users') {
    const rows = await env.DB.prepare('SELECT id,username,role,plan,class_quota,expire_at,status,created_at,last_login FROM users ORDER BY id DESC LIMIT 200').all();
    const cs = await env.DB.prepare('SELECT user_id, COUNT(*) AS n FROM classes GROUP BY user_id').all();
    const cnt = {};
    for (const c of cs.results || []) cnt[c.user_id] = c.n;
    const list = (rows.results || []).map((u) => ({ ...u, classCount: cnt[u.id] || 0 }));
    const codes = await env.DB.prepare('SELECT * FROM codes ORDER BY created_at DESC LIMIT 100').all();
    return ok({ users: list, codes: codes.results || [] });
  }
  if (url.pathname === '/api/admin/codes') {
    const plan = ['trial', 'basic', 'pro'].includes(b.plan) ? b.plan : 'basic';
    const days = Math.min(3650, Math.max(0, Number(b.days) || 365));
    const count = Math.min(200, Math.max(1, Number(b.count) || 1));
    const bonus = Math.min(50, Math.max(0, Number(b.class_bonus) || 0));
    const batch = 'B' + nowSec().toString(36).toUpperCase();
    const out = [];
    for (let i = 0; i < count; i++) {
      const code = 'CTH-' + plan.slice(0, 1).toUpperCase() + pairCode(4) + '-' + pairCode(4);
      await env.DB.prepare('INSERT INTO codes (code, plan, class_bonus, days, batch, created_at) VALUES (?,?,?,?,?,?)')
        .bind(code, plan, bonus, days, batch, nowSec()).run();
      out.push(code);
    }
    await logAction(env, user.id, 'gen_codes', batch + ' x' + count, request);
    return ok({ codes: out, batch });
  }
  if (url.pathname === '/api/admin/user') {
    const id = Number(b.id || 0);
    const u = await env.DB.prepare('SELECT * FROM users WHERE id=?').bind(id).first();
    if (!u) return bad('用户不存在');
    if (b.action === 'disable' || b.action === 'enable') {
      await env.DB.prepare('UPDATE users SET status=? WHERE id=?').bind(b.action === 'enable' ? 'active' : 'disabled', id).run();
    } else if (b.action === 'extend') {
      const days = Number(b.days);
      if (days !== 3 && days !== 365) return bad('续期时长只能选择 3 天或 365 天');
      const base = Math.max(nowSec(), u.expire_at || 0);
      await env.DB.prepare('UPDATE users SET expire_at=?, status=? WHERE id=?').bind(base + days * 86400, 'active', id).run();
    } else if (b.action === 'setplan') {
      const plan = ['trial', 'basic', 'pro'].includes(b.plan) ? b.plan : 'basic';
      await env.DB.prepare('UPDATE users SET plan=?, class_quota=? WHERE id=?').bind(plan, planQuota(plan) + (Number(b.class_bonus) || 0), id).run();
    } else if (b.action === 'reset_password') {
      const mode = b.mode === 'random' ? 'random' : 'manual';
      let password;
      if (mode === 'random') {
        password = randomTempPassword(12);
      } else {
        password = String(b.password || '');
        if (password.length < 6) return bad('密码至少 6 位');
        if (password.length > 128) return bad('密码最长 128 位');
      }
      const salt = randomHex(16);
      const passHash = await hashPassword(password, salt);
      await env.DB.prepare('UPDATE users SET salt=?, pass_hash=? WHERE id=?').bind(salt, passHash, id).run();

      // 重置密码后让已有登录失效；若管理员改自己的密码，保留当前会话，避免刚改完就被踢下线
      const auth = request.headers.get('authorization') || '';
      const currentToken = auth.startsWith('Bearer ') ? auth.slice(7).trim() : '';
      let sessionsCleared = 0;
      if (id === user.id && currentToken) {
        const del = await env.DB.prepare('DELETE FROM sessions WHERE user_id=? AND token<>?').bind(id, currentToken).run();
        sessionsCleared = (del.meta && del.meta.changes) || 0;
      } else {
        const del = await env.DB.prepare('DELETE FROM sessions WHERE user_id=?').bind(id).run();
        sessionsCleared = (del.meta && del.meta.changes) || 0;
      }
      await logAction(env, user.id, 'admin_reset_password', 'uid=' + id + ' mode=' + mode + ' sessions=' + sessionsCleared, request);
      return ok({
        username: u.username,
        mode,
        generated: mode === 'random',
        password: mode === 'random' ? password : undefined,
        sessionsCleared
      });
    } else if (b.action === 'delete') {
      await env.DB.prepare('DELETE FROM classes WHERE user_id=?').bind(id).run();
      await env.DB.prepare('DELETE FROM device_tokens WHERE user_id=?').bind(id).run();
      await env.DB.prepare('DELETE FROM templates WHERE user_id=?').bind(id).run();
      await env.DB.prepare('DELETE FROM schedules WHERE user_id=?').bind(id).run();
      await env.DB.prepare('DELETE FROM sessions WHERE user_id=?').bind(id).run();
      await env.DB.prepare('DELETE FROM users WHERE id=?').bind(id).run();
    } else {
      return bad('未知操作');
    }
    await logAction(env, user.id, 'admin_' + b.action, 'uid=' + id, request);
    return ok({});
  }
  return bad('not found', 404);
}

// ---------------- 路由 ----------------
function match(path, pattern) {
  const a = path.split('/').filter(Boolean);
  const b = pattern.split('/').filter(Boolean);
  if (a.length !== b.length) return null;
  const params = {};
  for (let i = 0; i < a.length; i++) {
    if (b[i].startsWith(':')) params[b[i].slice(1)] = decodeURIComponent(a[i]);
    else if (a[i] !== b[i]) return null;
  }
  return params;
}

async function route(request, env, ctx) {
  const url = new URL(request.url);
  const p = url.pathname;
  const m = request.method;
  const key = m + ' ' + p;

  if (p === '/healthz') return json({ ok: true, v: VERSION, t: nowSec() });

  // 教室端版本信息（无需鉴权）：latest 仅用于界面提示，不强制升级
  if (p === '/api/version') {
    const hints = deviceHints(env);
    return ok({
      app: 'classroom', api: VERSION, latest: hints.latest || '',
      download: '/download/yundianchuanhuatong-setup.exe'
    });
  }

  // DO alarm 触发的内部调度入口（仅服务内部调用，密钥校验）
  if (m === 'POST' && p === '/internal/tick') {
    const k = url.searchParams.get('key') || '';
    if (!env.CRON_KEY || k !== env.CRON_KEY) return bad('forbidden', 403);
    const cid = Number(url.searchParams.get('class_id') || 0);
    ctx.waitUntil(runSchedules(env, ctx, cid));
    return ok({ started: true, classId: cid });
  }

  if (m === 'POST' && p === '/api/device/activate') return apiDeviceActivate(request, env);
  if (m === 'POST' && p === '/api/device/unbind') return apiDeviceUnbind(request, env);
  if (m === 'GET' && p === '/api/device/config') return apiDeviceConfig(request, env, url);
  if (m === 'GET' && p === '/api/device/status') return apiDeviceStatus(request, env, url);
  if (m === 'GET' && p === '/api/device/poll') return apiDevicePoll(request, env, url);
  if (m === 'POST' && p === '/api/device/ack') return apiDeviceAck(request, env);
  if (m === 'POST' && p === '/api/device/played') return apiDevicePlayed(request, env);
  if (p === '/api/device/ws') return apiDeviceWs(request, env, url);

  if (p.startsWith('/api/admin/')) return apiAdmin(request, env, url);

  if (m === 'POST' && p === '/api/register') return apiRegister(request, env, ctx);
  if (m === 'POST' && p === '/api/login') return apiLogin(request, env);
  if (m === 'POST' && p === '/api/redeem') return apiRedeem(request, env);
  if (m === 'POST' && p === '/api/logout') return apiLogout(request, env);

  if (p.startsWith('/api/audio/')) return apiAudio(request, env, url);
  if (m === 'POST' && p === '/api/preview') return apiPreview(request, env);

  const authed = [
    ['GET', '/api/me', (rq, e, u) => apiMe(rq, e, u)],
    ['GET', '/api/classes', (rq, e) => apiClasses(rq, e)],
    ['POST', '/api/classes', (rq, e) => apiClassCreate(rq, e)],
    ['POST', '/api/classes/rename', (rq, e) => apiClassUpdate(rq, e)],
    ['POST', '/api/classes/update', (rq, e) => apiClassUpdate(rq, e)],
    ['POST', '/api/classes/delete', (rq, e) => apiClassDelete(rq, e)],
    ['POST', '/api/classes/paircode', (rq, e) => apiClassResetCode(rq, e)],
    ['POST', '/api/classes/resetcode', (rq, e) => apiClassResetCode(rq, e)],
    ['POST', '/api/classes/unbind', (rq, e) => apiClassUnbind(rq, e)],
    ['POST', '/api/classes/config', (rq, e, u, x) => apiClassConfig(rq, e)],
    ['POST', '/api/notify', (rq, e, u, x) => apiNotify(rq, e, x)],
    ['GET', '/api/messages', (rq, e, u, url2) => apiMessages(rq, e, url2)],
    ['GET', '/api/templates', (rq, e, u, x) => apiTemplates(rq, e, x)],
    ['POST', '/api/templates', (rq, e, u, x) => apiTemplates(rq, e, x)],
    ['GET', '/api/schedules', (rq, e, u, x) => apiSchedules(rq, e, x)],
    ['POST', '/api/schedules', (rq, e, u, x) => apiSchedules(rq, e, x)]
  ];
  for (const [mm, pp, fn] of authed) {
    if (m === mm && p === pp) {
      const { user, error } = await requireUser(request, env);
      if (error) return error;
      return fn(request, env, user, url);
    }
  }

  if (p.startsWith('/api/')) return bad('接口不存在', 404);

  // 静态资源：找不到的页面回落到 H5 首页
  const asset = await env.ASSETS.fetch(request);
  if (asset.status === 404 && (request.headers.get('accept') || '').includes('text/html')) {
    return env.ASSETS.fetch(new Request(new URL('/index.html', url.origin), request));
  }
  return asset;
}

async function apiMe(request, env, user) {
  const cs = await env.DB.prepare('SELECT * FROM classes WHERE user_id=? ORDER BY id').bind(user.id).all();
  const classes = [];
  for (const c of cs.results || []) {
    const st = await roomState(env, c.id);
    classes.push({
      id: c.id, name: c.name, pairCode: c.pair_code, deviceName: c.device_name,
      lastSeen: c.last_seen, online: !!st.online, config: deviceConfig(c)
    });
  }
  const exp = user.expire_at;
  return ok({
    user: { id: user.id, username: user.username, plan: user.plan, quota: user.class_quota, expireAt: exp, expired: exp ? exp < nowSec() : false, role: user.role },
    classes, serverTime: nowSec()
  });
}

// 到点自动喊话：处理到期定时任务 + 兜底维护（cron 与 DO alarm 共用）
// classId>0 = DO alarm 路径，只扫本班任务；原实现每个班的 alarm 都全表扫 30 条，
// 同一分钟 N 个班到点就会把同一批任务抢到 N 次、重复喊话（首次全量任务时必现）
// 再用 next_at 做乐观锁原子抢占，即使 cron 与 alarm 撞在一起也只有一路能发出去
let _schedBusy = false;      // 同一 isolate 内串行，避免自己跟自己并发
let _lastMaint = 0;          // 维护任务（清日志/判失败）节流时间戳
const MAINT_EVERY_MS = 5 * 60 * 1000;

async function runSchedules(env, ctx, classId) {
  const cid = Number(classId || 0);
  if (_schedBusy) return;
  _schedBusy = true;
  try {
    const t = nowSec();
    const due = cid
      ? await env.DB.prepare('SELECT * FROM schedules WHERE enabled=1 AND next_at<=? AND class_id=? LIMIT 30').bind(t, cid).all()
      : await env.DB.prepare('SELECT * FROM schedules WHERE enabled=1 AND next_at<=? LIMIT 30').bind(t).all();
    for (const s of due.results || []) {
      // 原子抢占：先把下次执行时间写掉，抢占成功（changes=1）才发送
      // next_at 为空 = 一次性任务已到期 → 停用并把 next_at 推到一天后占位
      const next = nextRunAt(s.time_hhmm, s.weekdays, s.mode, s.once_at, t);
      const claim = await env.DB.prepare('UPDATE schedules SET next_at=?, enabled=? WHERE id=? AND next_at=?')
        .bind(next || t + 86400, next ? 1 : 0, s.id, s.next_at).run();
      if (!claim.meta || !claim.meta.changes) continue;
      try {
        const cls = await env.DB.prepare('SELECT * FROM classes WHERE id=?').bind(s.class_id).first();
        const user = await env.DB.prepare('SELECT * FROM users WHERE id=?').bind(s.user_id).first();
        if (cls && user && user.status === 'active' && !isExpired(user)) {
          const r = await sendNotify(env, ctx, user, cls, s.text, { voice: 1, repeat: 1, kind: 'schedule' });
          if (r && r.dropped) {
            try {
              await env.DB.prepare('INSERT INTO logs (user_id, action, detail, ip, created_at) VALUES (?,?,?,?,?)')
                .bind(user.id, 'schedule_dropped', '教室端离线，定时喊话已丢弃：' + s.text, '', t).run();
            } catch (e) { }
          }
        }
      } catch (e) {}
    }
    // 维护任务：DO alarm 才是主要触发源（免费版 cron 名额已满），所以 class 路径也要跑，
    // 但 5 分钟节流一次，别让每个班的 alarm 都删一遍日志
    if (!cid || Date.now() - _lastMaint > MAINT_EVERY_MS) {
      _lastMaint = Date.now();
      // 清理 7 天前的日志
      await env.DB.prepare('DELETE FROM logs WHERE created_at < ?').bind(t - 7 * 86400).run();
      // 兜底：接受了却没有回执的消息 → 判失败（手机端“播报失败”），教室端回来也不补发
      // created_at 是秒；ack_at 是毫秒
      try {
        await env.DB.prepare("UPDATE messages SET status='failed' WHERE status='sent' AND created_at < ?")
          .bind(t - StaleSentSec).run();
        await env.DB.prepare("UPDATE messages SET status='failed' WHERE status='acked' AND ack_at > 0 AND ack_at < ?")
          .bind(Date.now() - StaleAckedMs).run();
      } catch (e) { }
    }
  } finally {
    _schedBusy = false;
  }
}

export default {
  async fetch(request, env, ctx) {
    try {
      return await route(request, env, ctx);
    } catch (e) {
      return json({ ok: false, error: '服务异常：' + (e && e.message ? e.message : 'unknown') }, 500);
    }
  },

  // 定时任务：到点自动喊话 + 过期用户标记
  async scheduled(event, env, ctx) {
    ctx.waitUntil(runSchedules(env, ctx));
  }
};
