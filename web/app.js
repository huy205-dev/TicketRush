// TicketRush frontend: dev-login, waiting room, seat map, order, tickets.
// Plain JavaScript, no build step. Served by booking; the waiting room is
// reached through the same origin (nginx routes /v1/queue/* to it).
// State lives in sessionStorage, so two tabs can be two different buyers.
"use strict";

const eventId = Number(new URLSearchParams(location.search).get("event") || 1);
const POLL_MS = 2000;
const $ = (id) => document.getElementById(id);

const store = {
  get(k) { try { return JSON.parse(sessionStorage.getItem(k)); } catch { return null; } },
  set(k, v) { if (v == null) sessionStorage.removeItem(k); else sessionStorage.setItem(k, JSON.stringify(v)); },
};

let user = store.get("user");           // { id, token }
let admission = store.get("admission"); // { token, expiresAt } for this event
let orderId = store.get("order");
let event = null;
let selected = new Set();
let seatEls = new Map();
let seatStatus = new Map();
let seatsEtag = "";
let seatsTimer = 0, orderTimer = 0, countdownTimer = 0;
let stream = null;
let pendingKey = null; // Idempotency-Key reused when the same booking is retried

function say(text, isError) {
  $("message").textContent = text || "";
  $("message").className = isError ? "error" : "";
}

function vnd(n) { return n.toLocaleString("vi-VN") + " đ"; }

async function api(path, { method = "GET", body, headers = {} } = {}) {
  const h = { ...headers };
  if (user) h.Authorization = "Bearer " + user.token;
  if (body !== undefined) h["Content-Type"] = "application/json";
  const res = await fetch(path, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body), cache: "no-store" });
  if (res.status === 304) return { status: 304 };
  let data = null;
  try { data = await res.json(); } catch { /* empty body */ }
  if (!res.ok) {
    const e = (data && data.error) || {};
    const err = new Error(e.message || "HTTP " + res.status);
    Object.assign(err, { status: res.status, code: e.code, details: e.details || {} });
    throw err;
  }
  return { status: res.status, data, etag: res.headers.get("ETag") };
}

// ---- login --------------------------------------------------------------

$("login-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const id = Number($("user-id").value);
  try {
    const { data } = await api("/v1/auth/dev-login", { method: "POST", body: { user_id: id } });
    user = { id, token: data.access_token };
    store.set("user", user);
    render();
  } catch (e) { say("Đăng nhập thất bại: " + e.message, true); }
});

$("logout").addEventListener("click", () => {
  sessionStorage.clear();
  location.reload();
});

// ---- waiting room -------------------------------------------------------

$("join").addEventListener("click", join);

async function join() {
  say("");
  try {
    const { data } = await api(`/v1/queue/${eventId}/join`, { method: "POST" });
    follow(data.queue_id);
  } catch (e) { say("Không vào hàng được: " + e.message, true); }
}

function follow(queueId) {
  if (stream) stream.close();
  $("join").disabled = true;
  $("queue-status").textContent = "Đang vào hàng…";
  stream = new EventSource(`/v1/queue/${eventId}/stream?queue_id=${encodeURIComponent(queueId)}`);
  stream.addEventListener("position", (m) => {
    const p = JSON.parse(m.data);
    $("queue-status").textContent = `Vị trí của bạn: ${p.position}. Chờ khoảng ${p.estimated_wait_seconds} giây.`;
  });
  stream.addEventListener("admitted", (m) => {
    const a = JSON.parse(m.data);
    stream.close(); stream = null;
    admission = { token: a.admission_token, expiresAt: Date.now() + a.expires_in * 1000 };
    store.set("admission", admission);
    say("Đã đến lượt bạn. Hãy chọn ghế.");
    render();
  });
  stream.addEventListener("expired", () => {
    stream.close(); stream = null;
    $("join").disabled = false;
    $("queue-status").textContent = "";
    say("Lượt của bạn đã hết hạn, hãy vào hàng lại.", true);
  });
  stream.onerror = () => {
    // EventSource reconnects by itself; a closed stream means it gave up.
    if (stream && stream.readyState === EventSource.CLOSED) {
      $("join").disabled = false;
      say("Mất kết nối với phòng chờ, hãy bấm vào hàng lại.", true);
    }
  };
}

function admissionValid() { return admission && admission.expiresAt > Date.now(); }

// ---- seat map -----------------------------------------------------------

