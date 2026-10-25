const {open}=require('./lib');
(async()=>{
  const {b,p}=await open();
  await p.goto('http://localhost:4466/c/main/workflows'); await p.waitForTimeout(2000);
  await p.getByTestId('env-select').click(); await p.getByRole('option',{name:'dev',exact:true}).click(); await p.waitForTimeout(2000);
  await p.getByTestId('service-input').click();
  await p.getByRole('option',{name:/^hello/}).click(); await p.getByRole('option',{name:/^world/}).click();
  await p.keyboard.press('Escape');
  await p.waitForTimeout(500);
  console.log('hello prefill:', await p.getByTestId('repo-hello').inputValue(), await p.getByTestId('ref-hello').inputValue());
  console.log('world prefill:', await p.getByTestId('repo-world').inputValue(), await p.getByTestId('ref-world').inputValue());
  await p.getByTestId('ref-world').fill('feature');   // 各自选分支
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
