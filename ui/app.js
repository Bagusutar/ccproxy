'use strict';

const KEY = new URLSearchParams(location.search).get('k') || '';
const $ = (id) => document.getElementById(id);
const esc = (s) => String(s).replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

async function api(path, body) {
  const res = await fetch(path, {
    method: body ? 'POST' : 'GET',
    headers: { 'Content-Type': 'application/json', 'X-CCProxy-Key': KEY },
    body: body ? JSON.stringify(body) : undefined,
  });
  return res.json();
}

/* ---------- 状态 ---------- */

const state = {
  providers: [], slots: {}, slotDefs: [],
  port: 15722, retryWatchdog: true,
  firstByteSec: 95, stallSec: 60,
  settingsPath: '', autostart: false,
  active: null,
  probing: {}, probeMsg: {}, altOpen: {},
  modelStatus: {},   // providerID -> { model: true | '错误文本' }
  testedAt: {},
  prices: {},          // 模型 -> {in,cacheW,cacheR,out}，手填单价
  effPrices: {},
  usage: null, usageLoading: false, meter: null,
  usageFilters: {
    meter: { applied: { from: '', to: '', preset: 'all' }, draft: { from: '', to: '' } },
    usage: { applied: { from: '', to: '', preset: 'all' }, draft: { from: '', to: '' } },
  },
  usageRequest: 0,
  dirty: false,
};

// 名称选填：留空时用地址域名，与后端保持一致
function displayName(p, i) {
  if (p.name && p.name.trim()) return p.name.trim();
  try {
    const h = new URL(p.baseUrl).hostname;
    if (h) return h.replace(/^api\./, '');
  } catch (_) { /* 地址还没填完 */ }
  return `上游 ${i + 1}`;
}

function nextProviderId() {
  const used = new Set(state.providers.map((p) => p.id));
  for (;;) {
    const bytes = new Uint8Array(12);
    crypto.getRandomValues(bytes);
    const id = 'p_' + [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('');
    if (!used.has(id)) return id;
  }
}

function markDirty() { state.dirty = true; $('dirty').classList.add('show'); }

/* ---------- toast ---------- */

let toastTimer = null;
function toast(text, isErr) {
  const t = $('toast');
  t.textContent = text;
  t.className = 'toast show' + (isErr ? ' err' : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.className = 'toast'; }, 2600);
}

// busy 是「操作进行中」的提示：不自动消失，由调用方在结束时显式清除。
//
// 普通 toast 2.6 秒就隐藏，而保存要把每个槽位模型都实调一次、再等代理
// 起来，通常好几秒。提示先没了，界面上只剩一个禁用的按钮，用户无从判断
// 是在做事还是卡死了。等待提示必须活到操作真正结束。
function busy(text) {
  const t = $('toast');
  t.textContent = text;
  t.className = 'toast show';
  clearTimeout(toastTimer);
}

function busyDone() {
  clearTimeout(toastTimer);
  $('toast').className = 'toast';
}

/* ---------- Mac 风格确认框 ---------- */
// 不用浏览器原生 confirm()：它会顶着「127.0.0.1:xxxx 显示」的标题栏。

function dialog({ title, message = '', okText = '确定', danger = false, alert = false, input }) {
  return new Promise((resolve) => {
    const box = $('modalBackdrop'), ok = $('modalOk'), inp = $('modalInput');
    $('modalTitle').textContent = title;
    $('modalMsg').textContent = message;
    $('modalMsg').style.display = message ? '' : 'none';
    ok.textContent = okText;
    ok.classList.toggle('danger', danger);
    $('modalCancel').hidden = alert;
    inp.hidden = input === undefined;
    if (input !== undefined) inp.value = input;
    box.hidden = false;
    (input !== undefined ? inp : ok).focus();

    const done = (v) => {
      box.hidden = true;
      ok.onclick = null; $('modalCancel').onclick = null; box.onkeydown = null;
      resolve(input !== undefined && v ? inp.value.trim() : v);
    };
    // 监听挂在模态框元素上而非 document，不做全局按键捕获
    box.onkeydown = (e) => {
      if (e.key === 'Escape') { e.stopPropagation(); done(false); }
      if (e.key === 'Enter') { e.stopPropagation(); done(true); }
    };
    ok.onclick = () => done(true);
    $('modalCancel').onclick = () => done(false);
  });
}

/* ---------- 侧边栏 ---------- */

function renderNav() {
  const only = state.providers.length <= 1;
  $('providerNav').innerHTML = state.providers.map((p, i) => {
    const st = state.modelStatus[p.id] || {};
    const tested = Object.keys(st).length;
    const bad = Object.values(st).some((v) => v !== true);
    const cls = !(p.models || []).length ? 'dot' : (tested && bad ? 'dot' : 'dot ok');
    return `
    <div class="side-item" data-pane="${esc(p.id)}" data-drag="${esc(p.id)}">
      <span class="grip">⠿</span>
      <i class="${cls}"></i>
      <span class="name">${esc(displayName(p, i))}</span>
      <button class="side-del" data-del="${esc(p.id)}" ${only ? 'disabled' : ''} title="删除此上游">
        <svg width="12" height="12" viewBox="0 0 14 14" fill="none" stroke="currentColor"
             stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M2 3.5h10M5.5 3.5V2.2h3v1.3M3.2 3.5l.6 8.3h6.4l.6-8.3M5.8 6v3.6M8.2 6v3.6"/>
        </svg>
      </button>
      ${i === 0 ? '<span class="badge-mini">默认</span>' : ''}
    </div>`;
  }).join('');

  document.querySelectorAll('.side-item').forEach((b) => {
    b.classList.toggle('active', b.dataset.pane === state.active);
    b.onclick = () => selectPane(b.dataset.pane);
  });
  document.querySelectorAll('.side-del').forEach((btn) => {
    btn.onclick = (e) => { e.stopPropagation(); deleteProvider(btn.dataset.del); };
  });
  wireReorder();
}

function selectPane(name) {
  state.active = name;
  if (name === 'usage' && !state.usage && !state.usageLoading) loadUsage();
  document.querySelectorAll('.side-item').forEach((b) => b.classList.toggle('active', b.dataset.pane === name));
  document.querySelectorAll('.page').forEach((p) => p.classList.toggle('active', p.dataset.pane === name));
  const isProv = state.providers.some((p) => p.id === name);
  $('delProvider').hidden = !isProv || state.providers.length <= 1;
  $('delProvider').onclick = () => deleteProvider(name);
}

// 拖拽排序：顺序即优先级，拖到最上面即成为默认上游。
// 刻意不加显式提示——按住条目直接拖即可，手柄悬停才显形。
//
// 用 Pointer Events 而不是 HTML5 drag-and-drop：后者要走系统的 OLE 拖放循环，
// 在 WebView2 里并不可靠，而且 body 上的 user-select:none 会抑制拖拽起始。
// 这里只需要同一列表内换位，用不上 DataTransfer 的任何能力。
function wireReorder() {
  const items = [...document.querySelectorAll('[data-drag]')];
  if (items.length < 2) return;
  const sidebar = document.querySelector('.sidebar');
  const clear = () => items.forEach((el) => el.classList.remove('drop-above', 'drop-below', 'dragging'));

  items.forEach((el) => {
    el.addEventListener('pointerdown', (e) => {
      if (e.button !== 0 || e.target.closest('.side-del')) return;

      const startY = e.clientY;
      let active = false;   // 超过阈值才算拖拽，否则这一下仍是普通点击
      let target = null;    // 落点条目
      let above = false;

      const move = (ev) => {
        if (!active) {
          if (Math.abs(ev.clientY - startY) < 4) return;
          active = true;
          el.setPointerCapture(e.pointerId);
          el.classList.add('dragging');
          sidebar.classList.add('reordering');
        }
        target = null;
        items.forEach((it) => it.classList.remove('drop-above', 'drop-below'));
        for (const it of items) {
          if (it === el) continue;
          const r = it.getBoundingClientRect();
          if (ev.clientY < r.top || ev.clientY > r.bottom) continue;
          target = it;
          above = ev.clientY < r.top + r.height / 2;
          it.classList.add(above ? 'drop-above' : 'drop-below');
          break;
        }
      };

      const cleanup = () => {
        document.removeEventListener('pointermove', move);
        document.removeEventListener('pointerup', up);
        document.removeEventListener('pointercancel', cancel);
        el.removeEventListener('lostpointercapture', cancel);
        sidebar.classList.remove('reordering');
      };
      const cancel = () => { cleanup(); clear(); };
      const up = () => {
        cleanup();
        if (!active) return;
        // 已判定为拖拽，抑制随之而来的 click，避免顺手切换了面板
        el.addEventListener('click', (ev) => ev.stopPropagation(), { capture: true, once: true });

        if (!target) { clear(); return; }
        const from = state.providers.findIndex((x) => x.id === el.dataset.drag);
        const moved = state.providers.splice(from, 1)[0];
        let to = state.providers.findIndex((x) => x.id === target.dataset.drag);
        if (!above) to += 1;
        state.providers.splice(to, 0, moved);
        clear(); markDirty(); renderAll();
      };

      document.addEventListener('pointermove', move);
      document.addEventListener('pointerup', up);
      document.addEventListener('pointercancel', cancel);
      el.addEventListener('lostpointercapture', cancel);
    });
  });
}

/* ---------- 上游页 ---------- */

function ago(ts) {
  const s = Math.floor((Date.now() - ts) / 1000);
  if (s < 60) return '刚刚';
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
  return `${Math.floor(s / 3600)} 小时前`;
}

