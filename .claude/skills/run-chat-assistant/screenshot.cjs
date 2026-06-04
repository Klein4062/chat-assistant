// screenshot.cjs — Playwright smoke test for chat-assistant
// Usage: BASE_URL=http://47.95.244.175 node screenshot.cjs
// Saves screenshot to PROJECT_DIR/screenshot.png

const { chromium } = require('playwright');
const path = require('path');

const BASE_URL = process.env.BASE_URL || 'http://47.95.244.175';
const OUTPUT = path.join(__dirname, '..', '..', '..', 'screenshot.png');

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 900, height: 700 } });

  console.log(`Opening ${BASE_URL} ...`);
  await page.goto(BASE_URL, { waitUntil: 'networkidle' });

  // Verify page loaded
  const title = await page.title();
  if (title !== 'AI 助手') throw new Error(`Unexpected title: ${title}`);
  console.log(`  Title: ${title}`);

  // Wait for WebSocket to connect
  await page.waitForFunction(() => {
    const el = document.querySelector('.status-text');
    return el && el.textContent === '已连接';
  }, { timeout: 10000 });
  console.log('  WebSocket: connected');

  // Type and send a message
  await page.fill('#messageInput', '你好！这是一条测试消息');
  await page.click('#sendButton');

  // Wait for echo response
  await page.waitForFunction(() => {
    const bubbles = document.querySelectorAll('.message-bubble');
    if (bubbles.length >= 2) {
      return bubbles[bubbles.length - 1].textContent.includes('Echo:');
    }
    return false;
  }, { timeout: 10000 });

  const msgCount = await page.locator('.message-row').count();
  console.log(`  Messages: ${msgCount} bubbles`);

  // Screenshot
  await page.screenshot({ path: OUTPUT });
  console.log(`  Screenshot: ${OUTPUT}`);

  await browser.close();
  console.log('✅ Chat smoke test passed');
})().catch(err => {
  console.error('❌ Failed:', err.message);
  process.exit(1);
});
