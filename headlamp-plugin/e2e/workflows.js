const {open}=require('./lib');
async function run(p, ref){
  await p.goto('http://localhost:4466/c/main/workflows'); await p.waitForTimeout(2000);
  await p.getByTestId('env-select').click(); await p.getByRole('option',{name:'dev',exact:true}).click(); await p.waitForTimeout(1500);
  await p.getByTestId('service-input').click(); await p.getByRole('option',{name:/^hello/}).click(); await p.keyboard.press('Escape');
  await p.waitForTimeout(2500);
  await p.getByTestId('ref-hello').click(); await p.getByRole('option',{name:ref}).click();
  await p.getByTestId('run-button').click();
  await p.waitForURL(/\/workflows\/dev\/hello-/, {timeout:20000});
}
(async()=>{
  const {b,p}=await open();
  await run(p,'main');
  const seen=new Set(); let logSeen='';
  for (let i=0;i<60;i++){
    const ph = await p.getByTestId('phase-chip').first().innerText().catch(()=>'?'); seen.add(ph);
    if (ph==='Running'){ await p.getByTestId('tab-build').click().catch(()=>{}); logSeen = (await p.getByTestId('log-view').innerText().catch(()=>'')).replace(/\s+/g,' ').slice(0,100) || logSeen; }
    if (/Success|Failed/.test(ph)) break;
    await p.waitForTimeout(700);
  }
  console.log('phases seen:', [...seen].join(' -> '), '| live build log while running:', logSeen);
  await p.waitForTimeout(2000);
  await p.getByTestId('tab-release').click(); await p.waitForTimeout(2000);
  console.log('release log:', (await p.getByTestId('log-view').innerText()).replace(/\s+/g,' ').slice(0,200));
  await b.close();
})();