function statusLine(p) {
  const st = state.modelStatus[p.id] || {};
  const n = (p.models || []).length;
  if (!n) return '<span class="pill">尚未获取模型列表</span>';
  const tested = Object.keys(st).length;
  const good = Object.values(st).filter((v) => v === true).length;
  const bad = tested - good;
  const out = [];
  if (tested) {
    out.push(`<span class="pill ${bad ? 'b-err' : 'b-ok'}">● ${good} 可用${bad ? ` · ${bad} 失败` : ''}</span>`);
    out.push(`<span class="pill">${n} 个模型${n > tested ? ` · ${n - tested} 未测` : ''}</span>`);
  } else {
    out.push(`<span class="pill">${n} 个模型 · 未测</span>`);
  }
  if (state.testedAt[p.id]) out.push(`<span>${ago(state.testedAt[p.id])}测过</span>`);
  return out.join('');
}

function shortErr(s) {
  const m = String(s).match(/HTTP (\d{3})/);
  return m ? m[1] : '失败';
}

function renderProviders() {
  $('providerPanes').innerHTML = state.providers.map((p, i) => {
    const probing = state.probing[p.id];
    const msg = state.probeMsg[p.id];
    const n = (p.models || []).length;
    // 已填过就一直展开；否则听用户手动展开的临时状态
    const altOn = !!(p.openaiBaseUrl || state.altOpen[p.id]);
    const st = state.modelStatus[p.id] || {};
    const chips = (p.models || []).map((m) => {
      const v = st[m];
      const cls = v === undefined ? 'unk' : (v === true ? 'ok' : 'err');
      const sym = v === undefined ? '?' : (v === true ? '✓' : '!');
      const code = v === undefined ? '未测' : (v === true ? '' : shortErr(v));
      const tip = typeof v === 'string' ? ` title="${esc(v)}"` : '';
      return `<span class="chip ${cls}"${tip}><i class="st">${sym}</i>${esc(m)}` +
        (code ? `<span class="code">${esc(code)}</span>` : '') +
        `<button class="x" data-rmmodel="${esc(p.id)}|${esc(m)}" title="从列表移除">×</button></span>`;
    }).join('');

    return `
    <section class="page" data-pane="${esc(p.id)}">
      <div class="page-head">
        <h1>${esc(displayName(p, i))}</h1>
        <div class="statusline">${statusLine(p)}</div>
      </div>

      <details class="note">
        <summary>关于上游与优先级</summary>
        <div class="inner">
          地址必须是 API 根地址（可填写或省略结尾的 <code>/v1</code>）。ccproxy 会并行探测 Anthropic Messages、OpenAI Chat Completions 与 Responses 三种协议；请求优先原生透传，必要时才转换。<br>
          列表可拖拽排序，<b>第一位同时作为默认上游</b>：模型名未匹配到任何上游时，请求转发至该上游。
        </div>
      </details>

      <div class="card-title">连接</div>
      <div class="card">
        <div class="row">
          <div class="row-label">名称</div>
          <div class="row-main">
            <input type="text" data-p="${esc(p.id)}" data-f="name"
                   value="${esc(p.name || '')}" placeholder="${esc(displayName(p, i))}">
          </div>
        </div>
        <div class="row-foot">选填。留空时使用地址的域名</div>

        <div class="row">
          <div class="row-label">地址</div>
          <div class="row-main">
            <input type="text" class="mono" data-p="${esc(p.id)}" data-f="baseUrl"
                   value="${esc(p.baseUrl || '')}" placeholder="https://…">
          </div>
        </div>
        <div class="row-foot">填写 <code>/v1</code> 之前的部分，例如 <code>https://api.anthropic.com</code></div>

        <div class="row">
          <div class="row-label">凭证</div>
          <div class="row-main">
            <span class="secret">
              <input type="password" class="mono" data-p="${esc(p.id)}" data-f="token"
                     value="${esc(p.token || '')}" placeholder="sk-…">
              <span class="acts">
                <button class="eye" data-eye title="显示">
                  <svg width="15" height="15" viewBox="0 0 16 16" fill="none" stroke="currentColor"
                       stroke-width="1.2" stroke-linecap="round">
                    <path d="M1.6 8S4 3.8 8 3.8 14.4 8 14.4 8 12 12.2 8 12.2 1.6 8 1.6 8z"/>
                    <circle cx="8" cy="8" r="2"/>
                    <path class="slash" d="M2.6 13.4 13.4 2.6"/>
                  </svg>
                </button>
                <button class="in-copy" data-copysecret title="复制">
                  <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor"
                       stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round">
                    <rect x="5.4" y="5.4" width="8.2" height="8.2" rx="1.8"/>
                    <path d="M10.6 5.4V4.2a1.8 1.8 0 0 0-1.8-1.8H4.2a1.8 1.8 0 0 0-1.8 1.8v4.6a1.8 1.8 0 0 0 1.8 1.8h1.2"/>
                  </svg>
                </button>
              </span>
            </span>
          </div>
        </div>

        <div class="row">
          <div class="row-label">OpenAI 端点</div>
          <div class="row-main">
            <button class="switch${altOn ? ' on' : ''}" data-altsw="${esc(p.id)}"></button>
          </div>
        </div>
        <div class="row-foot">
          默认与上方地址相同。<b>仅当该上游的 OpenAI 接口不在上方地址下时</b>，才需要单独指定。
          多数上游两种格式共用同一地址，无需填写此项。
        </div>
        ${altOn ? `
        <div class="row">
          <div class="row-label">OpenAI 地址</div>
          <div class="row-main">
            <input type="text" class="mono" data-p="${esc(p.id)}" data-f="openaiBaseUrl"
                   value="${esc(p.openaiBaseUrl || '')}" placeholder="${esc(p.baseUrl || 'https://…')}">
          </div>
        </div>
        <div class="row-foot">
          留空时与上方地址相同。
          例如 DeepSeek 的 Anthropic 端点在 <code>https://api.deepseek.com/anthropic</code>，
          OpenAI 端点在 <code>https://api.deepseek.com</code>，两套接口位于不同地址。
        </div>` : ''}
      </div>

      <div class="card-title">上游声明的模型</div>
      <div class="card">
        <div class="chip-bar">
          <button class="btn" data-probe="${esc(p.id)}" ${probing ? 'disabled' : ''}>${probing ? '获取中…' : '获取模型列表'}</button>
          <button class="btn" data-testall="${esc(p.id)}" ${n && !probing ? '' : 'disabled'}>连通性测试</button>
          <div class="spacer"></div>
          <span class="hint-mut">${msg ? esc(msg.text) : ''}</span>
        </div>
        <div class="chips">${chips}<button class="chip add" data-addmodel="${esc(p.id)}">＋ 手动添加</button></div>
        <div class="card-foot">
          这是上游 <code>/v1/models</code> 返回的目录，<b>不代表每个都能调用</b>：部分网关目录里
          列出的模型在调用时返回 404。「连通性测试」逐个发送一次最小请求实际验证，结果标注在模型名上。
          目录中没有的模型可手动添加。
        </div>
      </div>
    </section>`;
  }).join('');
  wireProviderEvents();
}

function wireProviderEvents() {
  document.querySelectorAll('[data-p]').forEach((el) => {
    el.oninput = () => {
      const p = state.providers.find((x) => x.id === el.dataset.p);
      if (!p) return;
      p[el.dataset.f] = el.value;
      // 换了地址或凭证，之前探测出的结论就不再作数了。留着它，路由会按
      // 旧结论决定该不该翻译——比如原上游的模型只有 responses、新上游
      // 原生支持 messages，代理仍会去翻译，换来一个莫名其妙的 404。
      // countTokens 同理：旧上游支持、新上游不支持，代理会照旧转发，
      // 换来一个 404。清掉即回到「未探测」，路由原样透传，由上游如实报错。
      if (el.dataset.f === 'baseUrl' || el.dataset.f === 'openaiBaseUrl' ||
          el.dataset.f === 'token') {
        delete p.modelProtocols;
        delete p.countTokens;
        delete state.modelStatus[p.id];
        delete state.testedAt[p.id];
      }
      markDirty();
      if (el.dataset.f !== 'token') {
        renderNav();
        const h = el.closest('.page').querySelector('h1');
        if (h) h.textContent = displayName(p, state.providers.indexOf(p));
      }
    };
  });

  document.querySelectorAll('[data-altsw]').forEach((btn) => {
    btn.onclick = () => {
      const id = btn.dataset.altsw;
      const pv = state.providers.find((x) => x.id === id);
      const on = !!((pv || {}).openaiBaseUrl || state.altOpen[id]);
      state.altOpen[id] = !on;
      // 关掉即清空：否则会留下一个界面上看不见、却仍在生效的地址
      if (on && pv && pv.openaiBaseUrl) { pv.openaiBaseUrl = ''; markDirty(); }
      renderAll();
    };
  });

  document.querySelectorAll('[data-eye]').forEach((btn) => {
    btn.onclick = () => {
      const inp = btn.closest('.secret').querySelector('input');
      const show = inp.type === 'password';
      inp.type = show ? 'text' : 'password';
      btn.classList.toggle('revealed', show);
      btn.title = show ? '隐藏' : '显示';
    };
  });

  // 凭证不进 toast，避免明文被截屏带走；只用按钮变绿作确认。
  document.querySelectorAll('[data-copysecret]').forEach((btn) => {
    btn.onclick = async () => {
      const inp = btn.closest('.secret').querySelector('input');
      if (!inp.value) return;
      await copyText(inp.value);
      btn.classList.add('done');
      btn.title = '已复制';
      setTimeout(() => { btn.classList.remove('done'); btn.title = '复制'; }, 1400);
    };
  });

  document.querySelectorAll('[data-probe]').forEach((b) => { b.onclick = () => probeProvider(b.dataset.probe); });
  document.querySelectorAll('[data-testall]').forEach((b) => { b.onclick = () => testAllModels(b.dataset.testall); });

  document.querySelectorAll('[data-rmmodel]').forEach((b) => {
    b.onclick = () => {
      const [pid, model] = b.dataset.rmmodel.split('|');
      const p = state.providers.find((x) => x.id === pid);
      if (!p) return;
      p.models = p.models.filter((m) => m !== model);
      if (state.modelStatus[pid]) delete state.modelStatus[pid][model];
      markDirty(); renderAll();
    };
  });

  document.querySelectorAll('[data-addmodel]').forEach((b) => {
    b.onclick = async () => {
      const name = await dialog({
        title: '手动添加模型',
        message: '填入上游模型目录中未列出、但实际可用的模型名。',
        okText: '添加', input: '',
      });
      if (!name) return;
      const p = state.providers.find((x) => x.id === b.dataset.addmodel);
      if (!p) return;
      if (!p.models.includes(name)) p.models.push(name);
      p.models.sort();
      markDirty(); renderAll();
    };
  });
}

