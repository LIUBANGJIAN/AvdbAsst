/**
 * 量结果页筛选栏占几行 —— 手工验证脚本，不是 CI 依赖。
 *
 * 为什么需要它：筛选栏"一行到底"这件事，go test 量不了（没有浏览器），
 * 而它已经因为两个机制反复坏过两次——
 *   1. flex 的断行依据是**基准尺寸之和**，不是实际渲染宽度：
 *      靠收缩挤进一行没用，基准之和超了照样提前断行；
 *   2. 原生 <select> 的宽度由**最宽的 option 文字**撑开，不是 min-width。
 * go 侧的样式契约见 paging_test.go 的 TestToolbarStaysOnOneRow，
 * 那个只能守住"规则还在"，**真正占几行必须用这个脚本量**。
 *
 * 用法：
 *   BASE_URL=http://127.0.0.1:8080 node scripts/measure-toolbar.mjs
 *
 * 需要本机有 playwright；若浏览器版本对不上，用 CHROMIUM 指定可执行文件：
 *   CHROMIUM=/path/to/chrome.exe BASE_URL=... node scripts/measure-toolbar.mjs
 * playwright 没装在仓库里时，用 NODE_PATH 指过去即可（下面用 createRequire
 * 而不是 import，就是为了让 NODE_PATH 生效——ESM 的 import 不认它）：
 *   NODE_PATH=/path/to/node_modules node scripts/measure-toolbar.mjs
 *
 * 判据：
 *   > 980px  → 必须 1 行
 *   ≤ 980px  → 允许折行，但**任何宽度都不许溢出**（横向滚动条比折行更糟）
 */
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://127.0.0.1:8080';
const WIDTHS = [640, 900, 980, 981, 1000, 1074, 1180, 1280, 1440, 1600];
const ONE_LINE_FROM = 981;

const browser = await chromium.launch(
  process.env.CHROMIUM ? { executablePath: process.env.CHROMIUM } : {}
);
const page = await browser.newPage();
let failed = 0;

for (const w of WIDTHS) {
  await page.setViewportSize({ width: w, height: 900 });
  await page.goto(BASE + '/s?q=' + encodeURIComponent(process.env.QUERY || 'HD'), {
    waitUntil: 'domcontentloaded',
  });
  await page.waitForSelector('.row');

  const info = await page.evaluate(() => {
    const form = document.getElementById('filter-form');
    const cs = getComputedStyle(form);
    const kids = Array.from(form.children).filter(
      (el) => el.tagName !== 'NOSCRIPT' && el.type !== 'hidden' && el.offsetParent !== null
    );
    const rects = kids.map((el) => {
      const r = el.getBoundingClientRect();
      return { name: el.id || el.className, w: Math.round(r.width), bottom: Math.round(r.bottom), right: r.right };
    });

    // 按**底边**聚类判行：工具栏 align-items: flex-end，同一 flex 行内底边相同。
    // 按 top 判会把不同高度的控件误判成多行（这个坑误判过两次）。
    const rows = [];
    for (const r of rects) {
      let row = rows.find((x) => Math.abs(x.bottom - r.bottom) <= 2);
      if (!row) {
        row = { bottom: r.bottom, items: [] };
        rows.push(row);
      }
      row.items.push(r);
    }

    const box = form.getBoundingClientRect();
    const limit = box.right - parseFloat(cs.paddingRight);
    return {
      lines: rows.length,
      rowDetail: rows.map((row) => row.items.map((i) => i.name + ':' + i.w).join(', ')),
      overflow: Math.round(Math.max(...rects.map((r) => r.right)) - limit),
      pageOverflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      filterW: Math.round(document.getElementById('f-filter').getBoundingClientRect().width),
      firstRowTop: Math.round(document.getElementById('results').getBoundingClientRect().top),
    };
  });

  const ok = info.overflow <= 1 && info.pageOverflow <= 0 && (w < ONE_LINE_FROM || info.lines === 1);
  if (!ok) failed++;
  console.log(
    `${String(w).padStart(4)}px · ${info.lines} 行 · 溢出 ${info.overflow}px · ` +
      `过滤框 ${info.filterW}px · 首行顶 ${info.firstRowTop}px${ok ? '' : '  <-- 不合格'}`
  );
  info.rowDetail.forEach((d, i) => console.log(`        第 ${i + 1} 行: ${d}`));
}

await browser.close();
console.log(failed === 0 ? '\n[全部合格]' : `\n[有 ${failed} 个宽度不合格]`);
process.exit(failed === 0 ? 0 : 1);
