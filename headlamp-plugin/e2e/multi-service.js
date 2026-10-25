const {open}=require('./lib');
(async()=>{
  const {b,p}=await open();
  await p.goto('http://localhost:4466/c/main/workflows'); await p.waitForTimeout(2000);
  await p.getByTestId('env-select').click(); await p.getByRole('option',{name:'dev',exact:true}).click(); await p.waitForTimeout(2000);
  await p.getByTestId('service-input').click();
  await p.getByRole('option',{name:/^hello/}).click(); await p.getByRole('option',{name:/^world/}).click();
  await p.keyboard.press('Escape');
  await p.waitForTimeout(500);
  await p.waitForTimeout(2500);
  console.log('free-text inputs in rows (expect 0):', await p.locator('[data-testid^=row-] input:not([aria-hidden])').count());
  console.log('hello repo:', (await p.getByTestId('repo-hello').innerText()).replace(/\s+/g,' '));
  console.log('hello default ref:', await p.getByTestId('ref-hello').innerText());
  console.log('world default ref:', await p.getByTestId('ref-world').innerText());
  await p.getByTestId('ref-world').click();           // 各自选分支:打开下拉
  console.log('world options:', (await p.getByRole('option').allInnerTexts()).join(','));
  await p.getByRole('option',{name:'feature'}).click();
  await p.screenshot({path:'m1.png', fullPage:true});
  console.log('button:', await p.getByTestId('run-button').innerText());
  await p.getByTestId('run-button').click();
  await p.getByTestId('created-tasks').waitFor({timeout:20000});
  console.log('created:', (await p.getByTestId('created-tasks').innerText()).replace(/\s+/g,' '));
  await p.waitForTimeout(500);
  console.log('form rows left:', await p.locator('[data-testid^=row-]').count());
  // 等两个任务结束
  for (let i=0;i<60;i++){ const t=await p.locator('tbody tr').allInnerTexts(); const top=t.slice(0,2).join(' ');
    if(!/Pending|Running/.test(top) && /Success|Failed/.test(top)) break; await p.waitForTimeout(3000);}
  await p.screenshot({path:'m2.png', fullPage:true});
  console.log((await p.locator('tbody tr').allInnerTexts()).slice(0,3).map(r=>r.replace(/\s+/g,' ')).join('\n'));
  await b.close();
})();