async function deleteProvider(id) {
  if (state.providers.length <= 1) return;
  const p = state.providers.find((x) => x.id === id);
  if (!p) return;
  const ok = await dialog({
    title: `删除「${displayName(p, state.providers.indexOf(p))}」？`,
    message: '引用该上游的模型位将一并清空。点击「保存并应用」前不会写入配置。',
    okText: '删除', danger: true,
  });
  if (!ok) return;
  state.providers = state.providers.filter((x) => x.id !== id);
  Object.keys(state.slots).forEach((k) => { if (state.slots[k].provider === id) delete state.slots[k]; });
  if (state.active === id) state.active = state.providers[0].id;
  markDirty(); renderAll();
}

/* ---------- 探测 ---------- */

async function probeProvider(id) {
  const p = state.providers.find((x) => x.id === id);
  if (!p) return;
  state.probing[id] = true;
  state.probeMsg[id] = { text: '获取中…' };
  renderProviders(); selectPane(state.active);
  try {
    const r = await api('/api/probe', {
      baseUrl: p.baseUrl, openaiBaseUrl: p.openaiBaseUrl || '', token: p.token,
    });
    if (r.ok) {
      p.models = r.models;
      p.countTokens = r.countTokens;
      state.probeMsg[id] = { text: `${r.models.length} 个模型 · ` + (r.live ? `连通 ${r.liveMs}ms` : '消息端点不通') };
      toast(r.live ? `已获取 ${r.models.length} 个模型 · 连通 ${r.liveMs}ms（${r.liveModel}）`
                   : `已获取 ${r.models.length} 个模型，但消息端点不通`, !r.live);
      markDirty();
    } else {
      state.probeMsg[id] = { text: '获取失败' };
      toast(r.error || '获取失败', true);
    }
  } catch (e) {
    state.probeMsg[id] = { text: '获取失败' };
    toast(String(e), true);
  } finally {
    state.probing[id] = false;
    renderAll();
  }
}

// 逐个验证目录里的每个模型。目录声明 ≠ 可调用。
async function testAllModels(id) {
  const p = state.providers.find((x) => x.id === id);
  if (!p || !(p.models || []).length) return;
  const n = p.models.length;
  const known = Object.keys(p.modelProtocols || {}).length;
  const ok = await dialog({
    title: `测试 ${n} 个模型的连通性？`,
    message: '对每个模型各发送一次 max_tokens=1 的最小请求，单次约 10 个输入 + 1 个输出 token。' +
             '\n\n结果保存在本地配置中，「保存并应用」时直接复用，不再重测；' +
             '修改地址或凭证后自动失效。' +
             (known ? `\n\n已有 ${known} 个模型的结论，本次将全部重测。` : '') +
             (n > 300 ? '\n\n超过 300 个的部分将被跳过。' : ''),
    okText: '开始',
  });
  if (!ok) return;

  state.probeMsg[id] = { text: '测试中…' };
  renderProviders(); selectPane(state.active);
  try {
    const r = await api('/api/testmodels', {
      baseUrl: p.baseUrl, openaiBaseUrl: p.openaiBaseUrl || '', token: p.token, models: p.models,
    });
    if (!r.ok) { toast(r.error || '测试失败', true); return; }
    const st = {};
    // 探测出的方言只写进配置供路由使用，界面上不展示——用户只需要知道通不通。
    const protos = { ...(p.modelProtocols || {}) };
    let good = 0;
    r.results.forEach((x) => {
      st[x.model] = x.ok ? true : (x.error || '失败');
      if (x.ok) { good++; protos[x.model] = x.protocols || []; }
      else delete protos[x.model];
    });
    p.modelProtocols = protos;
    state.modelStatus[id] = st;
    state.testedAt[id] = Date.now();
    state.probeMsg[id] = { text: `${good}/${r.results.length} 可用` };
    toast(`${good} 个可用、${r.results.length - good} 个失败` + (r.skipped ? `，${r.skipped} 个未测` : ''));
  } catch (e) {
    toast(String(e), true);
  } finally {
    renderAll();
  }
}

/* ---------- 模型分配 ---------- */

// DELEGATE_PROMPT 供用户粘贴到 CLAUDE.md，约束主模型何时委派子 Agent。
// 与「子 Agent」模型位相邻：选择模型与决定委派时机是同一件事的两个方面。
const DELEGATE_PROMPT = `## Task Delegation

Use only built-in agents unless explicitly requested otherwise. Only the top-level agent delegates; subagents execute directly.

The top-level agent owns user intent, requirements, business rules, architecture, boundaries, contracts, compatibility, risk decisions, final synthesis, and communication. Optimize API cost without materially reducing correctness.

### When to Delegate

Keep work top-level when delegation overhead exceeds the work, or when it is tightly coupled to conversation context, ambiguous/interactive, or requires decisions about scope, architecture, contracts, compatibility, risk, or final conclusions.

Otherwise delegate self-contained work when parallelism, independent review, or isolating substantial repository reading, logs, experiments, or context clearly helps.

### Model Selection

Default to \`haiku\` when the goal, decisions, boundaries, approach, and verification are clear. Length, runtime, or file count alone do not justify \`opus\`.

Use \`opus\` when correctness depends on substantial judgment: unclear/conflicting requirements; architecture, boundaries, public APIs, compatibility, or major dependencies; auth/security/privacy/payments/concurrency/transactions/persisted data/data integrity; competing root-cause hypotheses; cross-subsystem effects; or hard-to-verify failures.

Opus-level work normally stays top-level. Use child \`opus\` only when isolated context provides clear value: extensive investigation, independent analysis/review, parallel investigation, or medium/high-risk review. Difficulty alone is insufficient. Always specify the model.

| Agent | Model | Use |
|---|---|---|
| \`Explore\` | \`haiku\` | Targeted read-only search, tracing, facts/evidence |
| \`general-purpose\` | \`haiku\` | Clear implementation, tests/builds/logs/docs, mechanical refactors |
| \`Plan\` | \`opus\` | Self-contained judgment-heavy read-only planning/investigation |
| \`general-purpose\` | \`opus\` | Hard debugging, security, concurrency, data integrity, cross-system analysis, high-risk review |

### Delegation Contract

Every delegation must include:
- Goal
- Relevant context/fixed decisions
- Boundaries and non-goals
- Acceptance criteria and verification method
- Return format
- Escalation conditions

A Haiku subagent may resolve ordinary implementation/build/test failures within the approved approach, but must stop and return evidence if completion requires changing the approach or scope, inventing business rules, altering contracts, adding unapproved dependencies, or accepting unverified risk.

Return concise conclusions, evidence, changed files, verification results, and unresolved risks—not working transcripts unless requested.

Parallelize only genuinely independent tasks; do not duplicate work. Use a separate Opus reviewer only for medium/high-risk changes. Low-risk, objectively verified work may be reviewed top-level.
`;


function renderSlots() {
  const any = state.providers.some((p) => (p.models || []).length);
  const groups = [
    { id: 'direct', title: '默认模型', foot:
      '填写后立即生效；留空则沿用 Claude Code 自身的默认选择。<br>' +
      '「1M 上下文」在模型名后附加 <code>[1M]</code>，由 Claude Code 转成扩展上下文的请求头；' +
      '该能力仅依赖请求头，上游若丢弃该头会静默失效。' },
    { id: 'alias', title: '模型路由', cap: '· 决定 /model opus 等内置别名对应的实际模型' },
  ];

  $('slotList').innerHTML = groups.map((g) => {
    const defs = state.slotDefs.filter((d) => d.group === g.id);
    if (!defs.length) return '';
    return `<div class="card-title">${esc(g.title)}${g.cap ? `<span class="cap">${esc(g.cap)}</span>` : ''}</div>
      <div class="card">${defs.map((sd) => slotRow(sd, any)).join('')}
      ${g.foot ? `<div class="card-foot">${g.foot}</div>` : ''}</div>
      ${g.id === 'direct' ? delegateNote() : ''}`;
  }).join('') + (any ? '' :
    '<div class="card"><div class="card-foot">尚无上游获取过模型列表，请先在左侧选择上游并获取。</div></div>');

  document.querySelectorAll('[data-combo]').forEach(wireCombo);
  // 这个按钮是渲染时才插进来的，页面加载时那轮全局绑定盖不到它。
  wireCopyButtons($('slotList'));
  document.querySelectorAll('[data-onem]').forEach((b) => {
    b.onclick = () => {
      const k = b.dataset.onem;
      if (!state.slots[k]) return;
      state.slots[k].oneM = !state.slots[k].oneM;
      markDirty(); renderSlots(); renderDiff();
    };
  });
  renderPreview();
}