async function loadSeats() {
  try {
    const headers = seatsEtag ? { "If-None-Match": seatsEtag } : {};
    const res = await api(`/v1/events/${eventId}/seats`, { headers });
    if (res.status === 200) {
      seatsEtag = res.etag || "";
      paintSeats(res.data.seats);
    }
  } catch (e) { say("Không tải được sơ đồ ghế: " + e.message, true); }
}

function paintSeats(seats) {
  const map = $("seat-map");
  if (seatEls.size === 0) {
    let zone = null, row = null, zoneEl, rowEl;
    for (const s of seats) {
      if (s.zone !== zone) {
        zone = s.zone; row = null;
        zoneEl = document.createElement("div"); zoneEl.className = "zone";
        const h = document.createElement("h3"); h.textContent = `${s.zone} · ${vnd(s.price_vnd)}`;
        zoneEl.appendChild(h); map.appendChild(zoneEl);
      }
      if (s.row !== row) {
        row = s.row;
        rowEl = document.createElement("div"); rowEl.className = "row";
        const l = document.createElement("span"); l.className = "row-label"; l.textContent = s.row;
        rowEl.appendChild(l); zoneEl.appendChild(rowEl);
      }
      const b = document.createElement("button");
      b.type = "button"; b.dataset.seat = s.seat_id; b.title = `${s.seat_id} · ${vnd(s.price_vnd)}`;
      b.addEventListener("click", () => toggleSeat(s.seat_id));
      rowEl.appendChild(b);
      seatEls.set(s.seat_id, { el: b, price: s.price_vnd });
    }
  }
  for (const s of seats) {
    seatStatus.set(s.seat_id, s.status);
    if (s.status !== "AVAILABLE") selected.delete(s.seat_id);
  }
  refreshSeatClasses();
}

function refreshSeatClasses() {
  for (const [id, { el }] of seatEls) {
    const st = seatStatus.get(id) || "SOLD";
    el.className = "seat " + (selected.has(id) ? "selected" : st);
    el.disabled = st !== "AVAILABLE" && !selected.has(id);
  }
  const max = event ? event.max_seats_per_order : 4;
  const total = [...selected].reduce((n, id) => n + seatEls.get(id).price, 0);
  $("selection").textContent = selected.size
    ? `Đã chọn ${selected.size}/${max} ghế: ${[...selected].join(", ")} · ${vnd(total)}`
    : `Chọn tối đa ${max} ghế`;
  $("book").disabled = selected.size === 0;
}

function toggleSeat(id) {
  const max = event ? event.max_seats_per_order : 4;
  if (selected.has(id)) selected.delete(id);
  else if (seatStatus.get(id) === "AVAILABLE" && selected.size < max) selected.add(id);
  else if (selected.size >= max) say(`Mỗi đơn tối đa ${max} ghế.`, true);
  pendingKey = null; // a different selection is a different request
  refreshSeatClasses();
}

$("book").addEventListener("click", book);

async function book() {
  say("");
  if (!admissionValid()) { admission = null; store.set("admission", null); say("Lượt vào đã hết hạn, hãy vào hàng lại.", true); render(); return; }
  pendingKey = pendingKey || crypto.randomUUID();
  $("book").disabled = true;
  try {
    const { data } = await api("/v1/orders", {
      method: "POST",
      body: { event_id: eventId, seat_ids: [...selected] },
      headers: { "Idempotency-Key": pendingKey, "X-Admission-Token": admission.token },
    });
    pendingKey = null; selected.clear();
    orderId = data.order_id; store.set("order", orderId);
    showOrder(data);
    render();
  } catch (e) {
    if (e.code === "SEATS_UNAVAILABLE") {
      pendingKey = null;
      for (const id of e.details.seat_ids || []) { selected.delete(id); seatStatus.set(id, "HELD"); }
      say("Ghế vừa có người giữ: " + (e.details.seat_ids || []).join(", ") + ". Hãy chọn ghế khác.", true);
    } else if (e.code === "ADMISSION_REQUIRED" || e.code === "ADMISSION_ALREADY_USED") {
      pendingKey = null; admission = null; store.set("admission", null);
      say("Cần vào hàng chờ lại trước khi đặt vé.", true);
      render();
    } else if (e.status === 409 && e.code === "IDEMPOTENCY_KEY_IN_PROGRESS") {
      say("Đơn đang được xử lý, thử lại sau giây lát.", true);
    } else {
      pendingKey = e.status >= 500 || e.status === 429 ? pendingKey : null;
      say("Không đặt được vé: " + e.message, true);
    }
    refreshSeatClasses();
    loadSeats();
  }
}

// ---- order --------------------------------------------------------------

