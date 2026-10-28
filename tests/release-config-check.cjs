const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const root = path.join(__dirname, '..');
for (const name of ['ci', 'release', 'publish']) {
  const workflow = fs.readFileSync(path.join(root, '.github/workflows', name + '.yml'), 'utf8');
  const gate = workflow.split(/^  (?:image|publish):/m)[0];
  assert.match(gate, /actions\/setup-node@/);
  assert.match(gate, /node-version: '24\.22\.1'/);
  assert.match(gate, /working-directory: plugins\/openclaw\/openclaw-plugin\s+run: \|\s+npm ci\s+npm run build\s+npm test/);
}
const compose = fs.readFileSync(path.join(root, 'docker-compose.yml'), 'utf8');
assert.doesNotMatch(compose, /^\s*-\s*["']?(?:0\.0\.0\.0:)?25:2525/m);
assert.match(compose, /# - "0\.0\.0\.0:25:2525"/);
assert.match(compose, /Uncomment to receive public SMTP directly/);
assert.match(compose, /Upgrading an existing Included MX installation\? Retain this mapping/);
// Creating the GitHub Release must be idempotent: re-running the same tag, or
// a release created by hand in the GitHub UI, must not fail the job.
const release = fs.readFileSync(path.join(root, '.github/workflows/release.yml'), 'utf8');
assert.match(release, /gh release view "\$GITHUB_REF_NAME"/);
assert.match(release, /gh release create "\$GITHUB_REF_NAME"/);
// A genuine GitHub Release creation failure must fail the workflow,
// not be hidden by continue-on-error.
assert.doesNotMatch(release, /name: Create GitHub release if needed[\s\S]*?continue-on-error: true/);
console.log('Release gates and opt-in SMTP mapping: PASS');