// delegateNote 渲染「默认模型」组下方的折叠说明。
// 内置 Agent 的模型由委派规则显式指定，不再写 CLAUDE_CODE_SUBAGENT_MODEL。
function delegateNote() {
  return `<details class="note" style="margin-top:2px">
    <summary>内置 Agent 委派规则<span class="cap">· 粘贴到 CLAUDE.md，统一模型选择与委派边界</span></summary>
    <div class="inner">
      以下规则要求主模型按 Agent 类型显式选择模型，不修改 Claude Code 的全局 subagent 设置，可直接并入任何 CLAUDE.md。
      <div class="codewrap">
        <button class="btn code-copy" data-copy="delegateBox">复制</button>
        <pre class="code" id="delegateBox">${esc(DELEGATE_PROMPT)}</pre>
      </div>
    </div>
  </details>`;
}

function slotRow(sd, any) {
  const cur = state.slots[sd.key] || {};
  const prov = cur.provider ? state.providers.find((x) => x.id === cur.provider) : null;
  const label = cur.model
    ? `<span class="combo-prov">${esc(prov ? displayName(prov, state.providers.indexOf(prov)) : '?')}</span>${esc(cur.model)}`
    : '<span class="combo-empty">保持默认</span>';
  const warn = sd.key === 'haiku' && !cur.model
    ? `<div class="inline-warn"><div><b>haiku 必须设置。</b>
       Claude Code 每轮都会用它发送后台请求；未设置时会使用内置的默认 Haiku 模型名，
       默认上游若无该模型，每轮后台请求都会静默失败。</div></div>` : '';
  return `
    <div class="row">
      <div class="row-label">${esc(sd.label)}</div>
      <div class="row-main">
        <div class="combo" data-combo="${esc(sd.key)}">
          <button class="combo-btn" ${any ? '' : 'disabled'}>
            <span class="combo-val">${label}</span>
            <svg class="chev" width="10" height="10" viewBox="0 0 10 10" fill="none"
                 stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><path d="M2.5 4L5 6.5 7.5 4"/></svg>
          </button>
          <div class="combo-pop" hidden>
            <input class="combo-search" type="text" spellcheck="false" placeholder="搜索模型…">
            <div class="combo-list"></div>
          </div>
        </div>
        <span class="switch-wrap">
          <button class="switch accent${cur.oneM ? ' on' : ''}" data-onem="${esc(sd.key)}" ${cur.model ? '' : 'disabled'}></button>
          <span class="lbl">1M 上下文</span>
        </span>
      </div>
    </div>
    <div class="row-foot">${esc(sd.hint)}<span class="env">${esc(sd.env)}</span></div>
    ${warn}`;
}

// 「生效结果」：把分散的配置合成一句话，用户不必在脑子里做映射
function renderPreview() {
  const dp = state.providers[0];
  const dpName = dp ? displayName(dp, 0) : '（未配置）';
  const cell = (key) => {
    const s = state.slots[key];
    if (!s || !s.model) return `<td class="dim">未指定，路由至默认上游 ${esc(dpName)}</td>`;
    const p = state.providers.find((x) => x.id === s.provider);
    return `<td class="mono">${esc(p ? displayName(p, state.providers.indexOf(p)) : '?')} · ${esc(s.model)}` +
           (s.oneM ? ' <span class="pill" style="margin-left:6px">1M</span>' : '') + '</td>';
  };
  const rows = [
    ['会话启动', 'main'],
    ['后台及显式 Haiku 请求', 'haiku'],
    ['<span class="mono">/model opus</span>', 'opus'],
    ['<span class="mono">/model sonnet</span>', 'sonnet'],
    ['<span class="mono">/model fable</span>', 'fable'],
  ];
  $('prevBody').innerHTML = rows.map(([t, k]) => `<tr><td>${t}</td><td class="arrow">→</td>${cell(k)}</tr>`).join('');
}

/* ---------- 可搜索的模型下拉 ---------- */

function wireCombo(root) {
  const key = root.dataset.combo;
  const btn = root.querySelector('.combo-btn');
  const pop = root.querySelector('.combo-pop');
  const search = root.querySelector('.combo-search');
  const list = root.querySelector('.combo-list');

  const all = [];
  state.providers.forEach((p, i) => {
    (p.models || []).forEach((m) => all.push({ pid: p.id, pname: displayName(p, i), model: m }));
  });
  let active = 0;

  function draw(filter) {
    const q = filter.trim().toLowerCase();
    // 模型名与上游名都参与匹配，方便按来源筛选
    const hits = q ? all.filter((x) => x.model.toLowerCase().includes(q) || x.pname.toLowerCase().includes(q)) : all;
    active = 0;
    const cur = state.slots[key] || {};
    let html = '<div class="combo-item clear" data-clear>保持 Claude Code 默认</div>';
    let lastP = null;
    hits.forEach((x) => {
      if (x.pid !== lastP) { html += `<div class="combo-group">${esc(x.pname)}</div>`; lastP = x.pid; }
      const on = cur.provider === x.pid && cur.model === x.model;
      html += `<div class="combo-item${on ? ' on' : ''}" data-pid="${esc(x.pid)}" data-m="${esc(x.model)}">${esc(x.model)}</div>`;
    });
    if (!hits.length) html += '<div class="combo-none">无匹配</div>';
    list.innerHTML = html;
    list.querySelectorAll('.combo-item').forEach((el) => {
      el.onmousedown = (e) => {
        e.preventDefault();
        if (el.dataset.clear !== undefined) delete state.slots[key];
        else state.slots[key] = { provider: el.dataset.pid, model: el.dataset.m, oneM: !!(state.slots[key] || {}).oneM };
        pop.hidden = true; markDirty(); renderSlots(); renderDiff();
      };
    });
  }

  function open() {
    document.querySelectorAll('.combo-pop').forEach((x) => { x.hidden = true; });
    pop.hidden = false;
    search.value = '';
    draw('');

    // fixed 定位：宽度对齐按钮，方向取上下空间较大的一侧，
    // 列表高度按该侧的实际余量收缩，永远不会越出窗口。
    const r = btn.getBoundingClientRect();
    const GAP = 4, EDGE = 12, CHROME = 34; // CHROME = 搜索框高度 + 弹层内边距
    const below = window.innerHeight - r.bottom - GAP - EDGE;
    const above = r.top - GAP - EDGE;
    const up = below < 200 && above > below;

    pop.style.left = r.left + 'px';
    pop.style.width = r.width + 'px';
    if (up) {
      pop.style.top = 'auto';
      pop.style.bottom = (window.innerHeight - r.top + GAP) + 'px';
    } else {
      pop.style.bottom = 'auto';
      pop.style.top = (r.bottom + GAP) + 'px';
    }
    list.style.maxHeight = Math.max(96, Math.min(280, (up ? above : below) - CHROME)) + 'px';

    search.focus();
  }

  btn.onclick = (e) => { e.stopPropagation(); pop.hidden ? open() : (pop.hidden = true); };
  search.oninput = () => draw(search.value);
  search.onkeydown = (e) => {
    const items = [...list.querySelectorAll('.combo-item')];
    if (e.key === 'Escape') { pop.hidden = true; btn.focus(); return; }
    if (e.key === 'Enter') { e.preventDefault(); if (items[active]) items[active].onmousedown(e); return; }
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      active = Math.max(0, Math.min(items.length - 1, active + (e.key === 'ArrowDown' ? 1 : -1)));
      items.forEach((el, i) => el.classList.toggle('hl', i === active));
      if (items[active]) items[active].scrollIntoView({ block: 'nearest' });
    }
  };
  root.onclick = (e) => e.stopPropagation();
}

document.addEventListener('click', () => {
  document.querySelectorAll('.combo-pop').forEach((x) => { x.hidden = true; });
  $('statusPop').classList.remove('show');
  $('statusBtn').classList.remove('open');
});

// 弹层是 fixed 定位的，页面滚动或窗口缩放后它不会跟着锚点走，直接收起。
// 用捕获阶段：滚动发生在 .scroll 上，不会冒泡到 document。
// 但必须放过弹层内部列表自己的滚动，否则一滚模型列表弹层就关了。
const closeCombos = () => {
  document.querySelectorAll('.combo-pop:not([hidden])').forEach((x) => { x.hidden = true; });
};
document.addEventListener('scroll', (e) => {
  if (e.target instanceof Element && e.target.closest('.combo-pop')) return;
  closeCombos();
}, true);
window.addEventListener('resize', closeCombos);

/* ---------- 用量与花费 ---------- */

const fmtN = (n) => Number(n || 0).toLocaleString('en-US');
const fmtM = (v) => '$' + Number(v || 0).toFixed(2);

