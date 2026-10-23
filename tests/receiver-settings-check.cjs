// Run: node tests/receiver-settings-check.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');
const start = source.indexOf('  function sync(dlg) {');
const end = source.indexOf("  document.querySelectorAll('.domain-dialog')", start);
assert.ok(start >= 0 && end > start);
const panel = () => ({
  hidden: false,
  fields: [{ disabled: false }, { disabled: false }],
  querySelectorAll() { return this.fields; }
});
const direct = [panel(), panel()];
const remote = panel();
const selector = { value: 'remotemx' };
const dlg = {
  classList: { toggle() {} },
  querySelector(s) {
    if (s === '.provider-select') return selector;
    if (s === '[data-remote-mx-panel]') return remote;
    return null;
  },
  querySelectorAll(s) { return s === '[data-mx-panel]' ? direct : []; }
};
const context = vm.createContext({ dlg });
vm.runInContext(source.slice(start, end), context);
for (const provider of ['remotemx', 'mx', 'mailgun', 'remotemx']) {
  selector.value = provider;
  vm.runInContext('sync(dlg)', context);
  for (const p of direct) {
    assert.equal(p.hidden, provider !== 'mx');
    assert.ok(p.fields.every(f => f.disabled === (provider !== 'mx')));
  }
  assert.equal(remote.hidden, provider !== 'remotemx');
  assert.ok(remote.fields.every(f => f.disabled === (provider !== 'remotemx')));
}
console.log('receiver settings provider switching passed');
