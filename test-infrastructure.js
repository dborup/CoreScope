const assert = require('assert');
const fs = require('fs');
const vm = require('vm');

const context = {window:{}, console};
vm.createContext(context);
vm.runInContext(fs.readFileSync('public/infrastructure.js', 'utf8'), context);
const rank = context.window.InfrastructurePage.rankCandidates;
const key = n => n.repeat(64);
const rows = [
  {public_key:key('a'), role:'repeater', relay_count_24h:12, bridge_score:0.1},
  {public_key:key('b'), role:'repeater', relay_count_24h:50, bridge_score:0.2},
  {public_key:key('c'), role:'room', relay_count_24h:500, bridge_score:0.9},
  {public_key:key('d'), role:'repeater', relay_count_24h:0, bridge_score:0.9}
];
assert.deepStrictEqual(Array.from(rank(rows, new Set([key('b')])).map(n => n.public_key)), [key('a')]);
assert.strictEqual(rank(rows, new Set()).length, 2);
console.log('infrastructure candidate ranking: ok');