// token 数缩写。写全了单列就是 11 个字符（852,526,881），八列铺开必然横向
// 滚动，主次反而看不清。精确值挂在 title 上，需要时悬停可查。
function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (n >= 1e4) return Math.round(n / 1e3) + 'K';
  return fmtN(n);
}
const tokCell = (n) => `<td class="n tok" title="${fmtN(n)}">${fmtTok(n)}</td>`;
// 合计块里也要缩写：1,035,352,153 这种写全了，窗口一窄整块就折行，
// 而且和下面表格里的 1.04B 对不上号。
const totCell = (label, n) =>
  `<div class="cell" title="${fmtN(n)}"><span class="n">${fmtTok(n)}</span>` +
  `<span class="unit">${label}</span></div>`;

// 单价按有效数字截断显示。预设里 DeepSeek 是人民币折过来的，
// 原样铺开是 0.14084507042253522——输入框装不下，也没人需要那么多位。
const pnum = (v) => (v === undefined || v === null ? '' : String(Number(Number(v).toPrecision(4))));

// 扫描是按需触发的：首次要读遍整个会话记录目录（实测 184 MB / 2.6 秒），
// 之后后端按文件的 size+mtime 走缓存，只解析新追加的部分。
function clearUsageView(error) {
  state.usage = null;
  state.meter = null;
  state.effPrices = {};
  $('meterTotal').innerHTML = `<div class="usage-empty usage-error"><strong>无法读取代理流量</strong><br>${esc(error)}</div>`;
  $('usageTotal').innerHTML = `<div class="usage-empty usage-error"><strong>无法读取 Claude Code 用量</strong><br>${esc(error)}</div>`;
  $('meterTable').innerHTML = '';
  $('usageTable').innerHTML = '';
  $('priceTable').innerHTML = '';
  $('priceEmpty').hidden = false;
  $('priceEmpty').textContent = '用量读取失败，暂时无法列出单价模型。';
  $('usageFoot').textContent = '';
  $('meterSince').textContent = '';
}

function setUsageLoading(on) {
  document.querySelectorAll('[data-range-card] .usage-card-body').forEach((body) => {
    body.classList.toggle('is-loading', on);
    body.setAttribute('aria-busy', String(on));
  });
}

async function loadUsage() {
  const requestID = ++state.usageRequest;
  state.usageLoading = true;
  setUsageLoading(true);
  const refresh = $('refreshUsage');
  if (!state.usage && !state.meter) {
    $('meterTotal').innerHTML = '<div class="usage-empty">读取中…</div>';
    $('usageTotal').innerHTML = '<div class="usage-empty">读取中…</div>';
  }
  if (refresh) { refresh.disabled = true; refresh.textContent = '刷新中…'; }
  try {
    const qs = new URLSearchParams();
    for (const name of ['meter', 'usage']) {
      const f = state.usageFilters[name].applied;
      if (f.from) qs.set(name + 'From', f.from);
      if (f.to) qs.set(name + 'To', f.to);
    }
    const r = await api('/api/usage' + (qs.toString() ? '?' + qs : ''));
    if (requestID !== state.usageRequest) return;
    if (!r.ok) {
      clearUsageView(r.error || '统计失败');
      return;
    }
    state.usage = r.usage;
    state.meter = r.meter || { rows: [], cost: 0 };
    state.effPrices = { ...(state.effPrices || {}), ...(r.prices || {}) };
    renderUsage();
  } catch (e) {
    if (requestID !== state.usageRequest) return;
    clearUsageView(e && e.message ? e.message : '网络错误');
  } finally {
    if (requestID === state.usageRequest) {
      state.usageLoading = false;
      setUsageLoading(false);
      if (refresh) { refresh.disabled = false; refresh.textContent = '刷新用量'; }
    }
  }
}

function renderUsage() {
  renderMeter();
  const u = state.usage;
  if (!u) return;

  // 若历史记录未保留缓存分类，而代理实测有缓存用量，只标记为估算，
  // 不猜测拆分，也不改后端 token 数。
  const meterByModel = new Map();
  const meterRange = state.usageFilters.meter.applied;
  const usageRange = state.usageFilters.usage.applied;
  const comparableRanges = meterRange.from === usageRange.from && meterRange.to === usageRange.to;
  if (comparableRanges) {
    (state.meter?.rows || []).forEach((r) => {
      const prev = meterByModel.get(r.model) || { cacheW: 0, cacheR: 0 };
      prev.cacheW += Number(r.cacheW || 0);
      prev.cacheR += Number(r.cacheR || 0);
      meterByModel.set(r.model, prev);
    });
  }
  const unclassified = new Set((u.models || [])
    .filter((m) => {
      if (!comparableRanges) return false;
      const meter = meterByModel.get(m.model);
      const sessionCache = Number(m.cacheW || 0) + Number(m.cacheR || 0);
      const meteredCache = Number(meter?.cacheW || 0) + Number(meter?.cacheR || 0);
      return meteredCache > sessionCache;
    })
    .map((m) => m.model));
  const cacheWarning = unclassified.size > 0;
  const warningModels = [...unclassified].sort();
  const warningText = cacheWarning
    ? '部分模型缓存分类未保留，估算可能偏高'
    : '';

  // 与代理计量那张表同样的处理：没有可统计的记录时，用一句说明占住位置，
  // 而不是画一张只有表头和一行全零「合计」的表。
  if (!(u.models || []).length) {
    $('usageTotal').innerHTML =
      `<div class="usage-empty claude-empty"><strong>还没有可统计的 Claude Code 会话记录</strong><br>`
      + '使用 Claude Code 产生对话后，这里会按模型列出全部历史用量。<br>'
      + `日期不完整的记录不计入日期筛选。</div>`;
    $('usageTable').innerHTML = '';
    const usageFilter = state.usageFilters.usage.applied;
    const noDateHint = (usageFilter.from || usageFilter.to)
      ? ((u.noDate || []).length ? '部分记录没有日期，未纳入当前日期范围。' : '')
      : ((u.noDate || []).length ? '包含没有日期的记录。' : '');
    $('usageFoot').innerHTML = '数据来自本机 Claude Code 用量记录。' +
      (noDateHint ? `<br><span class="range-note">${noDateHint}</span>` : '');
    renderPrices();
    return;
  }

  const t = u.total || {};
  $('usageTotal').innerHTML = `
    <div class="cell"><span class="big">${esc(fmtM(u.cost))}</span>
      <span class="unit">估算花费${u.unpriced ? ' · 部分模型未设单价' : ''}${warningText ? ` · ${warningText}` : ''}</span></div>
    <div class="cell"><span class="n">${fmtN(t.msgs)}</span><span class="unit">消息</span></div>
    ${totCell('输入', t.in)}${totCell('缓存写', t.cacheW)}
    ${totCell('缓存读', t.cacheR)}${totCell('输出', t.out)}`;

  const head = `<tr class="head"><th>模型</th><th>消息</th><th>输入</th><th>缓存写</th>` +
               `<th>缓存读</th><th>输出</th><th>花费</th></tr>`;
  const rows = (u.models || []).map((m) => `
    <tr>
      <td class="m">${esc(m.model)}</td>
      <td class="n">${fmtN(m.msgs)}</td>
      ${tokCell(m.in)}${tokCell(m.cacheW)}${tokCell(m.cacheR)}${tokCell(m.out)}
      <td class="n${unclassified.has(m.model) ? ' usage-estimated' : ''}"${unclassified.has(m.model) ? ' title="缓存分类未保留，费用为估算"' : ''}>${m.priced ? (unclassified.has(m.model) ? '≈' : '') + esc(fmtM(m.cost)) : '未设单价'}</td>
    </tr>`).join('');

  $('usageTable').innerHTML = `<tbody>${head}${rows}
    <tr class="sum"><td>合计</td><td class="n">${fmtN(t.msgs)}</td>
    ${tokCell(t.in)}${tokCell(t.cacheW)}${tokCell(t.cacheR)}${tokCell(t.out)}
    <td class="n${cacheWarning ? ' usage-estimated' : ''}"${cacheWarning ? ' title="部分模型缓存分类未保留，合计费用可能偏高"' : ''}>${cacheWarning ? '≈' : ''}${esc(fmtM(u.cost))}</td></tr></tbody>`;

  renderPrices();

  const usageFilter = state.usageFilters.usage.applied;
  const noDateHint = (usageFilter.from || usageFilter.to)
    ? ((u.noDate || []).length ? '部分记录没有日期，未纳入当前日期范围。' : '')
    : ((u.noDate || []).length ? '包含没有日期的记录。' : '');
  $('usageFoot').innerHTML =
    `数据来自本机 Claude Code 用量记录。` +
    `只统计 Claude Code 的用量；经本代理的其他客户端（如 Codex）不在其中。` +
    (noDateHint ? `<br><span class="range-note">${noDateHint}</span>` : '') +
    (u.unpriced ? `<br><b>还有 ${fmtN(u.unpriced)} 个 token 没有单价</b>，未计入花费——在下方「单价」里填上即可。` : '') +
    (cacheWarning
      ? `<br><div class="usage-cache-warning"><b>${warningText}。</b>` +
        `受影响模型：<code>${warningModels.map(esc).join('</code>、<code>')}</code>。</div>`
      : '');
}

