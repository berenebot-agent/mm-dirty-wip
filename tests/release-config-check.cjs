const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const root = path.join(__dirname, '..');
for (const name of ['ci', 'release', 'publish']) {
  const workflow = fs.readFileSync(path.join(root, '.github/workflows', name + '.yml'), 'utf8');
  const gate = workflow.split(/^  (?:image|publish):/m)[0];
  assert.match(gate, /actions\/setup-node@/);
  assert.match(gate, /node-version: 24/);
  assert.match(gate, /working-directory: plugins\/openclaw\/openclaw-plugin\s+run: \|\s+npm ci\s+npm run build\s+npm test/);
}
const compose = fs.readFileSync(path.join(root, 'docker-compose.yml'), 'utf8');
assert.doesNotMatch(compose, /^\s*-\s*["']?(?:0\.0\.0\.0:)?25:2525/m);
assert.match(compose, /# - "0\.0\.0\.0:25:2525"/);
assert.match(compose, /Uncomment for Included MX/);
assert.match(compose, /Upgrading an existing Included MX installation\? Retain this mapping/);
console.log('Release gates and opt-in SMTP mapping: PASS');
