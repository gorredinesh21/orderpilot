/* OrderPilot front-end: SSE streaming, live agent trace, cart, order tracking. */
(() => {
  const $ = (id) => document.getElementById(id);
  const chatLog = $("chat-log"), chatInput = $("chat-input"), sendBtn = $("send-btn"),
        statusLine = $("status-line"), modePill = $("mode-pill"),
        traceBody = $("tab-trace"), cartBody = $("cart-body"), ordersBody = $("orders-body"),
        cartCount = $("cart-count"), ordersCount = $("orders-count"), budgetLeft = $("budget-left"),
        hero = $("hero"), workspace = $("workspace");

  const SID_KEY = "orderpilot_sid";
  let sid = localStorage.getItem(SID_KEY) || ("s-" + Math.random().toString(36).slice(2, 10));
  localStorage.setItem(SID_KEY, sid);
  let busy = false;

  /* ---------- helpers ---------- */
  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }
  function addMsg(text, who) {
    const m = el("div", "msg " + who, text);
    chatLog.appendChild(m);
    chatLog.scrollTop = chatLog.scrollHeight;
    return m;
  }
  function addSystem(text) {
    const m = el("div", "msg agent system", text);
    chatLog.appendChild(m);
    chatLog.scrollTop = chatLog.scrollHeight;
    return m;
  }
  function toolChip(tool, result) {
    const isErr = result && result.error;
    const c = el("div", "tool-chip" + (isErr ? " err" : ""));
    c.appendChild(el("b", null, "🔧 " + tool + " "));
    let txt = "→ done";
    if (isErr) txt = "→ " + result.error;
    else if (result && result.restaurants) txt = "→ " + result.restaurants.length + " restaurants";
    else if (result && result.dishes) txt = "→ " + result.dishes.length + " dishes";
    else if (result && result.items) txt = "→ " + result.items.length + " items";
    else if (result && result.cart) txt = "→ cart ₹" + result.cart.total;
    else if (result && result.orders) txt = "→ " + result.orders.length + " order(s) placed";
    else if (result && result.state) txt = "→ " + result.state;
    else if (result && result.budget) txt = "→ budget ₹" + result.budget;
    c.appendChild(document.createTextNode(txt));
    chatLog.appendChild(c);
    chatLog.scrollTop = chatLog.scrollHeight;
  }
  function setStatus(t, isErr) {
    statusLine.textContent = t;
    statusLine.className = "status-line" + (isErr ? " err" : "");
  }
  function setMode(mode) {
    if (mode === "llm") { modePill.textContent = "LLM agent (Llama-3.1)"; modePill.className = "mode-pill llm"; }
    else if (mode === "fallback") { modePill.textContent = "deterministic planner"; modePill.className = "mode-pill fallback"; }
    else { modePill.textContent = "agent ready"; modePill.className = "mode-pill"; }
  }

  /* ---------- trace panel ---------- */
  function traceItem(tool, args, result) {
    const item = el("div", "trace-item");
    const head = el("div", "trace-head");
    head.appendChild(el("span", "fname", tool));
    const ok = !(result && result.error);
    head.appendChild(el("span", ok ? "ok" : "fail", ok ? "✓" : "✗"));
    item.appendChild(head);
    const json = el("div", "trace-json");
    json.textContent = "args: " + JSON.stringify(args ?? {}, null, 1) + "\n\nresult: " +
      JSON.stringify(result ?? {}, null, 1);
    item.appendChild(json);
    head.onclick = () => item.classList.toggle("open");
    traceBody.prepend(item);
  }
  const STATES = ["PLACED", "CONFIRMED", "PREPARING", "PICKED_UP", "OUT_FOR_DELIVERY", "DELIVERED"];
  const stateLabel = { PLACED: "Placed", CONFIRMED: "Confirmed", PREPARING: "Preparing", PICKED_UP: "Picked up", OUT_FOR_DELIVERY: "On the way", DELIVERED: "Delivered", CANCELLED: "Cancelled" };

  /* ---------- cart panel ---------- */
  function renderCart(snap) {
    const groups = snap.cart || [];
    const total = (snap.cart || []).reduce((a, g) => a + g.subtotal, 0);
    const count = (snap.cart || []).reduce((a, g) => a + g.lines.reduce((x, l) => x + l.qty, 0), 0);
    cartCount.textContent = count > 0 ? count : "";
    if ($("budget-input") && snap.budget) $("budget-input").value = snap.budget;
    budgetLeft.textContent = snap.budget != null ? `left ₹${Math.max(0, snap.budget - total)}` : "";
    if (!groups.length) {
      cartBody.innerHTML = '<div class="empty-note">Cart is empty. Ask the agent to add dishes.</div>';
      return;
    }
    cartBody.innerHTML = "";
    for (const g of groups) {
      const gc = el("div", "cart-group");
      const h = el("h4", null, `${g.restaurant_name} `);
      h.appendChild(el("span", null, `· ${g.area || ""} · ₹${g.subtotal}`));
      gc.appendChild(h);
      for (const l of g.lines) {
        const row = el("div", "cart-line");
        const left = el("span");
        const dot = el("span", l.veg ? "veg-dot" : "nonveg-dot");
        left.appendChild(dot);
        left.appendChild(document.createTextNode(`${l.qty} × ${l.name}`));
        row.appendChild(left);
        row.appendChild(el("span", null, `₹${l.unit_price * l.qty}`));
        gc.appendChild(row);
      }
      cartBody.appendChild(gc);
    }
    const tot = el("div", "cart-total");
    tot.appendChild(el("span", null, "Cart total"));
    tot.appendChild(el("span", null, `₹${total} / ₹${snap.budget}`));
    cartBody.appendChild(tot);
  }

  /* ---------- orders panel ---------- */
  function renderOrders(snap) {
    const orders = snap.orders || [];
    ordersCount.textContent = orders.length > 0 ? orders.length : "";
    if (!orders.length) {
      ordersBody.innerHTML = '<div class="empty-note">No orders yet. The agent checks out after you confirm.</div>';
      return;
    }
    ordersBody.innerHTML = "";
    for (const o of orders.slice().reverse()) {
      const card = el("div", "order-card");
      const oh = el("div", "oh");
      oh.appendChild(el("b", null, `${o.order_id} · ${o.restaurant}`));
      oh.appendChild(el("span", "state-pill st-" + o.state, stateLabel[o.state] || o.state));
      card.appendChild(oh);

      if (o.state !== "CANCELLED") {
        const st = el("div", "stepper");
        const cur = STATES.indexOf(o.state);
        STATES.forEach((s, i) => {
          const d = el("div", "st" + (i < cur ? " done" : i === cur ? " now" : ""), stateLabel[s]);
          st.appendChild(d);
        });
        card.appendChild(st);
      }
      const meta = el("div", "order-meta");
      meta.appendChild(el("span", null, `🛵 ${o.de_name}`));
      if (o.state !== "DELIVERED" && o.state !== "CANCELLED") {
        meta.appendChild(el("span", "eta-big", etaText(o.eta_seconds_remaining)));
      } else if (o.state === "DELIVERED") {
        meta.appendChild(el("span", null, "✓ delivered"));
      }
      meta.appendChild(el("span", null, `₹${o.subtotal}`));
      card.appendChild(meta);
      const lines = el("div", "order-lines");
      lines.textContent = (o.lines || []).map(l => `${l.qty}× ${l.name}`).join(", ");
      card.appendChild(lines);
      ordersBody.appendChild(card);
    }
  }
  function etaText(sec) {
    if (sec == null) return "";
    if (sec <= 0) return "any moment";
    const m = Math.floor(sec / 60), s = sec % 60;
    return m > 0 ? `ETA ${m}m ${s}s` : `ETA ${s}s`;
  }

  function renderSnapshot(snap) {
    renderCart(snap);
    renderOrders(snap);
  }

  /* ---------- SSE chat ---------- */
  async function send(text) {
    if (busy || !text.trim()) return;
    busy = true;
    sendBtn.disabled = true;
    hero.style.display = "none";
    workspace.style.marginTop = "6px";
    addMsg(text, "user");
    chatInput.value = "";
    setStatus("Agent is thinking…");
    try {
      const resp = await fetch("/api/chat", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ session_id: sid, message: text }),
      });
      if (!resp.ok) {
        const t = await resp.text();
        throw new Error(`server ${resp.status}: ${t.slice(0, 200)}`);
      }
      const reader = resp.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf("\n\n")) >= 0) {
          const frame = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          if (!frame.startsWith("data: ")) continue;
          handleEvent(JSON.parse(frame.slice(6)));
        }
      }
      setStatus("Ready. The agent runs 100% server-side in Go.");
    } catch (e) {
      setStatus("Error: " + e.message, true);
      addSystem("⚠ Connection problem — " + e.message);
    } finally {
      busy = false;
      sendBtn.disabled = false;
      chatInput.focus();
    }
  }

  function handleEvent(ev) {
    switch (ev.type) {
      case "mode":
        setMode(ev.mode);
        if (ev.text) addSystem("⚙ " + ev.text);
        break;
      case "say":
        addMsg(ev.text, "agent");
        break;
      case "tool_start":
        setStatus(`Running ${ev.tool}…`);
        break;
      case "tool_result":
        toolChip(ev.tool, ev.result);
        traceItem(ev.tool, ev.args, ev.result);
        if (ev.result && ev.result.cart) renderCart({ cart: ev.result.cart.groups, budget: ev.result.cart.budget });
        if (ev.result && ev.result.orders) pollOnce();
        break;
      case "error":
        addSystem("⚠ " + ev.text);
        break;
      case "done":
        break;
      case "snapshot":
        renderSnapshot(ev);
        break;
    }
  }

  /* ---------- polling for live orders ---------- */
  let pollTimer = null;
  async function pollOnce() {
    try {
      const r = await fetch(`/api/session/${sid}`);
      if (!r.ok) return;
      const snap = await r.json();
      renderSnapshot(snap);
      const active = (snap.orders || []).some(o => o.state !== "DELIVERED" && o.state !== "CANCELLED");
      if (active && !pollTimer) pollTimer = setInterval(pollOnce, 4000);
      if (!active && pollTimer) { clearInterval(pollTimer); pollTimer = null; }
    } catch { /* transient */ }
  }

  /* ---------- wire up ---------- */
  sendBtn.onclick = () => send(chatInput.value);
  chatInput.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !busy) send(chatInput.value);
  });
  document.querySelectorAll(".chip").forEach(c => c.onclick = () => send(c.dataset.q));
  document.querySelectorAll(".tab-btn").forEach(b => b.onclick = () => {
    document.querySelectorAll(".tab-btn").forEach(x => x.classList.remove("active"));
    b.classList.add("active");
    ["trace", "cart", "orders"].forEach(t => $("tab-" + t).classList.toggle("hidden", t !== b.dataset.tab));
  });
  $("budget-set").onclick = async () => {
    const amount = parseInt($("budget-input").value, 10);
    if (!amount || amount < 50) { setStatus("Budget must be ≥ ₹50", true); return; }
    const r = await fetch("/api/budget", { method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ session_id: sid, amount }) });
    if (r.ok) { const j = await r.json(); setStatus(`Budget set to ₹${j.budget} (cart ₹${j.total})`); pollOnce(); }
    else setStatus("Could not set budget", true);
  };

  /* health badge */
  fetch("/healthz").then(r => r.json()).then(h => {
    if (h.llm && h.llm.live) { $("live-badge").textContent = "● LIVE · LLM ON"; setMode("llm"); }
    else { $("live-badge").textContent = "● LIVE · OFFLINE PLANNER"; setMode("fallback"); }
  }).catch(() => {});

  pollOnce();
})();
