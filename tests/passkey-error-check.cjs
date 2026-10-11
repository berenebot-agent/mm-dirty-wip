// Focused browser-glue checks with mocked WebAuthn and fetch; no dependencies.
// Run: node tests/passkey-error-check.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');
// Take the passkey module alone. Slicing to the end of the file would drag in
// every module that follows it (the status flag, the navigation reporter) and
// their DOM needs, which is not what this check is about.
const markerAt = source.indexOf('/* Passkey (WebAuthn) ceremonies.');
const start = source.indexOf('(function () {', markerAt);
const end = source.indexOf('\n})();', start);
assert.ok(markerAt >= 0 && start > markerAt && end > start, 'passkey module not found');
const moduleSource = source.slice(start, end + '\n})();'.length);
// Pin the boundary: the slice must be this module alone, not the modules that
// follow it in the bundle.
assert.ok(
  moduleSource.includes('passkey-signin') && !moduleSource.includes('Global Live/Loading status flag'),
  'passkey module slice covers the wrong range'
);

async function check(register, error, expected) {
  let click;
  const status = { textContent: '', classList: { toggle() {} } };
  const button = {
    addEventListener(event, fn) { click = fn; },
    getAttribute(name) { return name === 'data-password-enabled' ? '0' : '/mock'; }
  };
  const context = {
    Uint8Array, atob,
    document: {
      querySelector() { return { value: 'csrf' }; },
      // The module also wires the per-passkey settings gears, the dialog close
      // buttons and the feedback/settings modals it finds on the page. This
      // check drives only the sign-in and add-passkey error paths, so an empty
      // list lets that extra wiring no-op instead of throwing at load.
      querySelectorAll() { return []; },
      getElementById(id) {
        if (id === 'passkey-status') return status;
        return id === (register ? 'passkey-add' : 'passkey-signin') ? button : null;
      }
    },
    window: {
      PublicKeyCredential: {}, isSecureContext: true,
      location: { hostname: 'ui.example.net' }, prompt() { return 'Test'; }, confirm() { return false; }
    },
    navigator: { credentials: {
      create() { return Promise.reject(error); }, get() { return Promise.reject(error); }
    } },
    fetch() { return Promise.resolve({ ok: true, json() { return Promise.resolve({
      options: { publicKey: {
        challenge: 'YQ', user: { id: 'YQ' }, rp: { id: 'receive.example.com' }, rpId: 'receive.example.com'
      } }
    }); } }); }
  };
  vm.runInNewContext(moduleSource, context);
  click();
  await new Promise(resolve => setImmediate(resolve));
  assert.match(status.textContent, expected);
  assert.equal(button.disabled, false);
}

(async () => {
  for (const register of [true, false]) {
    await check(register, { name: 'SecurityError', message: 'The relying party ID is not a registrable domain suffix' },
      /configured for "receive\.example\.com".*visiting "ui\.example\.net".*BASE_URL.*DEDICATED_RECEIVER_URL/);
    await check(register, { name: 'NotAllowedError', message: 'The operation was cancelled.' }, /^The operation was cancelled\.$/);
    await check(register, { name: 'SecurityError', message: 'An unrelated security error' }, /^An unrelated security error$/);
  }
  console.log('Passkey registration/sign-in error checks passed');
})().catch(err => { console.error(err); process.exitCode = 1; });