// renderMeter 画「经过 ccproxy 的流量」。这一块才是代理该报告的东西：
// 它只包含真正过了代理的请求，而且按上游分得开——会话记录只知道模型名。
function renderMeter() {
  const m = state.meter || { rows: [], cost: 0 };
  const rows = m.rows || [];
  if (!rows.length) {
    $('meterTotal').innerHTML =
      '<div class="usage-empty">尚无流量经过代理。启动代理并发起一次请求后即会出现统计。</div>';
    $('meterTable').innerHTML = '';
    $('meterSince').textContent = '';
    return;
  }
  const t = rows.reduce((a, r) => ({
    reqs: a.reqs + r.reqs, in: a.in + r.in, cacheW: a.cacheW + r.cacheW,
    cacheR: a.cacheR + r.cacheR, out: a.out + r.out,
  }), { reqs: 0, in: 0, cacheW: 0, cacheR: 0, out: 0 });

  const meterRange = state.usageFilters.meter.applied;
  $('meterSince').textContent = !meterRange.from && !meterRange.to && m.since
    ? '· 自 ' + String(m.since).slice(0, 16).replace('T', ' ')
    : '';
  $('meterTotal').innerHTML = `
    <div class="cell"><span class="big">${esc(fmtM(m.cost))}</span><span class="unit">估算花费</span></div>
    <div class="cell"><span class="n">${fmtN(t.reqs)}</span><span class="unit">请求</span></div>
    ${totCell('输入', t.in)}${totCell('缓存写', t.cacheW)}
    ${totCell('缓存读', t.cacheR)}${totCell('输出', t.out)}`;

  $('meterTable').innerHTML = `<tbody>
    <tr class="head"><th>上游</th><th>模型</th><th>请求</th><th>输入</th><th>缓存写</th><th>缓存读</th><th>输出</th><th>花费</th></tr>
    ${rows.map((r) => `<tr>
      <td>${esc(r.name || r.provider)}</td><td class="m">${esc(r.model)}</td>
      <td class="n">${fmtN(r.reqs)}</td>
      ${tokCell(r.in)}${tokCell(r.cacheW)}${tokCell(r.cacheR)}${tokCell(r.out)}
      <td class="n">${r.priced ? esc(fmtM(r.cost)) : '—'}</td></tr>`).join('')}
    <tr class="sum"><td colspan="2">合计</td><td class="n">${fmtN(t.reqs)}</td>
      ${tokCell(t.in)}${tokCell(t.cacheW)}${tokCell(t.cacheR)}${tokCell(t.out)}
      <td class="n">${esc(fmtM(m.cost))}</td></tr></tbody>`;
}

// renderPrices 画单价表。模型取两张用量表的并集——代理实测里可能有
// 会话记录没见过的（别的客户端打的），反过来也一样。
function renderPrices() {
  const seen = new Set();
  const list = [];
  const push = (m) => { if (m && !seen.has(m)) { seen.add(m); list.push(m); } };
  (state.usage?.models || []).forEach((m) => push(m.model));
  (state.meter?.rows || []).forEach((r) => push(r.model));
  // 筛选后暂时没有结果时，也保留已配置的单价模型。
  Object.keys(state.prices || {}).forEach(push);
  const custom = state.prices || {};

  // 空态：这张表的行来自上面两张表的并集，两张都空时它没有任何可填的行。
  // 只画一行表头会让人以为是坏了——那正是这里要避免的。
  $('priceEmpty').hidden = list.length > 0;
  if (!list.length) {
    $('priceEmpty').textContent =
      '单价按实际使用过的模型逐项列出。上面两张表尚无任何模型，因此此处为空；'
      + '产生用量后会自动列出，常见模型已内置预设单价。';
    $('priceTable').innerHTML = '';
    return;
  }

  const cols = [['in', '输入'], ['cacheW', '缓存写'], ['cacheR', '缓存读'], ['out', '输出']];
  $('priceTable').innerHTML = `<tbody>
    <tr class="head"><th>模型</th>${cols.map(([, l]) => `<th>${l}</th>`).join('')}<th>单位</th></tr>
    ${list.map((model) => {
      const p = state.effPrices[model] || {};
      const isCustom = !!custom[model];
      const cur = p.cur === 'CNY' ? 'CNY' : 'USD';
      return `<tr>
        <td class="m">${esc(model)}</td>
        ${cols.map(([k]) => `<td><input type="text" inputmode="decimal"
          class="${isCustom ? 'edited' : ''}"
          data-price="${esc(model)}|${k}" value="${esc(pnum(p[k]))}" placeholder="—"></td>`).join('')}
        <td><select data-price="${esc(model)}|cur">
          <option value="USD"${cur === 'USD' ? ' selected' : ''}>$ /百万</option>
          <option value="CNY"${cur === 'CNY' ? ' selected' : ''}>¥ /百万</option>
        </select></td>
      </tr>`;
    }).join('')}
  </tbody>`;

  document.querySelectorAll('[data-price]').forEach((el) => {
    const apply = () => {
      const [model, key] = el.dataset.price.split('|');
      // 手填要连币种一起存下来：只存数字的话，重开面板时会退回预设的币种，
      // 一个按人民币填的价就被当成美元算，差 6.75 倍。
      const p = { ...(state.prices[model] || state.effPrices[model] || {}) };
      if (key === 'cur') {
        p.cur = el.value === 'CNY' ? 'CNY' : '';
      } else {
        const v = parseFloat(el.value);
        if (isNaN(v) || v < 0) delete p[key]; else p[key] = v;
      }
      const hasNum = ['in', 'cacheW', 'cacheR', 'out'].some((k) => Number.isFinite(p[k]));
      if (hasNum) state.prices[model] = p; else delete state.prices[model];
      state.effPrices[model] = p;
      markDirty();
    };
    el.oninput = apply;
    el.onchange = apply;
  });
}

/* ---------- 状态 / 接入 / diff ---------- */

function setStatus(running, status) {
  const dot = $('statusDot');
  if (running && status) {
    dot.className = 'dot ok';
    const hits = status.hits || {};
    const total = Object.values(hits).reduce((a, b) => a + b, 0);
    $('statusText').textContent = `运行中 · ${status.port}` + (total ? ` · ${total} 次` : '');
  } else {
    dot.className = 'dot err';
    $('statusText').textContent = '未运行';
  }
}

function renderConnect() {
  const url = `http://127.0.0.1:${state.port}`;
  $('hookUrl').value = url;
  const k = document.querySelector('#segCode button.on').dataset.k;
  $('codeBox').textContent =
    k === 'sh' ? `export ANTHROPIC_BASE_URL=${url}\nexport ANTHROPIC_AUTH_TOKEN=ccproxy-managed`
  : k === 'ps' ? `$env:ANTHROPIC_BASE_URL   = "${url}"\n$env:ANTHROPIC_AUTH_TOKEN = "ccproxy-managed"`
  : k === 'codex' ? `# ~/.codex/config.toml
model = "gpt-5.6-luna"
model_provider = "ccproxy"

[model_providers.ccproxy]
name = "ccproxy"
base_url = "${url}"
env_key = "CCPROXY_KEY"
wire_api = "responses"

# 再设一个同名环境变量即可，值任意：
#   export CCPROXY_KEY=ccproxy-managed`
  : JSON.stringify({ env: { ANTHROPIC_BASE_URL: url, ANTHROPIC_AUTH_TOKEN: 'ccproxy-managed' } }, null, 2);
}

function renderDiff() {
  const out = ['<div class="keep">"env": {</div>'];
  const add = (k, v) => out.push(`<div class="add">  "${esc(k)}": "${esc(v)}",</div>`);
  add('ANTHROPIC_BASE_URL', `http://127.0.0.1:${state.port}`);
  add('ANTHROPIC_AUTH_TOKEN', 'ccproxy-managed');
  if (state.retryWatchdog) add('CLAUDE_CODE_RETRY_WATCHDOG', '1');
  state.slotDefs.forEach((d) => {
    const s = state.slots[d.key];
    if (s && s.model) add(d.env, s.model + (s.oneM ? '[1M]' : ''));
  });
  out.push('<div class="keep">  … 其余键原样保留</div><div class="keep">}</div>');
  $('diffBox').innerHTML = out.join('');
}

function renderAll() {
  renderProviders();
  renderNav();
  renderSlots();
  renderConnect();
  renderDiff();
  selectPane(state.active);
}

/* ---------- 加载 / 保存 ---------- */

async function load() {
  const s = await api('/api/state');
  if (!s.ok) {
    const message = s.error || '读取配置失败';
    toast(message, true);
    const scroll = document.querySelector('.scroll');
    if (scroll) {
      scroll.innerHTML = `<section class="page active"><div class="page-head"><h1>无法读取配置</h1>` +
        `<div class="sub">${esc(message)}</div></div>` +
        `<div class="usage-empty">请修正配置后重新打开 ccproxy。现有配置不会被修改。</div></section>`;
    }
    $('save').disabled = true;
    return;
  }

  const c = s.config || {};
  state.providers = (c.providers || []).map((p) => ({ ...p, models: p.models || [] }));
  // 连通性结论持久化在 modelProtocols 里，而界面状态是内存态。不反推的话
  // 重开面板后每个模型都显示「未测」，可测试对话框刚承诺过结果会被保存，
  // 保存时也确实在复用它。只反推成功的：失败没有记录，也不该跨会话保留——
  // 上游可能早就恢复了，把陈旧的失败摆在那儿比不显示更糟。
  state.providers.forEach((p) => {
    const known = Object.keys(p.modelProtocols || {});
    if (!known.length) return;
    const st = {};
    known.forEach((m) => { st[m] = true; });
    state.modelStatus[p.id] = st;
  });
  state.slots = c.slots || {};
  state.slotDefs = s.slotDefs || [];
  state.port = c.port || 15722;
  state.retryWatchdog = c.retryWatchdog !== false;
  state.firstByteSec = c.firstByteSec || 95;
  state.stallSec = c.stallSec || 60;
  state.settingsPath = c.settingsPath || '';
  state.prices = c.prices || {};
  state.autostart = !!s.autostart;

  const valid = [...state.providers.map((p) => p.id), 'alloc', 'adv', 'connect', 'restore'];
  if (!state.active || !valid.includes(state.active)) {
    state.active = state.providers.length ? state.providers[0].id : 'alloc';
  }

  if (s.version) $('brandVer').textContent = s.version;
  $('port').value = state.port;
  $('firstByte').value = state.firstByteSec;
  $('stall').value = state.stallSec;
  $('settingsPathInput').value = state.settingsPath;
  $('watchdog').classList.toggle('on', state.retryWatchdog);
  $('autostart').classList.toggle('on', state.autostart);

  const st = s.settings || {};
  $('settingsPath').textContent = st.path || '—';
  $('elevWarn').hidden = !s.elevated;

  renderAll();
  setStatus(s.running, s.status);
  state.dirty = false;
  $('dirty').classList.remove('show');

  if (st.exists && !st.valid) {
    toast('settings.json 不是合法 JSON，已停止操作', true);
    $('save').disabled = true;
  }
}

