// Focused browser-glue checks with mocked WebAuthn and fetch; no dependencies.
// Run: node tests/passkey-error-check.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/httpapp/assets/app.js', 'utf8');
const moduleSource = source.slice(source.indexOf('/* Passkey (WebAuthn) ceremonies.'));

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
