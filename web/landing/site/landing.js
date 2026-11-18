(() => {
  document.documentElement.classList.add('js');

  const toggle = document.querySelector('.nav-toggle');
  const menu = document.getElementById('nav-menu');
  if (toggle && menu) {
    const open = () => toggle.getAttribute('aria-expanded') === 'true';
    const set = (v) => toggle.setAttribute('aria-expanded', String(v));
    toggle.addEventListener('click', () => set(!open()));
    menu.addEventListener('click', (e) => {
      if (e.target instanceof Element && e.target.closest('a')) set(false);
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && open()) {
        set(false);
        toggle.focus();
      }
    });
    window.matchMedia('(max-width: 640px)').addEventListener('change', (e) => {
      if (!e.matches) set(false);
    });
  }

  const $ = (s, r = document) => r.querySelector(s);
  const $$ = (s, r = document) => Array.from(r.querySelectorAll(s));
  const clamp = (v, a, b) => Math.min(b, Math.max(a, v));
  const unit = (v) => clamp(v, 0, 1);
  const ease = (t) => 1 - Math.pow(1 - t, 3);
  const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  const wide = window.matchMedia('(min-width: 980px)');

  // Money in micro-USDC, with the rounding of internal/fee (the percentage rounds down).
  const MICRO = 1e6;
  const FEE_BPS = 50;
  const FIXED_FEE = 100000;
  // The confirmation window of Live changes, in minutes: the server's PRISMAPORT_CONFIRM_WINDOW
  // (default 10m, internal/service DefaultConfirmWindow). The page cannot read the server's
  // setting; a Go test (TestLandingShowsTheConfirmWindow) keeps this and the HTML in step with the default.
  const CONFIRM_WINDOW_MINUTES = 10;
  $$('[data-confirm-minutes]').forEach((el) => { el.textContent = String(CONFIRM_WINDOW_MINUTES); });
  $$('[data-confirm-clock]').forEach((el) => { el.textContent = CONFIRM_WINDOW_MINUTES + ':00'; });
  const MIN = MICRO;
  const MAX = 1e6 * MICRO;
  const feeOf = (g) => Math.floor(g * FEE_BPS / 10000) + FIXED_FEE;

  const fmt = (m, minDec = 2) => {
    const whole = Math.floor(m / MICRO).toLocaleString('en-US');
    let frac = String(m % MICRO).padStart(6, '0');
    while (frac.length > minDec && frac.endsWith('0')) frac = frac.slice(0, -1);
    return `${whole}.${frac}`;
  };

  // Accepts 1,000.50 and 1.000,50, or no grouping with either decimal mark.
  const parseAmount = (raw) => {
    const s = raw.trim().replace(/\s/g, '');
    let plain = null;
    if (/^\d{1,3}(,\d{3})+(\.\d{0,2})?$/.test(s)) plain = s.replace(/,/g, '');
    else if (/^\d{1,3}(\.\d{3})+,\d{0,2}$/.test(s)) plain = s.replace(/\./g, '').replace(',', '.');
    else if (/^\d+([.,]\d{0,2})?$/.test(s)) plain = s.replace(',', '.');
    if (plain === null) return null;
    const m = Math.round(parseFloat(plain) * 100) * 10000;
    return m >= MIN && m <= MAX ? m : null;
  };

  const state = { gross: 30 * MICRO, product: 'Studio plan' };
  let shown = state.gross;
  let binds = [];

  const values = (g) => {
    const fee = feeOf(g);
    const part = clamp(Math.round(g / 3 / 10000) * 10000, 10000, g - 10000);
    return {
      gross: fmt(g), exact: fmt(g, 6), json: fmt(g).replace(/,/g, ''), fee: fmt(fee), net: fmt(g - fee),
      partial: fmt(part), remaining: fmt(g - part), pct: `${(fee / g * 100).toFixed(2)}%`, product: state.product,
    };
  };

  const bind = (g) => {
    const v = values(g);
    binds.forEach((el) => {
      const t = v[el.dataset.v];
      if (el.textContent !== t) el.textContent = t;
    });
    // Sized by magnitude, not by text length: decimals change at every step of a drag.
    const net = g - feeOf(g);
    walletAmount.classList.toggle('long', net >= 1000 * MICRO && net < 100000 * MICRO);
    walletAmount.classList.toggle('xlong', net >= 100000 * MICRO);
    paintSplit(g);
  };

  // One amount for the whole page: the hero, the pricing form and the link builder share it.
  const range = $('#amt-range');
  const inputs = $$('[data-amount]');
  const presets = $$('[data-preset]');
  const toPos = (m) => clamp(Math.round(Math.log10(m / MICRO) / 5 * 1000), 0, 1000);
  const fromPos = (p) => {
    const raw = Math.pow(10, p / 1000 * 5);
    const step = raw < 10 ? 0.5 : raw < 100 ? 1 : raw < 1000 ? 5 : raw < 10000 ? 50 : 500;
    return Math.max(1, Math.round(raw / step) * step) * MICRO;
  };
  const heroAmount = $('#amt');
  const walletAmount = $('#wallet .big-amt');
  // The hero's field is narrow: large amounts step the type down so no digit is cut off.
  const fitHeroAmount = () => {
    heroAmount.classList.toggle('long', heroAmount.value.length > 7 && heroAmount.value.length <= 10);
    heroAmount.classList.toggle('xlong', heroAmount.value.length > 10);
  };
  const setInvalid = (i, bad) => {
    if (bad) i.setAttribute('aria-invalid', 'true');
    else i.removeAttribute('aria-invalid');
    document.getElementById(i.getAttribute('aria-errormessage')).hidden = !bad;
  };
  const syncControls = (src) => {
    const t = fmt(state.gross);
    inputs.forEach((i) => {
      if (i === src) return;
      i.value = t;
      setInvalid(i, false);
    });
    fitHeroAmount();
    if (src !== range) range.value = String(toPos(state.gross));
    range.style.setProperty('--p', `${Number(range.value) / 10}%`);
    presets.forEach((b) => b.setAttribute('aria-pressed', String(Number(b.dataset.preset) * MICRO === state.gross)));
  };
  let numRaf = 0;
  const setAmount = (m, src, tween) => {
    state.gross = m;
    syncControls(src);
    cancelAnimationFrame(numRaf);
    if (tween && !reduce) {
      const from = shown;
      const start = performance.now();
      const step = (now) => {
        const t = unit((now - start) / 380);
        shown = t < 1 ? Math.round((from + (m - from) * ease(t)) / 10000) * 10000 : m;
        bind(shown);
        if (t < 1) numRaf = requestAnimationFrame(step);
      };
      numRaf = requestAnimationFrame(step);
    } else {
      shown = m;
      bind(m);
    }
    if (reduce) paint();
  };
  inputs.forEach((i) => {
    i.addEventListener('input', () => {
      fitHeroAmount();
      const m = parseAmount(i.value);
      setInvalid(i, m === null);
      if (m !== null) setAmount(m, i, false);
    });
    i.addEventListener('blur', () => {
      i.value = fmt(state.gross);
      setInvalid(i, false);
      fitHeroAmount();
    });
  });
  range.addEventListener('input', () => setAmount(fromPos(Number(range.value)), range, false));
  presets.forEach((b) => b.addEventListener('click', () => setAmount(Number(b.dataset.preset) * MICRO, b, true)));
  $$('form').forEach((f) => f.addEventListener('submit', (e) => e.preventDefault()));

  // The hero: one payment in, your amount and the fee out.
  const stage = $('#stage');
  const cv = $('#beam');
  const ctx = cv.getContext('2d');
  const src = $('#src');
  const wallet = $('#wallet');
  const feeCard = $('#fee-card');
  const statusEl = $('#status');
  const pill = $('#st-pill');
  const stText = $('#st-text');
  const ticks = $$('#st-ticks i');
  const tally = $('#tally');
  const MONO_FONT = '500 12.5px "Geist Mono", ui-monospace, Menlo, monospace';
  const C = {};
  const B = { in: 6, thick: 7, thin: 2 };
  let W = 0;
  let H = 0;
  let dpr = 1;
  let G = null;

  const rgba = (hex, a) => {
    const n = parseInt(hex.replace('#', ''), 16);
    return `rgba(${n >> 16 & 255},${n >> 8 & 255},${n & 255},${a})`;
  };
  const dist = (a, b) => Math.hypot(b.x - a.x, b.y - a.y);
  const readColors = () => {
    const s = getComputedStyle(document.documentElement);
    ['surface', 'surface-raised', 'line-strong', 'ink', 'ink-muted', 'magenta'].forEach((k) => { C[k] = s.getPropertyValue(`--${k}`).trim(); });
  };
  const hitPoint = (a, b, c, d) => {
    const rx = b.x - a.x;
    const ry = b.y - a.y;
    const sx = d.x - c.x;
    const sy = d.y - c.y;
    const t = ((c.x - a.x) * sy - (c.y - a.y) * sx) / (rx * sy - ry * sx);
    return { x: a.x + rx * t, y: a.y + ry * t };
  };
  // Beams start and end at the panels on wide screens, at the canvas edges when stacked.
  const layout = () => {
    const r = cv.getBoundingClientRect();
    W = r.width;
    H = r.height;
    if (!W || !H) return;
    dpr = Math.min(window.devicePixelRatio || 1, 2);
    cv.width = Math.round(W * dpr);
    cv.height = Math.round(H * dpr);
    const box = (el) => {
      const q = el.getBoundingClientRect();
      return { l: q.left - r.left, r: q.right - r.left, t: q.top - r.top, b: q.bottom - r.top };
    };
    let ax = 0;
    let ayMin = 0;
    let ayMax = H;
    let x0 = 0;
    let x1 = W;
    let y0 = 14;
    let y1 = H - 14;
    let wEnd;
    let fEnd;
    if (wide.matches) {
      const s = box(src);
      const w = box(wallet);
      const f = box(feeCard);
      const st = box(statusEl);
      ax = x0 = s.r;
      x1 = w.l;
      y0 = 16;
      y1 = st.t - 20;
      ayMin = s.t + 28;
      ayMax = s.b - 28;
      wEnd = { x: w.l, y: (w.t + w.b) / 2 };
      fEnd = { x: f.l, y: (f.t + f.b) / 2 };
    }
    const gap = x1 - x0;
    const h = Math.min(y1 - y0, gap * (wide.matches ? 0.58 : 0.62));
    const w = h * 1.15;
    const cx = x0 + gap * (wide.matches ? 0.5 : 0.46);
    const cy = (y0 + y1) / 2;
    const top = cy - h / 2;
    // The same proportions as the mark: the split sits 57.5% down the right face.
    const P0 = { x: cx, y: top };
    const PR = { x: cx + w / 2, y: top + h };
    const PL = { x: cx - w / 2, y: top + h };
    const S = { x: cx + w / 2 * 0.575, y: top + h * 0.575 };
    const A = { x: ax, y: clamp(S.y + 0.09 * (S.x - ax), ayMin, ayMax) };
    if (!wide.matches) {
      wEnd = { x: W, y: Math.max(10, S.y - 0.34 * (W - S.x)) };
      fEnd = { x: W, y: Math.min(H - 10, S.y + 0.27 * (W - S.x)) };
    }
    G = { A, L: hitPoint(A, S, P0, PL), S, P0, PR, PL, wEnd, fEnd, sw: Math.max(2.5, h / 70), perim: dist(P0, PR) + dist(PR, PL) + dist(PL, P0) };
  };
  const seg = (p, q, color, w, f, dash) => {
    ctx.beginPath();
    ctx.moveTo(p.x, p.y);
    ctx.lineTo(p.x + (q.x - p.x) * f, p.y + (q.y - p.y) * f);
    ctx.strokeStyle = color;
    ctx.lineWidth = w;
    ctx.setLineDash(dash || []);
    ctx.lineCap = dash ? 'butt' : 'round';
    ctx.stroke();
    ctx.setLineDash([]);
  };
  const comet = (p, q, f, color, w, len) => {
    const d = dist(p, q);
    if (!d) return p;
    const ux = (q.x - p.x) / d;
    const uy = (q.y - p.y) / d;
    const head = f * d;
    const tail = Math.max(0, head - len);
    const hx = p.x + ux * head;
    const hy = p.y + uy * head;
    const tx = p.x + ux * tail;
    const ty = p.y + uy * tail;
    const g = ctx.createLinearGradient(tx, ty, hx, hy);
    g.addColorStop(0, rgba(color, 0));
    g.addColorStop(1, rgba(color, 1));
    ctx.strokeStyle = g;
    ctx.lineWidth = w;
    ctx.lineCap = 'round';
    ctx.beginPath();
    ctx.moveTo(tx, ty);
    ctx.lineTo(hx, hy);
    ctx.stroke();
    return { x: hx, y: hy };
  };
  const tag = (pt, txt, color, off) => {
    ctx.font = MONO_FONT;
    ctx.textBaseline = 'alphabetic';
    const m = ctx.measureText(txt).width;
    const x = clamp(pt.x - m / 2, 6, W - m - 6);
    const y = pt.y + off;
    ctx.fillStyle = C.surface;
    ctx.fillRect(x - 5, y - 13, m + 10, 18);
    ctx.fillStyle = color;
    ctx.fillText(txt, x, y);
  };

  const DUR = { in: 1150, confirm: 1300, out: 1050, paid: 1500 };
  const NEXT = { in: 'confirm', confirm: 'out', out: 'paid', paid: 'in' };
  let phase = 'in';
  let pt = 0;
  let intro = reduce ? 1 : 0;
  let lastTs = 0;
  let demoTotal = 0;
  let txId = '0x5f1e…a20c';
  let lastTick = -1;
  const hex4 = () => Math.random().toString(16).slice(2, 6).padEnd(4, '0');
  const setStatus = (kind, label, text) => {
    pill.className = `pill ${kind}`;
    pill.textContent = label;
    stText.textContent = text;
  };
  const fillTicks = (list, n) => list.forEach((t, i) => t.classList.toggle('on', i < n));
  const flash = (el) => {
    el.classList.remove('hit');
    void el.offsetWidth;
    el.classList.add('hit');
  };
  const enter = (p) => {
    phase = p;
    pt = 0;
    lastTick = -1;
    if (p === 'in') {
      txId = `0x${hex4()}…${hex4()}`;
      fillTicks(ticks, 0);
      setStatus('idle', 'Open', 'Waiting for your payment');
    }
    if (p === 'confirm') setStatus('idle', 'Confirming', '0 of 10 blocks on Base');
    if (p === 'out') {
      fillTicks(ticks, 10);
      setStatus('paid', 'Paid', 'Splitting in one transaction');
    }
    if (p === 'paid') {
      setStatus('paid', 'Paid', `Settled in ${txId}`);
      demoTotal += state.gross - feeOf(state.gross);
      tally.textContent = fmt(demoTotal);
      flash(wallet);
      flash(feeCard);
    }
  };
  function paint() {
    // WebKit can run this script before the stylesheet applies, when the colours read back empty.
    if (!C.ink) readColors();
    if (!G || !W || !C.ink) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, W, H);
    const g = state.gross;
    const fr = feeOf(g) / g;
    const v = values(shown);
    const inT = 3.5 + 2 * Math.log10(g / MICRO + 1);
    const thickT = inT * (1 - fr) + 1.5;
    const thinT = clamp(inT * fr * 6, 1.6, 3.2);
    const k = reduce ? 1 : 0.16;
    B.in += (inT - B.in) * k;
    B.thick += (thickT - B.thick) * k;
    B.thin += (thinT - B.thin) * k;
    const fIn = ease(unit(intro / 0.45));
    const fPrism = ease(unit((intro - 0.2) / 0.5));
    const fOut = ease(unit((intro - 0.55) / 0.45));
    const { A, L, S, P0, PR, PL, wEnd, fEnd, perim, sw } = G;
    const lenAL = dist(A, L);
    const lenLS = dist(L, S);
    const drawn = fIn * (lenAL + lenLS);
    seg(A, L, C['line-strong'], B.in, unit(drawn / lenAL));
    if (drawn > lenAL) seg(L, S, C['line-strong'], 1.5, unit((drawn - lenAL) / lenLS), [3, 6]);
    if (fOut > 0) {
      seg(S, wEnd, C.magenta, B.thick, fOut);
      seg(S, fEnd, C['ink-muted'], B.thin, fOut);
    }
    ctx.beginPath();
    ctx.moveTo(P0.x, P0.y);
    ctx.lineTo(PR.x, PR.y);
    ctx.lineTo(PL.x, PL.y);
    ctx.closePath();
    ctx.strokeStyle = C.ink;
    ctx.lineWidth = sw;
    ctx.lineJoin = 'round';
    if (fPrism < 1) {
      ctx.setLineDash([perim]);
      ctx.lineDashOffset = perim * (1 - fPrism);
    }
    ctx.stroke();
    ctx.setLineDash([]);
    ctx.lineDashOffset = 0;
    if (intro < 1 || reduce) return;
    const p = pt / DUR[phase];
    if (phase === 'in') {
      const f = ease(unit(p));
      const head = comet(A, S, f, C.ink, Math.max(2.5, B.in * 0.6), 130);
      if (f < 0.8) tag(head, `${v.gross} USDC`, C.ink, -14);
    }
    if (phase === 'confirm') {
      const q = (pt % 430) / 430;
      ctx.beginPath();
      ctx.arc(S.x, S.y, 6 + 13 * q, 0, Math.PI * 2);
      ctx.strokeStyle = rgba(C.ink, 1 - q);
      ctx.lineWidth = 1.5;
      ctx.stroke();
      ctx.beginPath();
      ctx.arc(S.x, S.y, 3.5, 0, Math.PI * 2);
      ctx.fillStyle = C.ink;
      ctx.fill();
    }
    if (phase === 'out') {
      const f = ease(unit(p));
      const a = comet(S, wEnd, f, C['surface-raised'], Math.max(2, B.thick * 0.5), 110);
      const b = comet(S, fEnd, f, C.ink, B.thin + 0.8, 80);
      if (f > 0.12 && f < 0.92) {
        tag(a, v.net, C.magenta, -14);
        tag(b, v.fee, C['ink-muted'], 22);
      }
    }
  }
  let visible = true;
  let running = false;
  let raf = 0;
  const frame = (ts) => {
    raf = 0;
    if (!running) return;
    const dt = Math.min(ts - (lastTs || ts), 50);
    lastTs = ts;
    if (intro < 1) {
      intro = Math.min(1, intro + dt / 1100);
      if (intro >= 1) enter('in');
    } else {
      pt += dt;
      if (phase === 'confirm') {
        const n = Math.min(10, Math.floor(pt / DUR.confirm * 10) + 1);
        if (n !== lastTick) {
          lastTick = n;
          fillTicks(ticks, n);
          stText.textContent = `${n} of 10 blocks on Base`;
        }
      }
      if (pt >= DUR[phase]) enter(NEXT[phase]);
    }
    paint();
    raf = requestAnimationFrame(frame);
  };
  const syncLoop = () => {
    running = visible && !document.hidden && !reduce;
    if (running && !raf) {
      lastTs = 0;
      raf = requestAnimationFrame(frame);
    }
  };

  // Payout splits: each wallet after the first rounds down and the first takes the rest, as in internal/payout.
  const WALLETS = [['Main wallet', '0x7a3F…e91c'], ['Partner', '0x41bD…07aa'], ['Studio', '0xC90e…2b13'], ['Contractor', '0x2Ef7…90ac'], ['Reserve', '0x8A11…d5f6']];
  const SVG_NS = 'http://www.w3.org/2000/svg';
  const bar = $('#bar');
  const srows = $('#srows');
  const addBtn = $('#s-add');
  const beamsG = $('#s-beams');
  const rxRows = $('#rx-rows');
  let shares = [{ w: WALLETS[0], bps: 7000 }, { w: WALLETS[1], bps: 3000 }];
  let segs = [];
  let handles = [];
  let rowEls = [];
  let beamEls = [];
  let rxEls = [];
  let feeRx = null;

  const svgEl = (name, attrs) => {
    const e = document.createElementNS(SVG_NS, name);
    Object.entries(attrs).forEach(([k, v]) => e.setAttribute(k, String(v)));
    return e;
  };
  const el = (tag, className, text) => {
    const e = document.createElement(tag);
    if (className) e.className = className;
    if (text) e.textContent = text;
    return e;
  };
  const payouts = (net) => {
    const out = shares.map(() => 0);
    let sum = 0;
    for (let i = 1; i < shares.length; i++) {
      out[i] = Math.floor(net * shares[i].bps / 10000);
      sum += out[i];
    }
    out[0] = net - sum;
    return out;
  };
  const moveBoundary = (i, to) => {
    let prev = 0;
    for (let j = 0; j < i; j++) prev += shares[j].bps;
    const next = prev + shares[i].bps + shares[i + 1].bps;
    const b = clamp(to, prev + 100, next - 100);
    shares[i].bps = b - prev;
    shares[i + 1].bps = next - b;
    paintSplit(shown);
  };
  const addHandle = (i) => {
    const h = el('button', 'handle');
    h.type = 'button';
    h.setAttribute('role', 'slider');
    h.setAttribute('aria-label', `Share between ${shares[i].w[0]} and ${shares[i + 1].w[0]}`);
    h.setAttribute('aria-valuemin', '1');
    h.setAttribute('aria-valuemax', '99');
    let drag = false;
    h.addEventListener('pointerdown', (e) => {
      drag = true;
      h.setPointerCapture(e.pointerId);
      h.focus();
      e.preventDefault();
    });
    h.addEventListener('pointermove', (e) => {
      if (!drag) return;
      const r = bar.getBoundingClientRect();
      moveBoundary(i, Math.round(unit((e.clientX - r.left) / r.width) * 100) * 100);
    });
    const stop = () => { drag = false; };
    h.addEventListener('pointerup', stop);
    h.addEventListener('pointercancel', stop);
    h.addEventListener('keydown', (e) => {
      if (e.key === 'Home' || e.key === 'End') {
        e.preventDefault();
        moveBoundary(i, e.key === 'Home' ? 0 : 10000);
        return;
      }
      const dir = { ArrowLeft: -1, ArrowDown: -1, ArrowRight: 1, ArrowUp: 1 }[e.key];
      if (!dir) return;
      e.preventDefault();
      let cum = 0;
      for (let j = 0; j <= i; j++) cum += shares[j].bps;
      moveBoundary(i, cum + dir * 100);
    });
    bar.appendChild(h);
    handles.push(h);
  };
  const buildSplit = () => {
    [bar, srows, beamsG, rxRows].forEach((n) => { n.textContent = ''; });
    segs = [];
    handles = [];
    rowEls = [];
    beamEls = [];
    rxEls = [];
    const n = shares.length;
    shares.forEach((s, i) => {
      segs.push(bar.appendChild(el('span', `seg shade${i}`)));

      const row = el('div', 'srow');
      const pct = el('span', 's-pct');
      const amt = el('span', 's-amt');
      row.append(el('i', `sw shade${i}`), el('span', 's-name', s.w[0]), pct, amt);
      if (i > 0 && n > 2) {
        const rm = el('button', 's-rm');
        rm.type = 'button';
        rm.setAttribute('aria-label', `Remove ${s.w[0]}`);
        const icon = svgEl('svg', { width: 16, height: 16, 'aria-hidden': 'true', focusable: 'false' });
        icon.appendChild(svgEl('use', { href: '#i-x' }));
        rm.appendChild(icon);
        rm.addEventListener('click', () => {
          shares[0].bps += s.bps;
          shares.splice(i, 1);
          buildSplit();
          addBtn.focus();
        });
        row.appendChild(rm);
      } else row.appendChild(el('span'));
      srows.appendChild(row);
      rowEls.push({ pct, amt });

      const y = 24 + i * (200 / (n - 1));
      const d = `M316 145 L560 ${y}`;
      const base = svgEl('path', { d, class: 'beam-out', 'stroke-linecap': 'round', fill: 'none' });
      const flow = svgEl('path', { d, class: 'flow', 'stroke-width': 2, fill: 'none' });
      const t = svgEl('text', { x: 556, y: y > 145 ? y + 16 : y - 10, 'text-anchor': 'end' });
      beamsG.append(base, flow, t);
      beamEls.push({ base, t });

      const tr = el('tr');
      tr.style.setProperty('--i', String(i));
      const td = el('td', '', s.w[0]);
      td.appendChild(el('small', '', s.w[1]));
      const ta = el('td', 'a');
      tr.append(td, ta);
      rxRows.appendChild(tr);
      rxEls.push(ta);
    });
    beamsG.appendChild(svgEl('path', { d: 'M316 145 L560 282', class: 'beam-fee', 'stroke-width': 2.5, 'stroke-linecap': 'round', fill: 'none' }));
    const feeT = svgEl('text', { x: 556, y: 298, 'text-anchor': 'end', class: 'fee-t' });
    feeT.textContent = 'Fee';
    beamsG.appendChild(feeT);
    const tr = el('tr');
    tr.style.setProperty('--i', String(n));
    const td = el('td', '', 'Prismaport fee');
    td.appendChild(el('small', '', '0x9C2e…44d1'));
    feeRx = el('td', 'a');
    tr.append(td, feeRx);
    rxRows.appendChild(tr);
    for (let i = 0; i < n - 1; i++) addHandle(i);
    addBtn.disabled = n >= WALLETS.length;
    paintSplit(shown);
  };
  function paintSplit(g) {
    if (!segs.length) return;
    const fee = feeOf(g);
    const p = payouts(g - fee);
    let cum = 0;
    shares.forEach((s, i) => {
      segs[i].style.flex = `${s.bps} 1 0`;
      rowEls[i].pct.textContent = `${s.bps / 100}%`;
      rowEls[i].amt.textContent = `${fmt(p[i])} USDC`;
      beamEls[i].base.setAttribute('stroke-width', String(Math.max(1.5, 16 * s.bps / 10000)));
      beamEls[i].t.textContent = `${s.bps / 100}%`;
      rxEls[i].textContent = `${fmt(p[i])} USDC`;
      cum += s.bps;
      if (handles[i]) {
        handles[i].style.left = `${cum / 100}%`;
        handles[i].setAttribute('aria-valuenow', String(Math.round(cum / 100)));
        handles[i].setAttribute('aria-valuetext', `${s.w[0]} ${s.bps / 100}%, ${shares[i + 1].w[0]} ${shares[i + 1].bps / 100}%`);
      }
    });
    feeRx.textContent = `${fmt(fee)} USDC`;
  }
  addBtn.addEventListener('click', () => {
    if (shares.length >= WALLETS.length) return;
    const big = shares.reduce((a, s) => (s.bps > a.bps ? s : a), shares[0]);
    if (big.bps < 200) return;
    const take = Math.floor(big.bps / 200) * 100;
    big.bps -= take;
    shares.push({ w: WALLETS.find((w) => !shares.some((s) => s.w === w)), bps: take });
    buildSplit();
  });

  // The story: one checkout through its five states.
  const PILLS = [['Open', 'idle'], ['Open', 'idle'], ['Partially paid', 'partial'], ['Confirming', 'idle'], ['Paid', 'paid']];
  const ck = $('#ck');
  const hook = $('#hook');
  const steps = $$('.step');
  const stepsBox = $('#steps');
  const rail = $('#rail');
  const fillConfirm = (root, n) => {
    $('[data-cklabel]', root).textContent = `${n} of 10`;
    $('[data-ckmeter]', root).style.width = `${n * 10}%`;
  };
  let curStep = 0;
  let tickTimer = 0;
  const setPill = (node, s) => {
    node.className = `pill ${s[1]}`;
    node.textContent = s[0];
  };
  const paintHook = (root, done) => {
    root.classList.toggle('on', done);
    setPill($('[data-hookpill]', root), done ? ['200 OK', 'paid'] : ['Waiting', 'idle']);
    $('[data-hookwait]', root).hidden = done;
    $('[data-hookdone]', root).hidden = !done;
  };
  const setStep = (i) => {
    if (i === curStep) return;
    curStep = i;
    ck.dataset.step = String(i);
    steps.forEach((s, j) => s.classList.toggle('on', j === i));
    $$('.ck-l', ck).forEach((l, j) => l.classList.toggle('on', j === i));
    setPill($('[data-pill]', ck), PILLS[i]);
    clearInterval(tickTimer);
    if (i === 3 && !reduce) {
      let n = 0;
      fillConfirm(ck, 0);
      tickTimer = setInterval(() => {
        n += 1;
        fillConfirm(ck, n);
        if (n >= 10) clearInterval(tickTimer);
      }, 170);
    } else fillConfirm(ck, i === 3 ? 10 : 0);
    paintHook(hook, i === 4);
  };
  // Phones get one static checkout per step instead of the sticky one.
  $$('[data-inline]').forEach((slot) => {
    const i = Number(slot.dataset.inline);
    const clone = ck.cloneNode(true);
    clone.removeAttribute('id');
    clone.dataset.step = String(i);
    $$('.ck-l', clone).forEach((l, j) => {
      if (j === i) l.classList.add('on');
      else l.remove();
    });
    setPill($('[data-pill]', clone), PILLS[i]);
    if (i === 3) fillConfirm(clone, 7);
    slot.appendChild(clone);
    if (i === 4) {
      const h = hook.cloneNode(true);
      h.removeAttribute('id');
      paintHook(h, true);
      slot.appendChild(h);
    }
  });
  const stepIO = new IntersectionObserver((entries) => {
    entries.forEach((e) => {
      if (e.isIntersecting && wide.matches) setStep(steps.indexOf(e.target));
    });
  }, { rootMargin: '-46% 0px -46% 0px' });
  steps.forEach((s) => stepIO.observe(s));

  let secs = 1799;
  const timers = $$('[data-timer]');
  setInterval(() => {
    secs = secs <= 1740 ? 1799 : secs - 1;
    const t = `${String(Math.floor(secs / 60)).padStart(2, '0')}:${String(secs % 60).padStart(2, '0')}`;
    timers.forEach((n) => { n.textContent = t; });
  }, 1000);

  // The API example, played once when it comes into view.
  const tabs = $$('.tab');
  const panels = tabs.map((t) => document.getElementById(t.getAttribute('aria-controls')));
  const termSt = $('#term-st');
  const TERM = [['POST', 'idle'], ['200 OK', 'paid'], ['session.paid', 'paid']];
  let termTimer = 0;
  let termAuto = true;
  const showTab = (i, focus) => {
    tabs.forEach((t, j) => {
      t.setAttribute('aria-selected', String(j === i));
      t.tabIndex = j === i ? 0 : -1;
      panels[j].hidden = j !== i;
    });
    setPill(termSt, TERM[i]);
    if (focus) tabs[i].focus();
  };
  const playTerm = (i) => {
    showTab(i);
    if (i < 2) termTimer = setTimeout(() => playTerm(i + 1), 2600);
  };
  tabs.forEach((t, i) => {
    t.addEventListener('click', () => {
      termAuto = false;
      clearTimeout(termTimer);
      showTab(i);
    });
    t.addEventListener('keydown', (e) => {
      const to = { ArrowRight: (i + 1) % tabs.length, ArrowLeft: (i - 1 + tabs.length) % tabs.length, Home: 0, End: tabs.length - 1 }[e.key];
      if (to === undefined) return;
      e.preventDefault();
      termAuto = false;
      clearTimeout(termTimer);
      showTab(to, true);
    });
  });
  $('#term-run').addEventListener('click', () => {
    termAuto = false;
    clearTimeout(termTimer);
    playTerm(0);
  });
  showTab(0);

  // The payment link builder.
  const lbName = $('#lb-name');
  const lbLim = $('#lb-lim');
  const lbExp = $('#lb-exp');
  const lbMeta = $('#lb-meta');
  lbName.addEventListener('input', () => {
    state.product = lbName.value.trim() || 'Your product';
    bind(shown);
  });
  const paintLink = () => {
    lbMeta.textContent = `${lbLim.checked ? 'Up to 100 payments' : 'No limit'}, ${lbExp.checked ? 'expires in 7 days' : 'no expiry'}`;
  };
  lbLim.addEventListener('change', paintLink);
  lbExp.addEventListener('change', paintLink);

  // Scroll progress, the sticky header's line and the story's rail.
  const progress = $('#progress');
  const siteHead = $('#site-head');
  let ticking = false;
  const onScroll = () => {
    if (ticking) return;
    ticking = true;
    requestAnimationFrame(() => {
      ticking = false;
      const max = document.documentElement.scrollHeight - window.innerHeight;
      progress.style.transform = `scaleX(${max > 0 ? window.scrollY / max : 0})`;
      siteHead.classList.toggle('stuck', window.scrollY > 8);
      const r = stepsBox.getBoundingClientRect();
      rail.style.setProperty('--p', String(unit((window.innerHeight * 0.5 - r.top) / r.height)));
    });
  };
  window.addEventListener('scroll', onScroll, { passive: true });
  window.addEventListener('resize', onScroll, { passive: true });

  if (!reduce) {
    const reveal = new IntersectionObserver((entries) => {
      entries.forEach((e) => {
        if (!e.isIntersecting) return;
        e.target.classList.add('in');
        reveal.unobserve(e.target);
      });
    });
    $$('.reveal').forEach((n) => reveal.observe(n));
    reveal.observe($('#receipt'));
    new IntersectionObserver(([e], o) => {
      if (!e.isIntersecting) return;
      o.disconnect();
      if (termAuto) playTerm(0);
    }, { threshold: 0.4 }).observe($('#term'));
    new IntersectionObserver(([e]) => {
      visible = e.isIntersecting;
      syncLoop();
    }).observe(stage);
  }
  document.addEventListener('visibilitychange', syncLoop);

  buildSplit();
  binds = $$('[data-v]');
  bind(state.gross);
  syncControls();
  readColors();
  layout();
  if (!reduce) {
    fillTicks(ticks, 0);
    setStatus('idle', 'Open', 'Waiting for your payment');
  }
  paint();
  syncLoop();
  const relayout = new ResizeObserver(() => {
    layout();
    paint();
  });
  [stage, src, wallet, feeCard].forEach((n) => relayout.observe(n));
  document.fonts.ready.then(() => {
    layout();
    paint();
  });
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
    readColors();
    paint();
  });
  onScroll();
})();