const statusText = {
  HELD: "Đang giữ ghế, chờ thanh toán", PAID: "Đã thanh toán, đang xuất vé", TICKETED: "Đã xuất vé",
  EXPIRED: "Hết hạn giữ ghế", CANCELLED: "Đã huỷ", REFUNDING: "Đang hoàn tiền", REFUNDED: "Đã hoàn tiền",
};

let current = null;

function showOrder(o) {
  current = o;
  $("order-summary").textContent = `Đơn ${o.order_id}: ${o.seats.map((s) => s.seat_id).join(", ")} · ${vnd(o.total_vnd)}`;
  $("order-status").textContent = "Trạng thái: " + (statusText[o.status] || o.status);
  $("pay").hidden = o.status !== "HELD";
  $("cancel").hidden = o.status !== "HELD";
  clearInterval(countdownTimer);
  if (o.status === "HELD") {
    const tick = () => {
      const left = Math.max(0, Math.round((Date.parse(o.hold_expires_at) - Date.now()) / 1000));
      $("countdown").textContent = `Giữ ghế còn ${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`;
    };
    tick(); countdownTimer = setInterval(tick, 1000);
  } else {
    $("countdown").textContent = "";
  }
  if (o.status === "TICKETED") showTickets(o.order_id);
}

async function pollOrder() {
  if (!orderId) return;
  try {
    const { data } = await api(`/v1/orders/${orderId}`);
    showOrder(data);
    if (["TICKETED", "EXPIRED", "CANCELLED", "REFUNDED"].includes(data.status)) clearInterval(orderTimer);
  } catch (e) {
    if (e.status === 404) { orderId = null; store.set("order", null); render(); }
  }
}

$("pay").addEventListener("click", async () => {
  if (!current) return;
  let url = current.payment_url;
  if (!url) { await pollOrder(); url = current.payment_url; }
  if (!url) { say("Chưa lấy được link thanh toán, thử lại sau giây lát.", true); return; }
  window.open(url, "_blank");
  say("Đã mở trang thanh toán. Trang này tự cập nhật khi vé được xuất.");
});

$("cancel").addEventListener("click", async () => {
  try {
    const { data } = await api(`/v1/orders/${orderId}/cancel`, { method: "POST" });
    showOrder(data);
    say("Đã huỷ đơn.");
  } catch (e) { say("Không huỷ được: " + e.message, true); }
});

let ticketsShown = "";

async function showTickets(id) {
  if (ticketsShown === id) return;
  try {
    const { data } = await api(`/v1/orders/${id}/tickets`);
    const box = $("tickets");
    box.replaceChildren();
    for (const t of data.tickets) {
      const card = document.createElement("figure"); card.className = "ticket";
      const img = document.createElement("img");
      img.src = "data:image/png;base64," + t.qr_png_base64; img.alt = "QR vé " + t.seat_id;
      const cap = document.createElement("figcaption"); cap.textContent = `Ghế ${t.seat_id}`;
      card.append(img, cap); box.appendChild(card);
    }
    ticketsShown = id;
  } catch (e) { say("Không tải được vé: " + e.message, true); }
}

// ---- page state ---------------------------------------------------------

function render() {
  const loggedIn = !!user;
  $("login-form").hidden = loggedIn;
  $("user-info").hidden = !loggedIn;
  if (loggedIn) $("user-label").textContent = `User #${user.id}`;

  const hasOrder = loggedIn && !!orderId;
  const admitted = loggedIn && !hasOrder && admissionValid();
  $("queue").hidden = !loggedIn || hasOrder || admitted;
  $("seats").hidden = !admitted;
  $("order").hidden = !hasOrder;

  clearInterval(seatsTimer); clearInterval(orderTimer);
  if (admitted) {
    const left = () => Math.max(0, Math.round((admission.expiresAt - Date.now()) / 1000));
    $("admission-status").textContent = `Lượt vào của bạn còn ${left()} giây.`;
    loadSeats();
    seatsTimer = setInterval(() => {
      $("admission-status").textContent = `Lượt vào của bạn còn ${left()} giây.`;
      if (!admissionValid()) { admission = null; store.set("admission", null); render(); return; }
      loadSeats();
    }, POLL_MS);
  }
  if (hasOrder) { pollOrder(); orderTimer = setInterval(pollOrder, POLL_MS); }
  if (!$("queue").hidden) $("join").disabled = false;
}

async function start() {
  try {
    const { data } = await api(`/v1/events/${eventId}`);
    event = data;
    $("event-name").textContent = `${data.name} · ${data.venue}`;
  } catch (e) { say("Không tải được sự kiện: " + e.message, true); }
  render();
}

start();