async function save() {
  const btn = $('save');
  btn.disabled = true;
  // 一句话覆盖整个过程：后端先逐个实调槽位模型，再启动代理并等它真正
  // 开始服务，全部完成才返回。这条提示到那时才消失。
  busy('正在校验模型并启动代理…');
  try {
    const r = await api('/api/save', {
      providers: state.providers,
      slots: state.slots,
      retryWatchdog: state.retryWatchdog,
      firstByteSec: parseInt($('firstByte').value, 10) || 95,
      stallSec: parseInt($('stall').value, 10) || 60,
      autostart: state.autostart,
      settingsPath: $('settingsPathInput').value.trim(),
      prices: state.prices,
      port: parseInt($('port').value, 10) || 15722,
    });
    // 等待到此结束——后面的对话框不该顶着一条「正在…」。
    busyDone();
    if (!r.ok) {
      if (r.saved) {
        if (Number.isInteger(r.port)) {
          state.port = r.port;
          $('port').value = r.port;
        }
        state.dirty = false;
        $('dirty').classList.remove('show');
        renderConnect(); renderDiff();
        await dialog({
          title: '配置已保存，但代理未启动',
          message: r.error || '配置已保存，但代理未能启动。',
          okText: '知道了', alert: true,
        });
      } else {
        await dialog({ title: '配置未写入', message: r.error || '保存失败', okText: '知道了', alert: true });
      }
      return;
    }
    state.port = r.port;
    $('port').value = r.port;
    state.dirty = false;
    $('dirty').classList.remove('show');
    renderConnect(); renderDiff();

    const assigned = state.slotDefs.map((d) => {
      const sl = state.slots[d.key];
      if (!sl || !sl.model) return null;
      const p = state.providers.find((x) => x.id === sl.provider);
      return `${d.label} → ${p ? displayName(p, state.providers.indexOf(p)) : '?'} · ${sl.model}${sl.oneM ? '[1M]' : ''}`;
    }).filter(Boolean);

    const lines = [`代理已在 127.0.0.1:${r.port} 运行。`];
    if (assigned.length) lines.push('', ...assigned);
    if (r.backupPath) lines.push('', '原配置已备份到 ' + r.backupPath.split(/[\\/]/).pop());
    lines.push('', '重启 Claude Code 后配置生效。');

    await dialog({
      title: '已应用',
      message: lines.join('\n'), okText: '知道了', alert: true,
    });

    // 开机自启写的是当前用户的注册表 Run 键，不需要授权。极少数被组策略
    // 或安全软件拦下时如实说明，并把开关恢复成真实状态——不静默失败。
    if (r.autostartErr) await reportAutostartFailure(r.autostartErr);

    setTimeout(refreshStatus, 800);
    if (state.usage) loadUsage();
  } catch (e) {
    toast(String(e), true);
  } finally {
    busyDone();
    btn.disabled = false;
  }
}

// reportAutostartFailure 如实报告自启没注册上。
//
// 这里没有第二条路可选：提权注册计划任务那条兜底已随绿色版一起删除——
// 计划任务留在任务计划库里，是唯一真正难清理的一处痕迹。
async function reportAutostartFailure(errText) {
  state.autostart = false;
  $('autostart').classList.remove('on');
  await dialog({
    title: '开机自启未能注册',
    message: errText +
      '\n\n通常是组策略或安全软件拦截了当前用户的注册表启动项。' +
      '\n\n其余配置已正常保存，开机后需手动运行一次 ccproxy.exe。',
    okText: '知道了', alert: true,
  });
}

async function uninstall() {
  const ok = await dialog({
    title: '卸载并还原？',
    message: '把这台机器恢复到未安装的状态：\n' +
      '· 还原 settings.json 中被 ccproxy 修改的字段，其余内容不变\n' +
      '· 停止后台代理，取消开机自启\n' +
      '· 删除 ccproxy 创建的全部文件，含已保存的上游地址与凭证\n\n' +
      '配置不保留，重新启用需要重新填写。',
    okText: '还原', danger: true,
  });
  if (!ok) return;

  busy('正在还原并清理…');
  let r;
  try {
    r = await api('/api/uninstall', {});
  } catch (e) {
    busyDone(); return toast(String(e), true);
  }
  busyDone();
  if (!r.ok) { toast(r.error || '还原失败', true); return; }

  // 半完成状态必须说清楚：配置还原了，但代理还在跑。
  // 只 toast「已还原」而左下角仍显示运行中，用户完全无从判断发生了什么。
  // 此时文件也一个没删——代理正开着日志、随时会把 status.json 写回来。
  if (r.stopErr) {
    await dialog({
      title: '已还原，但代理没能停止',
      message: 'settings.json 已还原、开机自启已取消。文件尚未清理。\n\n' + r.stopErr +
        '\n\n常见原因是后台代理由管理员权限的进程启动，而控制面板为普通权限，' +
        '普通进程无权结束它。在任务管理器中结束 ccproxy.exe 后，再执行一次还原即可。',
      okText: '知道了', alert: true,
    });
    refreshStatus();
    return;
  }

  const left = r.leftover || [];
  const lines = ['settings.json 已还原，开机自启已取消，后台代理已停止。'];
  if (left.length) {
    lines.push('', '配置、日志、备份均已删除，数据目录里还剩：', '',
      ...left.map((p) => p.split(/[\\/]/).pop()),
      '', '运行中的程序无法删除自身：本次控制面板是从数据目录内启动的。'
      + '关闭本窗口后手动删除即可。');
  } else {
    // 不说「零残留」：界面自身的 WebView2 缓存在 %TEMP% 下，要等这个窗口
    // 连同它派生的浏览器进程一起退干净才删得掉，而那是尽力而为的。
    // 这个项目栽过一次「说是删了、其实还在」，不能在收尾这句上再犯。
    lines.push('', 'ccproxy 创建的文件已全部删除：配置、日志、用量记录、'
      + '后台服务映像，以及数据目录本身。'
      + '\n控制面板自身的浏览器缓存位于系统临时目录，关闭本窗口时清除；'
      + '若浏览器组件退出较慢未能清除，将由系统的临时文件回收机制处理。');
  }
  lines.push('', '启动器 ccproxy.exe 不在清理范围内：其位置由用户决定，'
    + '本程序不记录该路径，不再需要时自行删除。');
  if (r.purgeErr) lines.push('', '有文件未能删除：' + r.purgeErr);

  await dialog({ title: '已还原', message: lines.join('\n'), okText: '知道了', alert: true });

  // 打开残留文件所在目录，然后关闭面板——exe 一旦不再被占用，用户当场就能删。
  if (left.length) await api('/api/action', { action: 'openlog' });
  await api('/api/quit', {});
}

async function refreshStatus() {
  try {
    const s = await api('/api/state');
    if (s.ok) { setStatus(s.running, s.status); $('elevWarn').hidden = !s.elevated; }
  } catch (_) { /* 轮询失败不打扰用户 */ }
}

/* ---------- 窗口交互 ---------- */
// winctl 由 Go 侧通过 webview Bind 注入；非 Windows 调试环境下不存在。
const winctl = (a) => { if (typeof window.winctl === 'function') window.winctl(a); };

document.querySelectorAll('[data-win]').forEach((b) => { b.onclick = () => winctl(b.dataset.win); });

// 整条标题栏可拖，只排除窗口按钮本身。
// 不用独立的覆盖层：标题栏层级更高会把它盖死，拖拽将完全收不到事件。
//
// 双击最大化必须在 mousedown 里自己判，不能挂 dblclick 事件：第一次按下
// 就已经把控制权交给了系统的移动模态循环（ReleaseCapture +
// WM_NCLBUTTONDOWN），之后的鼠标消息全被那个循环吃掉，dblclick 永远到不了
// 页面——实测现象正是「拖拽正常、双击毫无反应」。
//
// 判据取「时间 + 位移」，与 Windows 自己的双击判据一致。只看时间的话，
// 拖完窗口马上再按一下就会被误判成最大化；加上位移就不会。
// 500ms 是 Windows 的默认双击间隔。
const titlebar = document.querySelector('.titlebar');
let lastTitleDown = { t: 0, x: 0, y: 0 };
titlebar.addEventListener('mousedown', (e) => {
  if (e.button !== 0 || e.target.closest('.winctl')) return;
  e.preventDefault();
  const now = Date.now();
  const isDouble = now - lastTitleDown.t < 500 &&
    Math.abs(e.screenX - lastTitleDown.x) < 5 &&
    Math.abs(e.screenY - lastTitleDown.y) < 5;
  // 双击已经消费掉就清空计时，否则紧接着的第三次按下又会被判成双击
  lastTitleDown = isDouble ? { t: 0, x: 0, y: 0 } : { t: now, x: e.screenX, y: e.screenY };
  winctl(isDouble ? 'maximize' : 'drag');
});
document.querySelectorAll('[data-rz]').forEach((el) => {
  el.addEventListener('mousedown', (e) => { if (e.button === 0) { e.preventDefault(); winctl('resize:' + el.dataset.rz); } });
});

