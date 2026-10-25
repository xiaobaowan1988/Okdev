// 环境页:列出 deployer 里该环境的服务(期望+观测状态),服务名跳到原生工作负载页。
const { URL, login, pickEnv } = require('./lib');
(async () => {
  const { b, p } = await login();
  await p.goto(URL + '/c/main/services'); await p.waitForTimeout(3000);
  console.log('sidebar:', (await p.locator('.MuiDrawer-root a').allInnerTexts()).filter(Boolean).join(' | '));
  await pickEnv(p, 'dev3'); await p.waitForTimeout(2500);
  console.log((await p.locator('tbody tr').allInnerTexts()).map(r => r.replace(/\s+/g, ' ')).join('\n'));
  await p.getByRole('link', { name: 'api', exact: true }).first().click(); await p.waitForTimeout(3000);
  console.log('service link ->', p.url().replace(URL, ''), '| page:', (await p.locator('main').innerText()).slice(0, 30).replace(/\n/g, ' '));
  await b.close();
})();
