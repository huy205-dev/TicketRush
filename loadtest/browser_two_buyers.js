// SPEC.md M5 completion check: two browser tabs, two different users, each
// buys a ticket end to end through the real frontend: dev-login, waiting
// room (SSE), seat map, order with the admission token, fakepay's payment
// page, and the QR code once the ticket service has issued the ticket.
//
// Drives the installed Chrome headless with k6's browser module: one browser
// context, one tab per buyer (the frontend keeps its state in the tab's
// sessionStorage), both buying at the same time; fakepay opens in a third
// tab. Needs the whole system running behind nginx:
//   make up && make migrate && make seed (once)
//   REQUIRE_ADMISSION=true booking, waitingroom, relay, ticket, fakepay
//   (FAKEPAY_LATE_RATE=0, so no webhook waits 11 minutes)
// Run: make e2e-browser   (BASE_URL defaults to http://localhost:8000)
import { browser } from "k6/browser";
import { check } from "k6";
import { Counter } from "k6/metrics";

const buyersDone = new Counter("buyers_done");

export const options = {
  scenarios: {
    two_buyers: {
      executor: "shared-iterations",
      iterations: 1,
      options: { browser: { type: "chromium" } },
    },
  },
  // Zero checks would pass "rate==1.0"; the counter proves both finished.
  thresholds: { checks: ["rate==1.0"], buyers_done: ["count==2"] },
};

const BASE = __ENV.BASE_URL || "http://localhost:8000";
// Fresh user ids per run, so an earlier run's HELD order cannot get in the way.
const run = Math.floor(Date.now() / 1000) % 100000;
const buyers = [{ id: 900000 + run * 2 }, { id: 900001 + run * 2 }];

export default async function () {
  const context = await browser.newContext();
  try {
    // Tabs are opened one at a time (the browser module cannot open two at
    // once); the two purchases then run concurrently.
    const tabs = [];
    for (let i = 0; i < buyers.length; i++) tabs.push({ shop: await context.newPage(), pay: await context.newPage() });
    const results = await Promise.all(buyers.map((b, i) => buy(tabs[i], b, i)));
    check(results, {
      "both buyers got different seats": (r) => r[0].seat !== r[1].seat,
      "both buyers got a QR ticket": (r) => r.every((x) => x.qr),
    });
  } catch (e) {
    check(null, { [`flow completed: ${e}`]: () => false });
    throw e;
  } finally {
    await context.close();
  }
}

async function buy({ shop: page, pay }, buyer, i) {
  const step = (name, ok) => check(ok, { [`buyer ${i + 1}: ${name}`]: (v) => v });

  await page.goto(BASE + "/");
  await page.locator("#user-id").fill(String(buyer.id));
  await page.locator("#login-form button").click();
  await page.waitForSelector("#user-info:not([hidden])");

  // Waiting room: join, then wait for the "admitted" event to show the map.
  await page.locator("#join").click();
  await page.waitForSelector("#seats:not([hidden]) .seat.AVAILABLE", { timeout: 60000 });
  step("admitted by the waiting room", true);

  // Pick a free seat near the back, a different one for each buyer.
  const seat = await page.evaluate((n) => {
    const free = [...document.querySelectorAll(".seat.AVAILABLE")];
    return free[free.length - 1 - n].dataset.seat;
  }, i * 7);
  await page.locator(`button[data-seat="${seat}"]`).click();
  await page.locator("#book").click();
  await page.waitForSelector("#order:not([hidden])", { timeout: 15000 });
  const summary = await page.locator("#order-summary").textContent();
  step("order created for the chosen seat", summary.includes(seat));

  // Pay on fakepay's page, as the buyer does after clicking "Thanh toán".
  // payment_url can be missing on the first answer if fakepay was slow; the
  // page fetches it again by polling the order.
  let payURL = null;
  for (let tries = 0; tries < 20 && !payURL; tries++) {
    payURL = await page.evaluate(() => (current && current.payment_url) || null);
    if (!payURL) await page.waitForTimeout(500);
  }
  step("payment link received", !!payURL);
  await pay.goto(payURL);
  await pay.locator("button.ok").click();
  await pay.waitForSelector("#result:not(:empty)");
  step("fakepay accepted the payment", (await pay.locator("#result").textContent()).includes("SUCCEEDED"));
  await pay.close();

  // The order page polls until TICKETED and then shows the QR codes.
  await page.waitForSelector(".ticket img", { timeout: 90000 });
  const src = await page.locator(".ticket img").getAttribute("src");
  const qr = !!src && src.startsWith("data:image/png;base64,") && src.length > 200;
  step("QR ticket shown", qr);
  await page.screenshot({ path: `loadtest/out/browser-buyer-${i + 1}.png`, fullPage: true });
  await page.close();
  buyersDone.add(1);
  return { seat, qr };
}
