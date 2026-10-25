// 工作流页:多选服务、各选分支/Tag,运行,等任务完成;再单服务运行并查看详情页(状态条、事件、构建日志)。
const { URL, login, pickEnv } = require('./lib');
const waitDone = (p, sel) => p.waitForFunction(s => /Success|Failed/.test(document.querySelector(s)?.textContent || ''), sel, { timeout: 240000 });
(async () => {
  const { b, p } = await login();
  await p.goto(URL + '/c/main/workflows'); await p.waitForTimeout(3000);
  await pickEnv(p, 'dev5');
  await p.getByTestId('service-input').click();
  await p.getByRole('option', { name: 'api', exact: true }).click();
  await p.getByRole('option', { name: 'api-b', exact: true }).click();
  await p.keyboard.press('Escape'); await p.waitForTimeout(4000);
  console.log('free-text inputs in rows (expect 0):', await p.locator('[data-testid^=row-] input[type=text]:not([aria-hidden])').count());
  console.log('api    repo:', (await p.getByTestId('repo-api').innerText()).trim(), '| default ref:', await p.getByTestId('ref-api').innerText());
  await p.getByTestId('ref-api-b').click();
  console.log('api-b options:', (await p.getByRole('option').allInnerTexts()).slice(0, 6).join(','), '...');
  await p.getByRole('option', { name: 'master', exact: true }).click();
  await p.screenshot({ path: 'workflow-form.png', fullPage: true });
  console.log('run button:', await p.getByTestId('run-button').innerText());
  await p.getByTestId('run-button').click();
  await p.getByTestId('created-tasks').waitFor({ timeout: 20000 });
  console.log('created:', (await p.getByTestId('created-tasks').innerText()).replace(/\s+/g, ' '));
  for (let i = 0; i < 80; i++) {
    const rows = (await p.locator('tbody tr').allInnerTexts()).slice(0, 2).map(r => r.replace(/\s+/g, ' '));
    if (rows.length === 2 && rows.every(r => /Success|Failed/.test(r))) { console.log(rows.join('\n')); break; }
    await p.waitForTimeout(4000);
  }
  // 单服务:直接进详情页
  await p.reload(); await p.waitForTimeout(3000); await pickEnv(p, 'dev3');
  await p.getByTestId('service-input').click(); await p.getByRole('option', { name: 'api', exact: true }).click(); await p.keyboard.press('Escape');
  await p.waitForTimeout(3500); await p.getByTestId('run-button').click();
  await p.waitForURL(/\/workflows\/okdev-releases\//, { timeout: 30000 });
  console.log('single run -> detail', p.url().replace(URL, ''));
  const phases = new Set();
  for (let i = 0; i < 100; i++) {
    const ph = await p.getByTestId('phase-chip').first().innerText().catch(() => '?'); phases.add(ph);
    if (/Success|Failed/.test(ph)) break; await p.waitForTimeout(2500);
  }
  await p.waitForTimeout(4000);
  console.log('phases seen:', [...phases].join(' -> '));
  console.log('events:', (await p.getByTestId('event-timeline').innerText()).replace(/\n+/g, ' | ').slice(0, 260));
  console.log('log tail:', (await p.getByTestId('log-view').innerText()).replace(/\s+/g, ' ').slice(-160));
  await p.screenshot({ path: 'task-detail.png', fullPage: true });
  await b.close();
})();