/* ---------- 绑定 ---------- */

$('addProvider').onclick = () => {
  const p = { id: nextProviderId(), name: '', baseUrl: '', token: '', models: [] };
  state.providers.push(p);
  state.active = p.id;
  markDirty(); renderAll();
};

$('save').onclick = save;
$('uninstall').onclick = uninstall;

$('watchdog').onclick = () => {
  state.retryWatchdog = !state.retryWatchdog;
  $('watchdog').classList.toggle('on', state.retryWatchdog);
  markDirty(); renderDiff();
};
$('autostart').onclick = () => {
  state.autostart = !state.autostart;
  $('autostart').classList.toggle('on', state.autostart);
  markDirty();
};
$('resetTimings').onclick = () => {
  $('firstByte').value = 95; $('stall').value = 60; markDirty(); toast('已恢复默认值');
};
document.querySelectorAll('[data-step]').forEach((b) => {
  b.onclick = () => {
    const [id, delta] = b.dataset.step.split(':');
    const el = $(id);
    el.value = Math.max(5, (parseInt(el.value, 10) || 0) + parseInt(delta, 10));
    markDirty();
  };
});
['port', 'firstByte', 'stall', 'settingsPathInput'].forEach((id) => { $(id).oninput = markDirty; });

document.querySelectorAll('#segCode button').forEach((b) => {
  b.onclick = () => {
    document.querySelectorAll('#segCode button').forEach((x) => x.classList.remove('on'));
    b.classList.add('on');
    renderConnect();
  };
});

// 复制：WebView2 里 navigator.clipboard 在非安全上下文可能不可用，保留 execCommand 兜底
async function copyText(text) {
  try { await navigator.clipboard.writeText(text); }
  catch (_) {
    const ta = document.createElement('textarea');
    ta.value = text; document.body.appendChild(ta); ta.select();
    document.execCommand('copy'); ta.remove();
  }
  toast('已复制');
}
function wireCopyButtons(root) {
  (root || document).querySelectorAll('[data-copy]').forEach((b) => {
    b.onclick = () => {
      const el = $(b.dataset.copy);
      copyText(el.value !== undefined ? el.value : el.textContent.trim());
    };
  });
}
wireCopyButtons();

$('statusBtn').onclick = (e) => {
  e.stopPropagation();
  const open = $('statusPop').classList.toggle('show');
  $('statusBtn').classList.toggle('open', open);
};
document.querySelectorAll('[data-act]').forEach((b) => {
  b.onclick = async (e) => {
    e.stopPropagation();
    $('statusPop').classList.remove('show');
    $('statusBtn').classList.remove('open');
    const act = b.dataset.act;
    if (act === 'copyaddr') return copyText(`http://127.0.0.1:${state.port}`);
    // 停止与重启都要等端口真正腾出、代理真正起来，可能几秒。
    if (act === 'stop' || act === 'restart') {
      busy(act === 'stop' ? '正在停止代理…' : '正在重启代理…');
    }
    const r = await api('/api/action', { action: act });
    busyDone();
    if (!r.ok) {
      // 结束进程失败需要解释，一句 toast 说不清也放不下
      if (act === 'stop' || act === 'restart') {
        await dialog({
          title: act === 'stop' ? '未能停止代理' : '未能重启代理',
          message: (r.error || '操作失败') +
            '\n\n常见原因是后台代理由管理员权限的进程启动，而控制面板为普通权限，' +
            '普通进程无权结束它。在任务管理器中结束 ccproxy.exe 即可。',
          okText: '知道了', alert: true,
        });
        refreshStatus();
        return;
      }
      return toast(r.error || '操作失败', true);
    }
    if (act === 'copydiag') return copyText(r.text || '');
    toast(r.message || '完成');
    refreshStatus();
  };
});

// 重置只清代理自己数的那一份。会话记录那一块是 Claude Code 写的文件，
// 不归 ccproxy 管，也绝不会去动——确认框里要把这条说清楚，
// 否则用户会以为点下去连历史账单一起没了。
$('refreshUsage').onclick = () => {
  loadUsage();
};

$('resetUsage').onclick = async () => {
  const m = state.meter || { rows: [], cost: 0 };
  const n = (m.rows || []).reduce((a, r) => a + r.reqs, 0);
  const ok = await dialog({
    title: '重置用量计数？',
    message: `将清空「经过 ccproxy 的流量」的累计：${n} 次请求、` +
      `估算 ${fmtM(m.cost)}。重置后起始时间从当前时刻重新计算。\n\n` +
      '下方「Claude Code 总用量」不受影响：其数据来自 Claude Code 的会话记录，' +
      '本程序不会修改这些文件。\n\n清空后无法恢复。',
    okText: '重置', danger: true,
  });
  if (!ok) return;
  busy('正在重置…');
  try {
    const r = await api('/api/action', { action: 'resetusage' });
    if (!r.ok) {
      toast(r.error || '重置失败', true);
      return;
    }
    toast(r.message || '用量已重置');
    loadUsage();
  } catch (e) {
    toast(`重置失败：${e && e.message ? e.message : '网络错误'}`, true);
  } finally {
    busyDone();
  }
};

$('pickSettings').onclick = () => {
  if (typeof window.pickfile !== 'function') return toast('当前平台不支持文件选择框，请手工填写路径', true);
  Promise.resolve(window.pickfile($('settingsPathInput').value.trim())).then((p) => {
    if (p) { $('settingsPathInput').value = p; markDirty(); }
  });
};

function localDate(offset = 0) {
  const d = new Date();
  d.setHours(12, 0, 0, 0);
  d.setDate(d.getDate() + offset);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}
function syncRangeBar(bar, kind) {
  const f = state.usageFilters[kind];
  const custom = bar.querySelector('[data-custom]');
  bar.querySelectorAll('[data-preset]').forEach((b) => {
    const on = b.dataset.preset === (f.applied.preset || 'all');
    b.classList.toggle('active', on); b.setAttribute('aria-pressed', String(on));
  });
  bar.querySelector('[data-from]').value = f.draft.from;
  bar.querySelector('[data-to]').value = f.draft.to;
  custom.hidden = f.applied.preset !== 'custom';
  const summary = bar.querySelector('[data-summary]');
  const { from, to } = f.applied;
  summary.textContent = !from && !to ? '全部日期' : (from === to ? from : `${from} 至 ${to}`);
}
function rangeRequest(kind, applied) {
  const f = state.usageFilters[kind];
  f.applied = applied.preset === 'all'
    ? { from: '', to: '', preset: 'all' }
    : { ...applied };
  f.draft = { from: applied.from, to: applied.to };
  const bar = document.querySelector(`[data-range="${kind}"]`);
  if (bar) {
    bar.querySelector('[data-status]').textContent = '';
    syncRangeBar(bar, kind);
  }
  if (kind === 'meter') {
    $('meterSince').textContent = f.applied.from || f.applied.to || !state.meter?.since
      ? ''
      : '· 自 ' + String(state.meter.since).slice(0, 16).replace('T', ' ');
  }
  loadUsage();
}
document.querySelectorAll('.range-bar').forEach((bar) => {
  const kind = bar.dataset.range;
  const f = state.usageFilters[kind];
  f.applied.preset = 'all';
  syncRangeBar(bar, kind);
  bar.querySelectorAll('[data-preset]').forEach((b) => b.onclick = () => {
    if (b.dataset.preset === 'custom') {
      bar.querySelector('[data-status]').textContent = '';
      f.applied = { ...f.applied, preset: 'custom' };
      syncRangeBar(bar, kind);
      return;
    }
    const preset = b.dataset.preset;
    if (preset === 'all') {
      rangeRequest(kind, { from: '', to: '', preset: 'all' });
      return;
    }
    const to = preset === 'yesterday' ? localDate(-1) : localDate();
    const from = preset === 'today' || preset === 'yesterday'
      ? to
      : localDate(-(Number(preset) - 1));
    rangeRequest(kind, { from, to, preset });
  });
  bar.querySelectorAll('input').forEach((input) => input.oninput = () => {
    f.draft[input.dataset.from !== undefined ? 'from' : 'to'] = input.value;
    bar.querySelector('[data-status]').textContent = '';
  });
  bar.querySelector('[data-apply]').onclick = () => {
    const { from, to } = f.draft;
    const status = bar.querySelector('[data-status]');
    if (!from || !to) { status.textContent = '请选择开始和结束日期'; return; }
    if (from > to) { status.textContent = '开始日期不能晚于结束日期'; return; }
    rangeRequest(kind, { from, to, preset: 'custom' });
  };
});
load();
setInterval(refreshStatus, 5000);
