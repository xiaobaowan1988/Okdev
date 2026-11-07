// 驱动 `okdev login`(设备授权流程)的浏览器一步:读命令输出里的网址,在 Dex 登录页用 Zadig 账号登录。
// 用法: node login.js <用户名> <密码> <okdev 凭证文件路径>    环境变量: OKDEV_BIN(okdev 二进制)、ISSUER、CA_FILE
const { chromium } = require('playwright');
const { spawn } = require('child_process');
const [user, pass, credFile] = process.argv.slice(2);
const bin = process.env.OKDEV_BIN || 'okdev';
(async () => {
  const cli = spawn(bin, ['login', '--issuer', process.env.ISSUER, '--ca-file', process.env.CA_FILE, '--credentials', credFile], { stdio: ['ignore', 'pipe', 'pipe'] });
  let out = '';
  const done = new Promise((res) => cli.on('exit', (code) => res(code)));
  cli.stderr.on('data', (d) => (out += d)); cli.stdout.on('data', (d) => (out += d));
  let url;
  for (let i = 0; i < 100 && !url; i++) { url = (out.match(/https?:\/\/\S+/) || [])[0]; await new Promise((r) => setTimeout(r, 100)); }
  if (!url) { console.error('no login URL from okdev login:\n' + out); process.exit(1); }
  console.log('device login URL:', url.replace(/user_code=.*/, 'user_code=…'));
  // 沙箱里浏览器默认走出口代理,访问不到节点 IP 上的 Dex,所以直连
  const b = await chromium.launch({ args: ['--proxy-bypass-list=*', '--proxy-server=direct://'] });
  const p = await (await b.newContext({ ignoreHTTPSErrors: true })).newPage();
  await p.goto(url);
  if (await p.locator('input[name=user_code]').count()) { // 打开的是不带验证码的页面时
    await p.locator('input[name=user_code]').fill((out.match(/enter: (\S+)/) || [])[1]); await p.locator('button[type=submit]').click();
  }
  await p.locator('input[name=login]').fill(user); await p.locator('input[name=password]').fill(pass);
  await p.locator('button[type=submit]').click(); await p.waitForTimeout(1500);
  if (await p.locator('#login-error').count()) console.log('Dex rejected the login');
  else { const approve = p.locator('button:has-text("Grant Access"), button[value=approve]'); if (await approve.count()) await approve.first().click(); }
  await p.waitForTimeout(1500);
  console.log('browser page:', (await p.locator('body').innerText()).replace(/\s+/g, ' ').slice(0, 120));
  await b.close();
  const code = await Promise.race([done, new Promise((r) => setTimeout(() => r('timeout'), 20000))]);
  console.log('okdev login exit:', code); if (code === 'timeout') cli.kill();
  process.exit(code === 0 ? 0 : 1);
})();
