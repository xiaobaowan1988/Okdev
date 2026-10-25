// 端到端脚本的公共部分:用 Dex 登录(用户名密码),返回浏览器和页面。
// 环境变量:HEADLAMP_URL(默认 http://localhost:4466)、E2E_USER / E2E_PASSWORD(默认 alice / alice-pass-1)。
const { chromium } = require('playwright');
exports.URL = process.env.HEADLAMP_URL || 'http://localhost:4466';
exports.login = async (user = process.env.E2E_USER || 'alice', pass = process.env.E2E_PASSWORD || 'alice-pass-1') => {
  // 沙箱里浏览器默认走出口代理,访问不到节点 IP 上的 Dex,所以直连
  const b = await chromium.launch({ args: ['--proxy-bypass-list=*', '--proxy-server=direct://'] });
  const p = await (await b.newContext({ viewport: { width: 1400, height: 1000 }, ignoreHTTPSErrors: true })).newPage();
  p.on('pageerror', e => console.log('PAGEERROR', e.message.slice(0, 300)));
  await p.goto(exports.URL + '/c/main/workflows', { waitUntil: 'domcontentloaded' });
  await p.waitForURL(/\/auth\/local\/login|\/auth\?/, { timeout: 20000 }).catch(() => {});
  await p.locator('input[name=login]').fill(user);
  await p.locator('input[name=password]').fill(pass);
  await p.locator('button[type=submit]').click();
  await p.waitForURL(u => u.toString().startsWith(exports.URL), { timeout: 20000 });
  await p.waitForTimeout(4000);
  return { b, p };
};
exports.pickEnv = async (p, env) => {
  await p.getByTestId('env-select').click();
  await p.getByRole('option', { name: env, exact: true }).click();
  await p.waitForTimeout(1500);
};
