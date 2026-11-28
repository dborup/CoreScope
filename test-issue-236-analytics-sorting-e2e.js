'use strict';
const assert = require('assert');
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('Local test server required');
const names = ['Å', 'Ø', 'Æ', 'Z'];
// Shapes checked against ChannelAnalyticsResponse / AreaAnalyticsResponse.
const channels = { activeChannels: 4, decryptable: 4, topSenders: [], channelTimeline: [], msgLengths: [],
  channels: names.map((name,i)=>({name, hash:i, messages:i+1, senders:i+1, encrypted:false, lastActivity:`2026-01-0${i+1}T00:00:00Z`})) };
const areas = { density: names.map((label,i)=>({label,total:i+1,active:i+1,degraded:0,silent:0,roleCounts:{}})),
  bridgeNodes:names.map((name,i)=>({name,label:name,publicKey:String(i).repeat(64),edgeCount:i+1,otherAreaCount:i+1,otherAreas:[]})),
  positionGaps:names.map((label,i)=>({label,realFix:i+1,approximated:1})), estimatedNodes:[] };
const hashes = { distribution:{1:0,2:4,3:0},total:4,hourly:[],topHops:[],
  multiByteNodes:names.map((name,i)=>({name,pubkey:String(i).repeat(64),role:'repeater',hashSize:2,packets:i+1,lastSeen:`2026-01-0${i+1}T00:00:00Z`})),multiByteCapability:[] };
(async()=>{
  const browser = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH || undefined});
  try {
    const page = await browser.newPage({locale:'da-DK'});
    for (const [path, body] of [['channels',channels],['areas',areas],['hash-sizes',hashes]]) {
      await page.route('**/api/analytics/'+path+'*', route=>route.fulfill({json:body}));
    }
    async function load(tab,table) {
      await page.goto(BASE+'/#/analytics?tab='+tab);
      await page.locator(table+' tbody tr').first().waitFor();
      await page.waitForTimeout(500); // existing delayed Hash Stats handler/theme refresh
    }
    const firstCells = table=>page.locator(table+' tbody tr').evaluateAll(rows=>rows.filter(r=>r.cells.length>1).map(r=>r.cells[0].textContent.trim()));
    async function exercise(table,attr,text,numeric) {
      const selector=table+' th['+attr+'="'+text+'"]';
      await page.locator(selector).focus();
      await page.keyboard.press('Enter');
      assert.equal(await page.locator(selector).getAttribute('aria-sort'),'ascending');
      assert.deepEqual(await firstCells(table),['Z','Æ','Ø','Å']);
      assert(await page.locator(selector).evaluate(el=>el===document.activeElement),'focus survives sort');
      await page.keyboard.press('Space');
      assert.equal(await page.locator(selector).getAttribute('aria-sort'),'descending');
      assert.deepEqual(await firstCells(table),names);
      assert(await page.locator(selector).evaluate(el=>el===document.activeElement));
      await page.keyboard.press('Tab');
      assert(await page.evaluate(()=>document.activeElement.matches('th[tabindex="0"]')),'Tab reaches next header');
      const numericSelector=table+' th['+attr+'="'+numeric+'"]';
      await page.locator(numericSelector).click();
      assert.equal(await page.locator(numericSelector).getAttribute('aria-sort'),'descending');
      assert.equal(await page.locator(table+' .sort-arrow:not([aria-hidden="true"])').count(),0);
    }
    await load('channels','#channelsTable');
    await exercise('#channelsTable','data-sort-col','name','senders');
    await page.reload();
    await page.locator('#channelsTable th[data-sort-col="senders"][aria-sort="descending"]').waitFor();
    await load('areas','#areasDensity table');
    await exercise('#areasDensity table','data-sort-col','area','total');
    await exercise('#areasBridgeNodes table','data-sort-col','node','edgeCount');
    await exercise('#areasPositionGaps table','data-sort-col','area','realFix');
    await load('hashsizes','#mbAdoptersTable');
    await exercise('#mbAdoptersTable','data-sort','name','packets');
    assert((await page.evaluate(()=>location.hash)).includes('mbdir=desc'));
    await page.reload();
    await page.locator('#mbAdoptersTable th[data-sort="packets"][aria-sort="descending"]').waitFor();
    await page.goto(BASE+'/#/analytics?tab=hashsizes&mbsort=packets&mbdir=asc');
    await page.reload();
    await page.locator('#mbAdoptersTable th[data-sort="packets"][aria-sort="ascending"]').waitFor();
    assert.deepEqual(await firstCells('#mbAdoptersTable'),names);
    await page.locator('#mbAdoptersTable th[data-sort="name"]').focus();
    if (process.env.SCREENSHOT_PATH) await page.screenshot({path:process.env.SCREENSHOT_PATH,fullPage:true});
    console.log('PASS: five analytics tables, Danish collation, Enter/Space/Tab, focus, numeric defaults, persistence and explicit asc deep link');
  } finally { await browser.close(); }
})().catch(e=>{console.error(e);process.exitCode=1;});
