// E2E for the Nodes JSON export button (#/nodes → "Export JSON").
// Defaults to localhost:3000 — NEVER point at prod (AGENTS.md). CI sets BASE_URL.
//
// #1889 review fix 5: the narrowing check below searches by a unique
// PUBLIC-KEY PREFIX taken from one real exported contact, not "the first
// contact's name" — a human-chosen name can be a substring of several other
// nodes' names (or of none), so asserting the result "shrinks" after
// searching for it is not deterministic against a live, unknown dataset.
// A long-enough pubkey prefix (24 of 64 hex chars = 96 bits) is effectively
// guaranteed unique, so searching for it can be asserted against an EXACT
// expected outcome (narrows to exactly 1) instead of a fuzzy "got smaller"
// check — and that same exact-outcome assertion is correct whether the
// dataset started with 1 exportable contact or 10,000, so there is no
// separate "only one contact" special case to get wrong.
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://localhost:3000';

const REF_KEYS = ['type', 'name', 'custom_name', 'public_key', 'flags', 'latitude',
  'longitude', 'last_advert', 'last_modified', 'out_path_list'];

async function readDownloadedJson(download) {
  const stream = await download.createReadStream();
  let raw = '';
  for await (const chunk of stream) raw += chunk;
  return JSON.parse(raw);
}

function parseCount(label) {
  const m = label.match(/^Export JSON \((\d+)\)$/);
  return m ? Number(m[1]) : null;
}

(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage({ acceptDownloads: true });

  await page.goto(BASE + '/#/nodes');
  await page.waitForSelector('#nodesLeft[data-loaded="true"]', { timeout: 30000 });

  const btn = await page.$('#nodesExportBtn');
  if (!btn) throw new Error('#nodesExportBtn is missing from the nodes topbar');

  const label = (await btn.textContent()).trim();
  if (await btn.isDisabled()) {
    // No exportable node in this dataset (all lack a name, a valid 64-hex
    // pubkey, a known role, or a usable in-range position).
    if (label !== 'Export JSON') {
      throw new Error('disabled button should carry no count, got "' + label + '"');
    }
    console.log('nodes-export E2E SKIP (no exportable node in dataset)');
    await browser.close();
    return;
  }

  const expectedCount = parseCount(label);
  if (expectedCount === null) throw new Error('enabled button must show the contact count, got "' + label + '"');

  const [download] = await Promise.all([
    page.waitForEvent('download', { timeout: 15000 }),
    btn.click(),
  ]);

  const name = download.suggestedFilename();
  if (!/^corescope_nodes_[A-Za-z0-9_-]+_\d{4}-\d{2}-\d{2}-\d{6}\.json$/.test(name)) {
    throw new Error('unexpected download filename: ' + name);
  }

  const payload = await readDownloadedJson(download);

  if (!Array.isArray(payload.contacts)) throw new Error('export must have a contacts array');
  if (payload.contacts.length !== expectedCount) {
    throw new Error('button said ' + expectedCount + ' contacts, file has ' + payload.contacts.length);
  }
  if (Object.keys(payload).length !== 1) {
    throw new Error('export must contain only the contacts key, got ' + Object.keys(payload).join(','));
  }

  payload.contacts.forEach(function (c, i) {
    const keys = Object.keys(c).join(',');
    if (keys !== REF_KEYS.join(',')) {
      throw new Error('contact ' + i + ' key mismatch: ' + keys);
    }
    if (typeof c.public_key !== 'string' || c.public_key.length !== 64 || !/^[0-9a-fA-F]{64}$/.test(c.public_key)) {
      throw new Error('contact ' + i + ' public_key must be exactly 64 hex chars, got: ' + c.public_key);
    }
    if (typeof c.latitude !== 'string' || typeof c.longitude !== 'string') {
      throw new Error('contact ' + i + ' coordinates must be strings');
    }
    if (c.latitude === '0' && c.longitude === '0') {
      throw new Error('contact ' + i + ' is at null island and should have been skipped');
    }
    if (typeof c.last_advert !== 'number' || typeof c.type !== 'number') {
      throw new Error('contact ' + i + ' last_advert/type must be numbers');
    }
    if (!Number.isInteger(c.type) || c.type < 0 || c.type > 15) {
      throw new Error('contact ' + i + ' type must be an integer in [0,15], got: ' + c.type);
    }
  });

  // WYSIWYG (#1889 review fix 1): the export's row order must match the
  // currently rendered table order exactly, not just contain the same set.
  const domOrder = await page.$$eval('#nodesBody tr[data-key]', rows => rows.map(r => r.getAttribute('data-key')));
  const exportOrder = payload.contacts.map(c => c.public_key.toLowerCase());
  const domOrderLower = domOrder.map(k => (k || '').toLowerCase());
  // The table can render more rows than the export contains (rows the
  // export skips for having no valid position/role/pubkey still show in
  // the table) -- so assert the export order is a subsequence of the DOM
  // order, not a strict equal-length match.
  let di = 0;
  for (const pk of exportOrder) {
    while (di < domOrderLower.length && domOrderLower[di] !== pk) di++;
    if (di >= domOrderLower.length) {
      throw new Error('export contact ' + pk + ' does not appear in the DOM row order at or after the previous exported contact -- WYSIWYG order violated');
    }
    di++;
  }

  // Search by a unique public-key prefix (see file header) instead of a
  // human-chosen name. Deterministic for any dataset size, including
  // exactly one exportable contact: searching for that one contact's own
  // prefix must still narrow to exactly 1, not "fewer than 1".
  const target = payload.contacts[0];
  const prefix = target.public_key.slice(0, 24);

  await page.fill('#nodeSearch', prefix);
  await page.waitForFunction(function (want) {
    var b = document.getElementById('nodesExportBtn');
    return b && b.textContent.trim() !== want;
  }, label, { timeout: 15000 });

  const narrowed = (await page.textContent('#nodesExportBtn')).trim();
  const narrowedCount = parseCount(narrowed);
  if (narrowedCount !== 1) {
    throw new Error('searching by a 24-char pubkey prefix unique to one contact must narrow the export to exactly 1, got "' + narrowed + '"');
  }

  const [narrowedDownload] = await Promise.all([
    page.waitForEvent('download', { timeout: 15000 }),
    page.click('#nodesExportBtn'),
  ]);
  const narrowedPayload = await readDownloadedJson(narrowedDownload);
  if (narrowedPayload.contacts.length !== 1) {
    throw new Error('narrowed export file must contain exactly 1 contact, got ' + narrowedPayload.contacts.length);
  }
  if (narrowedPayload.contacts[0].public_key.toLowerCase() !== target.public_key.toLowerCase()) {
    throw new Error('narrowed export contains the wrong contact');
  }

  console.log('nodes-export E2E OK (' + expectedCount + ' contacts, ' + name + '; prefix-narrow → exactly 1, matched)');
  await browser.close();
})();
