// Dependency-free checks for the inbox connector editor. Run from the repo root.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const { webcrypto } = require('node:crypto');
const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');
const css = fs.readFileSync('internal/httpapp/ui.go', 'utf8');
function section(start, end) {
  const a = source.indexOf(start);
  const b = source.indexOf(end, a + start.length);
  assert.ok(a >= 0 && b > a, start);
  return source.slice(a, b);
}
function element() {
  return {
    handlers: {}, attributes: {}, value: '',
    addEventListener(event, handler) { this.handlers[event] = handler; },
    setAttribute(key, value) { this.attributes[key] = value; },
    focus() { this.focused = true; }
  };
}
const save = element();
const input = element();
const auth = element();
const generate = element();
const editForm = element();
const bearer = element();
bearer.querySelector = () => input;
const editor = {
  innerHTML: '', textContent: '',
  querySelector(selector) {
    if (selector === '#inbox-connector-edit-form') return editForm;
    if (!this.innerHTML.includes('data-inline-webhook-auth')) return null;
    return {
      '[data-inline-webhook-auth]': auth,
      '[data-inline-bearer]': bearer,
      '[data-inline-generate-bearer]': generate,
      '[name=bearer_secret]': input
    }[selector] || null;
  }
};
const active = { classList: { remove() { this.removed = true; } } };
const dlg = {
  classList: { remove() {} },
  querySelector(selector) {
    return { '#inbox-edit-save': save, '#inbox-connector-editor': editor,
      '.inbox-subview.active[data-inbox-subview=connectors]': this.editing ? active : null }[selector];
  },
  querySelectorAll() { return [active]; },
  close() { this.closed = true; }
};
const alerts = [];
const sandbox = {
  Uint8Array, window: { crypto: webcrypto, btoa: s => Buffer.from(s, 'binary').toString('base64'), alert: s => alerts.push(s) },
  connectorEditor: editor, inboxSaveButton: save, dlg,
  editInboxID: 'in_test', csrfValue: 'csrf', escapeConnector: s => s || '',
  showInboxSubview() { dlg.editing = true; },
  document: { getElementById: () => cancel }
};
const cancel = element();
vm.createContext(sandbox);
vm.runInContext(section('function generateBearerSecret()', '\n(function'), sandbox);
vm.runInContext(section('  function setConnectorEditorMode(', '  function renderConnectors('), sandbox);
vm.runInContext(section('function hideInboxSubview(', 'function generateBearerSecret('), sandbox);
vm.runInContext(section("  var cancel = document.getElementById('inbox-edit-cancel');", '  // Reopen the inbox edit dialog'), sandbox);

for (const kind of ['hermes', 'openclaw', 'webhook']) {
  auth.value = 'bearer';
  sandbox.renderConnectorEditor({ Kind: kind, ID: 'test', Name: 'Test', Role: 'owner', AuthMode: 'bearer', Enabled: true });
  assert.equal(save.attributes.form, 'inbox-connector-edit-form');
  assert.ok(editor.innerHTML.includes(`/ui/${kind === 'webhook' ? 'webhooks' : kind}/test/edit`));
  assert.doesNotMatch(editor.innerHTML, /Delivery log|Remove connector|open-delete-client/);
}
assert.equal(input.disabled, false);
assert.equal(bearer.hidden, false);
generate.handlers.click();
assert.match(input.value, /^[A-Za-z0-9_-]{43}$/);
assert.equal(Buffer.from(input.value, 'base64url').length, 32);
assert.equal(input.type, 'text');
assert.equal(input.focused, true);
assert.equal(sandbox.inboxSubmitting, undefined, 'Generate does not submit');
const first = input.value;
generate.handlers.click();
assert.notEqual(input.value, first);
auth.value = 'signature';
auth.handlers.change();
assert.equal(input.disabled, true);
assert.equal(bearer.hidden, true);
sandbox.renderConnectorEditor({ Kind: 'webhook', ID: 'test', AuthMode: 'signature', Enabled: false });
assert.match(editor.innerHTML, /Rotate signing secret/);
assert.match(editor.innerHTML, /Enable webhook/);
assert.equal(input.disabled, true, 'initial signature mode disables bearer input');
editForm.handlers.submit();
assert.equal(sandbox.inboxSubmitting, true);
sandbox.window.crypto = { getRandomValues() { throw new Error('unavailable'); } };
generate.handlers.click();
assert.equal(alerts.length, 1, 'generation failure is surfaced');
cancel.handlers.click();
assert.equal(dlg.closed, undefined, 'connector Cancel returns to the list');
assert.equal(editor.textContent, '');
assert.equal(save.attributes.form, 'inbox-edit-form', 'Back/Cancel restores inbox Save target');
dlg.editing = false;
cancel.handlers.click();
assert.equal(dlg.closed, true, 'ordinary inbox Cancel still closes');
assert.match(css, /:has\(\.inbox-subview.active\[data-inbox-subview=connectors\]\) \.inbox-settings-footer \[type=submit\]\{display:inline-block\}/);
assert.match(css, /:has\(\.inbox-subview.active\[data-inbox-subview=connectors\]\) #inbox-edit-delete\{display:none\}/);
assert.match(css, /\.connector-editor \[data-inline-bearer\] \.row\{align-items:center/, 'bearer row centres its Generate button');
assert.match(css, /\.connector-editor \[data-inline-bearer\] \.row input\{margin:0\}/, 'bearer input margins move to the row');
console.log('connector editor checks passed');
