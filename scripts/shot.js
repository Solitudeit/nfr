// Screenshot helper for scripts/screenshots.py.
//
// Renders one panel URL with the session cookie the Python driver already
// negotiated, so the browser never has to type the password. Configuration
// arrives through the environment (SHOT_*) and the file named by SHOT_COOKIES.
//
//   SHOT_URL       page to render
//   SHOT_OUT       destination PNG
//   SHOT_COOKIES   JSON array of {name, value} cookies
//   SHOT_VIEWPORT  WxH, e.g. 1440x900
//   SHOT_FULL      1 to capture the whole page
//   SHOT_DARK      1 to flip the theme toggle before capturing

const fs = require("fs");

const playwright = require(process.env.SHOT_PLAYWRIGHT || "playwright");

(async () => {
  const [width, height] = (process.env.SHOT_VIEWPORT || "1440x900").split("x").map(Number);
  const origin = new URL(process.env.SHOT_URL);

  const browser = await playwright.chromium.launch();
  const context = await browser.newContext({
    viewport: { width, height },
    deviceScaleFactor: 2,
    ignoreHTTPSErrors: true,
    locale: "zh-CN",
    timezoneId: "Asia/Taipei",
    // Always start from light: the panel's toggle flips whatever the current
    // theme resolves to, so a dark prefers-color-scheme would flip it back.
    colorScheme: "light",
  });

  const cookies = JSON.parse(fs.readFileSync(process.env.SHOT_COOKIES, "utf8"));
  await context.addCookies(
    cookies.map((c) => ({
      name: c.name,
      value: c.value,
      domain: origin.hostname,
      path: "/",
      httpOnly: true,
      secure: true,
    }))
  );

  const page = await context.newPage();
  const resp = await page.goto(process.env.SHOT_URL, { waitUntil: "networkidle", timeout: 30000 });
  if (resp && resp.status() >= 400) {
    throw new Error("page returned " + resp.status() + " for " + process.env.SHOT_URL);
  }

  if (process.env.SHOT_DARK === "1") {
    // The panel remembers the choice in localStorage; clicking the toggle is
    // what actually repaints, so prefer it when the button is present.
    const toggle = page.locator("[data-theme-toggle]").first();
    if (await toggle.count()) {
      await toggle.click();
      await page.waitForTimeout(250);
    } else {
      await page.evaluate(() => {
        document.documentElement.dataset.theme = "dark";
      });
      await page.waitForTimeout(250);
    }
  }

  await page.waitForTimeout(500);
  await page.screenshot({ path: process.env.SHOT_OUT, fullPage: process.env.SHOT_FULL === "1" });
  console.log("ok");
  await browser.close();
})().catch((err) => {
  console.error(err && err.message ? err.message : String(err));
  process.exit(1);
});
