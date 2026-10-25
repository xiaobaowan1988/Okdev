const {open}=require('./lib');
(async()=>{
  const {b,p}=await open();
  await p.goto('http://localhost:4466/c/main/services'); await p.waitForTimeout(2000);
  await p.getByTestId('env-select').click(); await p.getByRole('option',{name:'dev',exact:true}).click();
  await p.waitForTimeout(2500);
  await p.screenshot({path:'s1.png'});
  const rows = await p.locator('tbody tr').allInnerTexts();
  console.log(rows.map(r=>r.replace(/\s+/g,' ')).join('\n'));
  for (const [name,seg] of [['hello','deployments'],['db','statefulsets'],['nightly','cronjobs']]) {
    await p.getByRole('link',{name,exact:true}).first().click(); await p.waitForTimeout(1500);
    console.log(name,'->',p.url().replace('http://localhost:4466',''), p.url().includes('/'+seg+'/dev/'+name)?'OK':'BAD');
    await p.goBack(); await p.waitForTimeout(1500);
  }
  await b.close();
})();
