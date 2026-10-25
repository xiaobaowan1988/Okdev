const { chromium } = require('playwright');
const fs = require('fs');
exports.open = async () => {
  const b = await chromium.launch(); const p = await (await b.newContext({viewport:{width:1400,height:1000}})).newPage();
  p.on('pageerror', e => console.log('PAGEERROR', e.message.slice(0,300)));
  await p.goto('http://localhost:4466/c/main/token');
  await p.getByLabel('ID token').first().fill(fs.readFileSync(process.env.TOKEN_FILE || '/tmp/token', 'utf8').trim());
  await p.getByRole('button', {name:'Authenticate'}).click();
  await p.waitForTimeout(3000);
  return {b, p};
};
