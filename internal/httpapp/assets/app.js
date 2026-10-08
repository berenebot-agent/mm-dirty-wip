(function () {
  var dlg = document.getElementById('key-dialog');
  if (!dlg) {
    return;
  }
  var dialogStyle = document.createElement('style');
  dialogStyle.textContent = '#key-dialog[open]{display:flex;flex-direction:column;max-height:calc(100dvh - 32px);overflow:hidden}#key-dialog h2{flex:0 0 auto}#key-form:not([hidden]){display:flex;flex:1 1 auto;flex-direction:column;min-height:0;overflow:hidden}#key-form[hidden]{display:none}.key-form-scroll{flex:1 1 auto;min-height:0;overflow-y:auto;overscroll-behavior:contain}.key-fields{display:block;min-width:0;overflow:visible}.connector-config-row{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px}.connector-config-row>div{min-width:0}.connector-config-row label{display:block}.connector-config-row select{margin-bottom:6px}@media(max-width:520px){.connector-config-row{grid-template-columns:1fr}}#key-dialog #key-error{flex:0 0 auto}#key-dialog #key-form>.dialog-actions{position:static;flex:0 0 auto;padding:12px 0;margin-top:4px;background:#fff;border-top:1px solid #eee}';
  document.head.appendChild(dialogStyle);
  var sel = document.getElementById('key-type');
  var form = document.getElementById('key-form');
  var formScroll = document.createElement('div');
  formScroll.className = 'key-form-scroll';
  var formError = document.getElementById('key-error');
  while (form.firstChild && form.firstChild !== formError) {
    formScroll.appendChild(form.firstChild);
  }
  form.insertBefore(formScroll, formError);
  var title = document.getElementById('key-dialog-title');
  var createSource = 'client';
  var fixedInboxID = '';
  var returnInboxID = '';
  var idInput = form.querySelector('[name=id]');
  var nameInput = form.querySelector('[name=name]');
  var adminInput = form.querySelector('[name=admin]');
  var result = document.getElementById('key-result');
  var resultTitle = document.getElementById('key-result-title');
  var resultLabel = document.getElementById('key-result-label');
  var resultSecret = document.getElementById('key-result-secret');
  var copyBtn = document.getElementById('key-copy');
  var copyNote = document.getElementById('key-copy-note');
  var errorBox = document.getElementById('key-error');
  var doneBtn = document.getElementById('key-done');
  var matrix = document.getElementById('key-matrix');
  var submitBtn = document.getElementById('key-submit');
  var rotateBtn = document.getElementById('key-rotate');
  var hermesInbox = form.querySelector('.key-fields[data-type=hermes] select[name=inbox]');
  var hermesWarning = document.getElementById('key-hermes-warning');
  var hermesAckRow = document.getElementById('key-hermes-ack-row');
  var hermesAck = document.getElementById('key-hermes-ack');
  var openclawInbox = form.querySelector('.key-fields[data-type=openclaw] select[name=inbox]');
  var openclawWarning = document.getElementById('key-openclaw-warning');
  var openclawAckRow = document.getElementById('key-openclaw-ack-row');
  var openclawAck = document.getElementById('key-openclaw-ack');
  var adminSnapshot = null;
  var currentKind = 'api';
  var webhookFields = form.querySelector('.key-fields[data-type=webhook]');
  var webhookAuth = webhookFields.querySelector('select[name=auth]');
  var bearerFields = document.createElement('div');
  bearerFields.hidden = true;
  bearerFields.innerHTML = '<label for="webhook-bearer-secret">Bearer secret</label><div class="row"><input id="webhook-bearer-secret" name="bearer_secret" type="password" autocomplete="new-password" placeholder="Paste your secret here, or click Generate for a random one" aria-describedby="webhook-bearer-hint"><button type="button" class="secondary btn-narrow" id="webhook-generate">Generate</button></div><p class="muted small" id="webhook-bearer-hint"></p>';
  webhookFields.insertBefore(bearerFields, webhookAuth.nextElementSibling);
  var payloadSelect = webhookFields.querySelector('select[name=mode]');
  var configRow = document.createElement('div');
  configRow.className = 'connector-config-row';
  [payloadSelect, webhookAuth].forEach(function (select) {
    var field = document.createElement('div');
    field.appendChild(select.previousElementSibling);
    field.appendChild(select);
    configRow.appendChild(field);
  });
  webhookFields.insertBefore(configRow, bearerFields);
  form.querySelectorAll('.connector-auto-actions').forEach(function (section) {
    section.remove();
  });
  var bearerInput = bearerFields.querySelector('input');
  var generateBtn = bearerFields.querySelector('button');

  // Relay connectors share the same outbound relay transport and the same
  // no-allow-list risk gate: Hermes and OpenClaw.
  function isRelayKind(kind) {
    return kind === 'hermes' || kind === 'openclaw';
  }

  function syncBearer() {
    var active = sel.value === 'webhook' && webhookAuth.value === 'bearer';
    var editing = form.getAttribute('action') !== '/ui/keys';
    bearerFields.hidden = !active;
    bearerInput.disabled = !active;
    bearerInput.required = active && !editing;
    document.getElementById('webhook-bearer-hint').textContent = editing
      ? 'Leave blank to keep the current secret. Paste only the token, without Bearer, or Generate a replacement. It changes when you save.'
      : 'Paste only the token, without Bearer, or click Generate. Copy it before saving if another app needs it.';
    if (rotateBtn) {
      var canRotate = editing && !!idInput.value && (sel.value === 'api' || (sel.value === 'webhook' && !active));
      rotateBtn.hidden = !canRotate;
      rotateBtn.style.display = canRotate ? '' : 'none';
      rotateBtn.textContent = sel.value === 'webhook' ? 'Rotate signing secret' : 'Rotate Key';
    }
  }

  webhookAuth.addEventListener('change', syncBearer);
  generateBtn.addEventListener('click', function () {
    errorBox.hidden = true;
    try {
      var bytes = new Uint8Array(32);
      window.crypto.getRandomValues(bytes);
      var raw = '';
      bytes.forEach(function (b) { raw += String.fromCharCode(b); });
      bearerInput.value = window.btoa(raw).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
      bearerInput.type = 'text';
      bearerInput.focus();
    } catch (err) {
      errorBox.textContent = 'Could not generate a secret. Paste a secret from your other app.';
      errorBox.hidden = false;
    }
  });

  function sync() {
    document.querySelectorAll('.key-fields').forEach(function (fs) {
      var active = fs.getAttribute('data-type') === sel.value;
      fs.style.display = active ? '' : 'none';
      fs.querySelectorAll('input,select').forEach(function (el) {
        el.disabled = !active;
      });
    });
    dlg.classList.add('key-dialog--wide');
    var connectorCreate = form.getAttribute('action') === '/ui/keys' && sel.value !== 'api';
    if (title) {
      title.textContent = connectorCreate ? 'Add connector' : (form.getAttribute('action') === '/ui/keys' ? 'Add client' : (sel.value === 'api' ? 'Edit client' : 'Edit connector'));
    }
    if (submitBtn && form.getAttribute('action') === '/ui/keys') {
      submitBtn.textContent = connectorCreate ? 'Add Connector' : 'Add Client';
    }
    form.querySelectorAll('.connector-inbox-select').forEach(function (inboxSel) {
      var label = inboxSel.previousElementSibling;
      var hide = connectorCreate && createSource === 'connector' && !!fixedInboxID;
      inboxSel.hidden = hide;
      if (label && label.classList.contains('connector-inbox-label')) {
        label.hidden = hide;
      }
      if (fixedInboxID && connectorCreate) {
        inboxSel.value = fixedInboxID;
      }
    });
    syncHermesRisk();
    syncBearer();
  }

  function rolesRadios() {
    return matrix ? matrix.querySelectorAll('input[type=radio][name^=role_]') : [];
  }

  function setAllRoles(value) {
    rolesRadios().forEach(function (el) {
      el.checked = el.value === value;
    });
  }

  function setDomainRoles(domainID, value) {
    if (!matrix) {
      return;
    }
    var group = matrix.querySelector('tbody[data-domain="' + domainID + '"]');
    if (!group) {
      return;
    }
    group.querySelectorAll('input[type=radio][name^=role_]').forEach(function (el) {
      el.checked = el.value === value;
    });
  }

  function scopeRadios(scope) {
    if (!matrix) {
      return [];
    }
    if (scope === 'all') {
      return rolesRadios();
    }
    var group = matrix.querySelector('tbody[data-domain="' + scope + '"]');
    return group ? group.querySelectorAll('input[type=radio][name^=role_]') : [];
  }

  // The de-facto role is the single checked value shared by every inbox in the
  // scope, or undefined when the scope is mixed or has no inboxes.
  function uniformRole(radios) {
    var value;
    var seen = false;
    for (var i = 0; i < radios.length; i++) {
      if (!radios[i].checked) {
        continue;
      }
      if (!seen) {
        value = radios[i].value;
        seen = true;
      } else if (value !== radios[i].value) {
        return undefined;
      }
    }
    return seen ? value : undefined;
  }

  function syncScopeButtons() {
    if (!matrix) {
      return;
    }
    document.querySelectorAll('#key-matrix .seg[data-set-scope]').forEach(function (seg) {
      var role = uniformRole(scopeRadios(seg.getAttribute('data-set-scope')));
      seg.querySelectorAll('button[data-set-role]').forEach(function (btn) {
        var on = role !== undefined && btn.getAttribute('data-set-role') === role;
        btn.classList.toggle('active', on);
        btn.setAttribute('aria-pressed', on ? 'true' : 'false');
      });
    });
  }

  function syncAdmin() {
    if (!matrix) {
      return;
    }
    var on = !!(adminInput && adminInput.checked);
    if (on) {
      if (adminSnapshot === null) {
        adminSnapshot = [];
        rolesRadios().forEach(function (el) {
          if (el.checked) {
            adminSnapshot.push({ name: el.name, value: el.value });
          }
        });
      }
      setAllRoles('owner');
    } else if (adminSnapshot !== null) {
      rolesRadios().forEach(function (el) {
        el.checked = false;
      });
      adminSnapshot.forEach(function (s) {
        var el = form.querySelector('input[type=radio][name="' + s.name + '"][value="' + s.value + '"]');
        if (el) {
          el.checked = true;
        }
      });
      adminSnapshot = null;
    }
    matrix.disabled = on;
    syncScopeButtons();
  }

  // Gate creating a relay connection (Hermes or OpenClaw) for an inbox with no
  // allowed senders: show the red warning and keep Create disabled until the
  // operator acknowledges that the agent will reply to anyone. Skipped when
  // editing an existing connection (the inbox is fixed and already accepted).
  function syncHermesRisk() {
    var editing = form.getAttribute('action') !== '/ui/keys';
    if (sel.value === 'hermes' || sel.value === 'openclaw') {
      var openclaw = sel.value === 'openclaw';
      var inboxSel = openclaw ? openclawInbox : hermesInbox;
      var warning = openclaw ? openclawWarning : hermesWarning;
      var ackRow = openclaw ? openclawAckRow : hermesAckRow;
      var ack = openclaw ? openclawAck : hermesAck;
      var opt = inboxSel ? inboxSel.options[inboxSel.selectedIndex] : null;
      var warn = !editing && (!opt || opt.getAttribute('data-allowlist') !== '1');
      if (warning) {
        warning.hidden = !warn;
      }
      // The row carries an inline display:flex, which wins over [hidden];
      // toggle display directly so it actually disappears when a list is set.
      if (ackRow) {
        ackRow.style.display = warn ? 'flex' : 'none';
      }
      if (!warn) {
        if (ack) {
          ack.checked = false;
        }
        if (submitBtn) {
          submitBtn.disabled = false;
        }
        return;
      }
      if (ack && submitBtn) {
        submitBtn.disabled = !ack.checked;
      }
    } else if (hermesWarning && hermesAckRow && hermesAck && submitBtn) {
      hermesWarning.hidden = true;
      hermesAckRow.style.display = 'none';
      if (openclawWarning && openclawAckRow && openclawAck) {
        openclawWarning.hidden = true;
        openclawAckRow.style.display = 'none';
      }
      submitBtn.disabled = false;
    }
  }

  function showForm() {
    form.hidden = false;
    result.hidden = true;
    errorBox.hidden = true;
    errorBox.textContent = '';
    resultSecret.textContent = '';
    if (copyBtn) {
      copyBtn.textContent = 'Copy';
    }
    if (copyNote) {
      copyNote.hidden = true;
    }
  }

  function showResult(data) {
    dlg.classList.add('key-dialog--wide');
    form.hidden = true;
    errorBox.hidden = true;
    resultTitle.textContent = data.notice || 'Key created';
    resultLabel.textContent = data.label || '';
    resultSecret.textContent = data.secret || '';
    if (copyBtn) {
      copyBtn.textContent = 'Copy';
    }
    if (copyNote) {
      copyNote.hidden = true;
    }
    result.hidden = false;
  }

  function selectSecret() {
    if (!window.getSelection || !document.createRange) {
      return;
    }
    var range = document.createRange();
    range.selectNodeContents(resultSecret);
    var selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
  }

  function setAPIOptionVisible(visible) {
    var opt = sel.querySelector('option[value=api]');
    if (opt) {
      opt.hidden = !visible;
      opt.disabled = !visible;
    }
  }

  function openCreate() {
    form.reset();
    bearerInput.type = 'password';
    form.action = '/ui/keys';
    idInput.value = '';
    adminSnapshot = null;
    currentKind = 'api';
    createSource = 'client';
    fixedInboxID = '';
    returnInboxID = '';
    setAPIOptionVisible(true);
    sel.value = 'api';
    sel.disabled = false;
    if (submitBtn) {
      submitBtn.textContent = 'Add Client';
    }
    if (rotateBtn) {
      rotateBtn.hidden = true;
      rotateBtn.textContent = 'Rotate Key';
    }
    showForm();
    sync();
    syncAdmin();
    dlg.showModal();
  }

  function openCreateConnector(inboxID) {
    form.reset();
    bearerInput.type = 'password';
    form.action = '/ui/keys';
    idInput.value = '';
    adminSnapshot = null;
    currentKind = 'hermes';
    createSource = 'connector';
    fixedInboxID = inboxID || '';
    returnInboxID = fixedInboxID;
    setAPIOptionVisible(false);
    sel.value = 'hermes';
    sel.disabled = false;
    if (rotateBtn) {
      rotateBtn.hidden = true;
      rotateBtn.textContent = 'Rotate Key';
    }
    showForm();
    sync();
    syncAdmin();
    dlg.showModal();
  }

  function openEdit(btn) {
    form.reset();
    bearerInput.type = 'password';
    adminSnapshot = null;
    createSource = 'edit';
    fixedInboxID = '';
    returnInboxID = '';
    setAPIOptionVisible(true);
    var kind = isRelayKind(btn.dataset.kind) || btn.dataset.kind === 'webhook' ? btn.dataset.kind : 'api';
    currentKind = kind;
    var segment = isRelayKind(kind) ? kind : (kind === 'webhook' ? 'webhooks' : 'keys');
    form.action = '/ui/' + segment + '/' + btn.dataset.id + '/edit';
    idInput.value = btn.dataset.id;
    sel.value = kind;
    sel.disabled = true;
    if (submitBtn) {
      submitBtn.textContent = 'Save';
    }
    if (rotateBtn) {
      rotateBtn.hidden = isRelayKind(kind);
      rotateBtn.textContent = 'Rotate Key';
    }
    nameInput.value = btn.dataset.name || '';
    if (adminInput) {
      adminInput.checked = btn.dataset.admin === '1';
    }
    if (kind === 'api') {
      var roles = {};
      try {
        roles = JSON.parse(btn.dataset.roles || '{}');
      } catch (e) {
        roles = {};
      }
      rolesRadios().forEach(function (el) {
        el.checked = el.value === (roles[el.name.slice(5)] || '');
      });
    } else if (kind === 'webhook') {
      var wf = form.querySelector('.key-fields[data-type=webhook]');
      var wInbox = wf.querySelector('select[name=inbox]');
      if (wInbox && btn.dataset.inbox) {
        wInbox.value = btn.dataset.inbox;
      }
      wf.querySelector('input[name=url]').value = btn.dataset.url || '';
      wf.querySelector('select[name=mode]').value = btn.dataset.mode || 'notify';
      wf.querySelector('select[name=auth]').value = btn.dataset.auth || 'signature';
    } else {
      var inbox = form.querySelector('.key-fields[data-type=' + kind + '] select[name=inbox]');
      if (inbox && btn.dataset.inbox) {
        inbox.value = btn.dataset.inbox;
      }
      var roleSel = form.querySelector('.key-fields[data-type=' + kind + '] select[name=role]');
      if (roleSel) {
        roleSel.value = btn.dataset.role || 'owner';
      }
    }
    sync();
    syncAdmin();
    if (isRelayKind(kind) || kind === 'webhook') {
      var inbox2 = form.querySelector('.key-fields[data-type=' + kind + '] select[name=inbox]');
      if (inbox2) {
        inbox2.disabled = true;
      }
      var setupSel = form.querySelector('.key-fields[data-type=' + kind + '] select[name=setup]');
      if (setupSel) {
        setupSel.disabled = true;
      }
    }
    showForm();
    dlg.showModal();
  }

  form.addEventListener('submit', function (e) {
    if (form.getAttribute('action') !== '/ui/keys' || typeof window.fetch !== 'function') {
      return;
    }
    e.preventDefault();
    errorBox.hidden = true;
    errorBox.textContent = '';
    var body = new URLSearchParams();
    new FormData(form).forEach(function (value, key) {
      body.append(key, value);
    });
    fetch(form.action, {
      method: 'POST',
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8'
      },
      body: body.toString(),
      credentials: 'same-origin'
    }).then(function (res) {
      if (!res.ok) {
        return res.text().then(function (text) {
          throw new Error(text || 'Could not create client');
        });
      }
      return res.json();
    }).then(function (data) {
      if (sel.value !== 'api') {
        var activeInbox = form.querySelector('.key-fields[data-type=' + sel.value + '] select[name=inbox]');
        returnInboxID = activeInbox ? activeInbox.value : returnInboxID;
      }
      if (sel.value === 'webhook' && webhookAuth.value === 'bearer') {
        dlg.close();
        if (returnInboxID) {
          window.location.href = '/?inbox=' + encodeURIComponent(returnInboxID) + '&inbox_tab=connectors';
        } else {
          window.location.reload();
        }
        return;
      }
      showResult(data);
    }).catch(function (err) {
      errorBox.textContent = err.message || 'Could not create client';
      errorBox.hidden = false;
    });
  });

  if (copyBtn) {
    copyBtn.addEventListener('click', function () {
      var text = resultSecret.textContent;
      if (window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () {
          copyBtn.textContent = 'Copied!';
          setTimeout(function () {
            copyBtn.textContent = 'Copy';
          }, 1500);
        }).catch(function () {
          if (copyNote) {
            copyNote.hidden = false;
          }
          selectSecret();
        });
        return;
      }
      if (copyNote) {
        copyNote.hidden = false;
      }
      selectSecret();
    });
  }

  if (doneBtn) {
    doneBtn.addEventListener('click', function () {
      dlg.close();
      if (returnInboxID) {
        window.location.href = '/?inbox=' + encodeURIComponent(returnInboxID) + '&inbox_tab=connectors';
      } else {
        window.location.reload();
      }
    });
  }

  if (rotateBtn) {
    rotateBtn.addEventListener('click', function () {
      var id = idInput.value;
      if (!id) {
        return;
      }
      var webhook = currentKind === 'webhook';
      if (!window.confirm(webhook ? 'Rotate this webhook signing secret? The current secret stops working immediately.' : 'Rotate this API key? The current key stops working immediately.')) {
        return;
      }
      var csrfInput = form.querySelector('[name=_csrf]');
      var body = new URLSearchParams();
      if (csrfInput) {
        body.append('_csrf', csrfInput.value);
      }
      rotateBtn.disabled = true;
      var rotatePath = webhook ? '/ui/webhooks/' + encodeURIComponent(id) + '/rotate' : '/ui/keys/' + encodeURIComponent(id) + '/rotate';
      fetch(rotatePath, {
        method: 'POST',
        headers: {
          Accept: 'application/json',
          'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8'
        },
        body: body.toString(),
        credentials: 'same-origin'
      }).then(function (res) {
        if (!res.ok) {
          return res.text().then(function (text) {
            throw new Error(text || 'Could not rotate key');
          });
        }
        return res.json();
      }).then(function (data) {
        rotateBtn.disabled = false;
        showResult(data);
      }).catch(function (err) {
        rotateBtn.disabled = false;
        errorBox.textContent = err.message || 'Could not rotate key';
        errorBox.hidden = false;
      });
    });
  }

  sel.addEventListener('change', function () {
    sync();
    syncAdmin();
  });
  if (adminInput) {
    adminInput.addEventListener('change', syncAdmin);
  }
  if (hermesInbox) {
    hermesInbox.addEventListener('change', syncHermesRisk);
  }
  if (hermesAck) {
    hermesAck.addEventListener('change', syncHermesRisk);
  }
  document.querySelectorAll('#key-matrix [data-set-role]').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var domain = btn.getAttribute('data-domain');
      if (domain) {
        setDomainRoles(domain, btn.getAttribute('data-set-role'));
      } else {
        setAllRoles(btn.getAttribute('data-set-role'));
      }
      syncScopeButtons();
    });
  });
  if (matrix) {
    matrix.addEventListener('change', syncScopeButtons);
  }
  var add = document.getElementById('add-key');
  if (add) {
    add.addEventListener('click', openCreate);
  }
  document.querySelectorAll('.add-connector').forEach(function (btn) {
    btn.addEventListener('click', function () {
      openCreateConnector(btn.getAttribute('data-inbox') || '');
    });
  });
  var cancel = document.getElementById('key-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      dlg.close();
    });
  }
  document.querySelectorAll('.edit-credential').forEach(function (btn) {
    btn.addEventListener('click', function () {
      openEdit(btn);
    });
  });
  syncScopeButtons();
})();

(function () {
  document.querySelectorAll('form[data-confirm]').forEach(function (form) {
    form.addEventListener('submit', function (e) {
      if (!window.confirm(form.getAttribute('data-confirm'))) {
        e.preventDefault();
      }
    });
  });
})();

(function () {
  document.querySelectorAll('tr.row-link[data-href]').forEach(function (row) {
    row.addEventListener('click', function (e) {
      if (e.target.closest('a,button,input,select,textarea,label,form')) {
        return;
      }
      window.location.href = row.getAttribute('data-href');
    });
  });
})();

// initSenderEditor wires an allow-list editor shared by the add and edit
// inbox dialogs: a list of allowed addresses, an approver address that is
// always allowed, and a restrict toggle that reveals the list.
var initSenderEditor = (function () {
  return function (opts) {
    var list = opts.list;
    var input = opts.input;
    var note = opts.note;
    var approverEmail = opts.approverEmail;
    var restrict = opts.restrict;
    var requireAuth = opts.requireAuth;
    var section = opts.section;
    var mxSection = opts.mxSection;

    function approverValue() {
      return (approverEmail && approverEmail.value || '').trim().toLowerCase();
    }

    function isRestricted() {
      return !!(restrict && restrict.checked);
    }

    function refreshRestrictVisibility() {
      if (section) {
        section.hidden = !isRestricted();
      }
    }

    function refreshSenderNote() {
      if (!note || !list) {
        return;
      }
      var count = list.querySelectorAll('input[name=allowed]').length;
      if (count === 0 && !approverValue()) {
        note.textContent = 'This inbox currently blocks all senders.';
      } else if (count === 0) {
        note.textContent = 'Only the approver can email this inbox.';
      } else {
        note.textContent = 'Only these From addresses are accepted. The From header can be spoofed, so this is a filter, not proof of identity.';
      }
    }

    function refreshSenderEmpty() {
      if (!list) {
        return;
      }
      var empty = list.querySelector('.empty');
      var hasSenders = list.querySelectorAll('input[name=allowed]').length > 0 || !!approverValue() || !!list.querySelector('.locked');
      if (!hasSenders) {
        if (!empty) {
          var li = document.createElement('li');
          li.className = 'empty';
          li.textContent = 'Add an address below to allow it to email this inbox.';
          list.appendChild(li);
        }
      } else if (empty) {
        empty.remove();
      }
    }

    function addLockedApprover(email) {
      if (!list) {
        return;
      }
      email = (email || '').trim().toLowerCase();
      if (!email) {
        return;
      }
      var li = document.createElement('li');
      li.className = 'locked';
      var label = document.createElement('span');
      label.className = 'addr';
      label.textContent = email + ' (approver)';
      var locked = document.createElement('span');
      locked.className = 'muted small';
      locked.textContent = 'always allowed';
      li.appendChild(label);
      li.appendChild(locked);
      list.appendChild(li);
    }

    function addSender(value) {
      if (!list) {
        return;
      }
      value = (value || '').trim().toLowerCase();
      if (!value) {
        return;
      }
      var li = document.createElement('li');
      var hidden = document.createElement('input');
      hidden.type = 'hidden';
      hidden.name = 'allowed';
      hidden.value = value;
      var label = document.createElement('span');
      label.className = 'addr';
      label.textContent = value;
      var remove = document.createElement('button');
      remove.type = 'button';
      remove.className = 'secondary icon-btn';
      remove.title = 'Remove';
      remove.setAttribute('aria-label', 'Remove');
      remove.innerHTML = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M4 4l8 8M12 4l-8 8"/></svg>';
      remove.addEventListener('click', function () {
        li.remove();
        refreshSenderNote();
        refreshSenderEmpty();
      });
      li.appendChild(hidden);
      li.appendChild(label);
      li.appendChild(remove);
      list.appendChild(li);
      refreshSenderNote();
      refreshSenderEmpty();
    }

    function setSenders(raw, approver) {
      if (!list) {
        return;
      }
      list.innerHTML = '';
      addLockedApprover(approver);
      (raw || '').split(',').forEach(function (entry) {
        if ((entry || '').trim().toLowerCase() === (approver || '').trim().toLowerCase()) {
          return;
        }
        addSender(entry);
      });
      refreshSenderNote();
      refreshSenderEmpty();
    }

    function setApprover(value) {
      if (approverEmail) {
        approverEmail.value = value || '';
      }
    }

    function setRestricted(value) {
      if (restrict) {
        restrict.checked = !!value;
      }
      refreshRestrictVisibility();
    }

    function setRequireAuth(value) {
      if (requireAuth) {
        requireAuth.checked = !!value;
      }
    }

    // setMX reveals the authenticated-sender control only for domains that
    // receive mail by direct SMTP (MX); the flag has no effect on webhook
    // providers. Disabling it also unchecks the box so a hidden control cannot
    // submit a stale value.
    function setMX(value) {
      if (mxSection) {
        mxSection.hidden = !value;
      }
      if (!value && requireAuth) {
        requireAuth.checked = false;
      }
    }

    function clearInput() {
      if (input) {
        input.value = '';
      }
    }

    function approverEdited() {
      var current = Array.prototype.map.call(list.querySelectorAll('input[name=allowed]'), function (i) {
        return i.value;
      }).join(',');
      setSenders(current, approverValue());
    }

    function reset() {
      if (list) {
        list.innerHTML = '';
      }
      setApprover('');
      clearInput();
      setRestricted(false);
      setRequireAuth(false);
      setMX(false);
      refreshSenderNote();
      refreshSenderEmpty();
    }

    if (opts.addBtn && input) {
      opts.addBtn.addEventListener('click', function () {
        addSender(input.value);
        input.value = '';
        input.focus();
      });
    }
    if (input) {
      input.addEventListener('keydown', function (e) {
        if (e.key === 'Enter') {
          e.preventDefault();
          addSender(input.value);
          input.value = '';
        }
      });
    }
    if (restrict) {
      restrict.addEventListener('change', refreshRestrictVisibility);
    }
    if (approverEmail) {
      approverEmail.addEventListener('input', approverEdited);
    }

    reset();

    return {
      setSenders: setSenders,
      setApprover: setApprover,
      setRestricted: setRestricted,
      setRequireAuth: setRequireAuth,
      setMX: setMX,
      clearInput: clearInput,
      reset: reset
    };
  };
})();

// initAliasEditor renders an inbox dialog's inline alias editor: a list of
// aliases (a sender name with the email address beneath it, plus edit and
// remove buttons) and an add/edit form inside the same panel. Each alias is
// submitted as a repeated `alias` field (the full address) plus a parallel
// repeated `alias_name` field, zipped by index server-side.
var initAliasEditor = (function () {
  return function (opts) {
    var list = opts.list;
    var form = opts.form || {};
    var editing = null;

    function rows() {
      if (!list) {
        return [];
      }
      return Array.prototype.slice.call(list.querySelectorAll('li.alias-row'));
    }

    function setError(msg) {
      if (!form.error) {
        return;
      }
      form.error.textContent = msg || '';
      form.error.hidden = !msg;
    }

    function refreshEmpty() {
      if (!list) {
        return;
      }
      var empty = list.querySelector('.empty');
      var hasAliases = rows().length > 0;
      if (!hasAliases) {
        if (!empty) {
          var li = document.createElement('li');
          li.className = 'empty';
          li.textContent = 'No aliases.';
          list.appendChild(li);
        }
      } else if (empty) {
        empty.remove();
      }
      if (opts.onChange) {
        opts.onChange();
      }
    }

    function iconButton(cls, label, svg) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'secondary icon-btn' + (cls ? ' ' + cls : '');
      b.title = label;
      b.setAttribute('aria-label', label);
      b.innerHTML = svg;
      return b;
    }

    var pencilSVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M11.4 2l2.6 2.6L5.6 13l-3.1.5.5-3.1z"/></svg>';
    var crossSVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M4 4l8 8M12 4l-8 8"/></svg>';

    function makeRow(name, address) {
      var li = document.createElement('li');
      li.className = 'alias-row';
      li.dataset.address = address;
      li.dataset.name = name || '';

      var text = document.createElement('div');
      text.className = 'alias-text';
      var nameEl = document.createElement('span');
      nameEl.className = 'alias-row-name';
      nameEl.textContent = name || '(no name)';
      if (!name) {
        nameEl.classList.add('muted');
      }
      var addrEl = document.createElement('span');
      addrEl.className = 'alias-row-addr';
      addrEl.textContent = address;
      text.appendChild(nameEl);
      text.appendChild(addrEl);

      var hiddenName = document.createElement('input');
      hiddenName.type = 'hidden';
      hiddenName.name = 'alias_name';
      hiddenName.value = name || '';
      var hiddenAddr = document.createElement('input');
      hiddenAddr.type = 'hidden';
      hiddenAddr.name = 'alias';
      hiddenAddr.value = address;

      var edit = iconButton('', 'Edit', pencilSVG);
      edit.addEventListener('click', function () {
        var at = address.lastIndexOf('@');
        openEditor(li, name, at > 0 ? address.slice(0, at) : address, at > 0 ? address.slice(at + 1) : '');
      });
      var remove = iconButton('danger', 'Remove', crossSVG);
      remove.addEventListener('click', function () {
        li.remove();
        refreshEmpty();
      });

      li.appendChild(text);
      li.appendChild(hiddenName);
      li.appendChild(hiddenAddr);
      li.appendChild(edit);
      li.appendChild(remove);
      return li;
    }

    function openEditor(row, name, local, domain) {
      editing = row || null;
      setError('');
      if (form.name) {
        form.name.value = name || '';
      }
      if (form.local) {
        form.local.value = local || '';
      }
      if (form.domain && domain) {
        form.domain.value = domain;
      }
      if (form.save) {
        form.save.textContent = editing ? 'Save' : 'Add';
      }
      if (opts.editor) {
        opts.editor.hidden = false;
      }
      if (form.name) {
        form.name.focus();
      }
    }

    function closeEditor() {
      editing = null;
      setError('');
      if (opts.editor) {
        opts.editor.hidden = true;
      }
    }

    function commit() {
      var name = (form.name ? form.name.value : '').trim();
      var local = (form.local ? form.local.value : '').trim().toLowerCase();
      var domain = (form.domain ? form.domain.value : '').trim().toLowerCase();
      if (!name) {
        setError('Name is required.');
        return;
      }
      if (name.length > 128) {
        setError('Name must be 128 characters or fewer.');
        return;
      }
      if (name.indexOf(',') !== -1 || /[\r\n]/.test(name) || /[\x00-\x1f\x7f]/.test(name)) {
        setError('Name may not contain commas, newlines or control characters.');
        return;
      }
      if (!local || local.indexOf('@') !== -1 || local.indexOf(' ') !== -1) {
        setError('Enter a valid email local part.');
        return;
      }
      if (!domain) {
        setError('Choose a domain.');
        return;
      }
      var address = local + '@' + domain;
      var duplicate = rows().some(function (r) {
        return r !== editing && r.dataset.address === address;
      });
      if (duplicate) {
        setError('That alias already exists.');
        return;
      }
      if (editing) {
        editing.dataset.address = address;
        editing.dataset.name = name;
        var nameEl = editing.querySelector('.alias-row-name');
        nameEl.textContent = name;
        nameEl.classList.remove('muted');
        editing.querySelector('.alias-row-addr').textContent = address;
        editing.querySelector('input[name=alias_name]').value = name;
        editing.querySelector('input[name=alias]').value = address;
      } else if (list) {
        list.appendChild(makeRow(name, address));
      }
      closeEditor();
      refreshEmpty();
    }

    // setAliases takes the comma-joined address list and an optional parallel
    // comma-joined name list (both emitted in the same order by the server).
    function setAliases(raw, rawNames) {
      if (!list) {
        return;
      }
      list.innerHTML = '';
      var addresses = (raw || '').split(',');
      var names = (rawNames || '').split(',');
      addresses.forEach(function (entry, i) {
        var address = (entry || '').trim().toLowerCase();
        if (!address) {
          return;
        }
        list.appendChild(makeRow((names[i] || '').trim(), address));
      });
      refreshEmpty();
    }

    function reset() {
      closeEditor();
      if (list) {
        list.innerHTML = '';
      }
      if (form.name) {
        form.name.value = '';
      }
      if (form.local) {
        form.local.value = '';
      }
      refreshEmpty();
    }

    if (opts.addBtn) {
      opts.addBtn.addEventListener('click', function () {
        var domainEl = form.domain;
        var def = domainEl && domainEl.options.length ? domainEl.options[0].value : '';
        openEditor(null, '', '', def);
      });
    }
    if (form.save) {
      form.save.addEventListener('click', commit);
    }
    if (form.cancel) {
      form.cancel.addEventListener('click', closeEditor);
    }
    [form.name, form.local].forEach(function (el) {
      if (el) {
        el.addEventListener('keydown', function (e) {
          if (e.key === 'Enter') {
            e.preventDefault();
            commit();
          }
        });
      }
    });

    refreshEmpty();

    return {
      setAliases: setAliases,
      reset: reset
    };
  };
})();

// externalAliasBase builds the dedicated page path for one external alias.
function externalAliasBase(inboxID, aliasID) {
  return '/ui/inboxes/' + encodeURIComponent(inboxID) + '/external-aliases/' + encodeURIComponent(aliasID);
}

// renderExternalAliases fills an inbox dialog's external-alias list from the
// secret-free JSON on the edit button. Each row shows the sender name and
// address, an edit button for the sender name, a domain-style connector button
// (amber until configured) that opens the alias's connector popup, and an
// Activity link.
function renderExternalAliases(list, inboxID, raw) {
  if (!list) {
    return;
  }
  var data = [];
  try {
    data = JSON.parse(raw || '[]') || [];
  } catch (e) {
    data = [];
  }
  list.innerHTML = '';
  if (!data.length) {
    var empty = document.createElement('li');
    empty.className = 'empty';
    empty.textContent = 'No external sending aliases.';
    list.appendChild(empty);
    return;
  }
  data.forEach(function (a) {
    var li = document.createElement('li');
    li.className = 'alias-row external';

    var text = document.createElement('div');
    text.className = 'alias-text';
    var nameEl = document.createElement('span');
    nameEl.className = 'alias-row-name';
    nameEl.textContent = a.display_name || '(no name)';
    if (!a.display_name) {
      nameEl.classList.add('muted');
    }
    var addrEl = document.createElement('span');
    addrEl.className = 'alias-row-addr';
    addrEl.textContent = a.address + ' · External · sending only';
    text.appendChild(nameEl);
    text.appendChild(addrEl);

    // Domain-style connector button: amber "Add" until configured, then a
    // secondary button labelled with the provider.
    var edit = document.createElement('button');
    edit.type = 'button';
    edit.className = 'secondary icon-btn external-alias-edit';
    edit.dataset.alias = a.id;
    edit.dataset.name = a.display_name || '';
    edit.dataset.address = a.address;
    edit.title = 'Edit sender name';
    edit.setAttribute('aria-label', 'Edit sender name');
    edit.innerHTML = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M11.4 2l2.6 2.6L5.6 13l-3.1.5.5-3.1z"/></svg>';

    var configure = document.createElement('button');
    configure.type = 'button';
    configure.className = (a.configured ? 'secondary' : 'amber') + ' btn-sm cell-edit external-alias-configure';
    configure.dataset.alias = a.id;
    configure.textContent = a.configured ? (a.provider || 'Configured') : 'Add';
    configure.title = a.configured ? 'Edit sending connector' : 'Configure sending connector';
    configure.setAttribute('aria-label', configure.title);

    var activity = document.createElement('a');
    activity.className = 'btn secondary icon-btn';
    activity.href = externalAliasBase(inboxID, a.id);
    activity.title = 'Activity';
    activity.setAttribute('aria-label', 'Activity');
    activity.innerHTML = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3.5" y="2.5" width="9" height="11" rx="1.5"/><path d="M5.5 5.5h5M5.5 8h5M5.5 10.5h3"/></svg>';

    var remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'secondary icon-btn danger external-alias-remove';
    remove.dataset.alias = a.id;
    remove.dataset.address = a.address;
    remove.title = 'Delete';
    remove.setAttribute('aria-label', 'Delete');
    remove.innerHTML = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg>';

    li.appendChild(text);
    li.appendChild(edit);
    li.appendChild(configure);
    li.appendChild(activity);
    li.appendChild(remove);
    list.appendChild(li);
  });
}

// deleteExternalAlias POSTs the alias's delete endpoint from a detached form.
// The row lives inside the inbox edit form (which may not nest a form), so a
// one-off form is submitted instead.
function deleteExternalAlias(inboxID, aliasID, address) {
  if (!window.confirm('Delete external alias ' + (address || '') + ' and its connector? This cannot be undone.')) {
    return;
  }
  var csrf = document.querySelector('#inbox-edit-form [name=_csrf]');
  var form = document.createElement('form');
  form.method = 'post';
  form.action = '/ui/inboxes/' + encodeURIComponent(inboxID) + '/external-aliases/' + encodeURIComponent(aliasID) + '/delete';
  var tok = document.createElement('input');
  tok.type = 'hidden';
  tok.name = '_csrf';
  tok.value = csrf ? csrf.value : '';
  form.appendChild(tok);
  document.body.appendChild(form);
  form.submit();
}

// openExternalAliasDialog opens the external-alias sending editor as a sub-view
// of the inbox settings dialog. The editors are rendered server-side on the
// dashboard, keyed by alias id.
function openExternalAliasDialog(dlg, aliasID) {
  if (!dlg || !aliasID) {
    return;
  }
  var found = false;
  dlg.querySelectorAll('.extalias-sending-editor').forEach(function (ed) {
    var on = ed.getAttribute('data-alias-id') === aliasID;
    ed.hidden = !on;
    found = found || on;
  });
  if (found) {
    showInboxSubview(dlg, 'ext-alias-sending');
  }
}

// senderLabel renders an option as "Name (email)". Without a name it falls
// back to the bare address.
function senderLabel(name, address) {
  if (name) {
    return name + ' (' + address + ')';
  }
  return address;
}

// buildDefaultSenderSelect populates the Primary/Default Address <select> from
// the current alias list. The primary (empty value) is labelled with the inbox
// display name; each alias with its sender name. The desired value is kept
// selected when it still exists, otherwise the primary is selected.
function buildDefaultSenderSelect(select, primaryName, primary, aliasNames, aliases, desired) {
  if (!select) {
    return;
  }
  select.innerHTML = '';
  var primaryOpt = document.createElement('option');
  primaryOpt.value = '';
  if (primary) {
    primaryOpt.textContent = senderLabel(primaryName, primary);
  } else {
    primaryOpt.textContent = 'Primary address';
  }
  select.appendChild(primaryOpt);
  (aliases || []).forEach(function (addr) {
    if (!addr) {
      return;
    }
    var opt = document.createElement('option');
    opt.value = addr;
    opt.textContent = senderLabel((aliasNames || {})[addr], addr);
    select.appendChild(opt);
  });
  var want = (desired || '').toLowerCase();
  var matched = false;
  Array.prototype.forEach.call(select.options, function (o) {
    if (o.value && o.value.toLowerCase() === want) {
      matched = true;
    }
  });
  select.value = matched ? desired : '';
}

// currentAliasValues reads the hidden `alias` inputs from an alias editor list.
function currentAliasValues(list) {
  if (!list) {
    return [];
  }
  return Array.prototype.map.call(list.querySelectorAll('input[name=alias]'), function (i) {
    return i.value;
  });
}

// aliasNameByAddress reads a `li.alias-row` list into an address-to-name
// lookup, for labelling the default-sender select.
function aliasNameByAddress(list) {
  var out = {};
  if (!list) {
    return out;
  }
  list.querySelectorAll('li.alias-row').forEach(function (row) {
    var addr = row.dataset.address;
    if (addr) {
      out[addr] = row.dataset.name || '';
    }
  });
  return out;
}

(function () {
  var dlg = document.getElementById('inbox-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var addQuotaValue = document.getElementById('inbox-add-quota-value');
  var addQuotaUnit = document.getElementById('inbox-add-quota-unit');
  var addQuotaUnlimited = document.getElementById('inbox-add-quota-unlimited');
  if (addQuotaUnlimited) {
    addQuotaUnlimited.addEventListener('change', function () {
      if (addQuotaValue) {
        addQuotaValue.disabled = addQuotaUnlimited.checked;
      }
      if (addQuotaUnit) {
        addQuotaUnit.disabled = addQuotaUnlimited.checked;
      }
    });
  }
  var editor = initSenderEditor({
    list: document.getElementById('inbox-add-sender-list'),
    input: document.getElementById('inbox-add-sender-input'),
    note: document.getElementById('inbox-add-sender-note'),
    approverEmail: document.getElementById('inbox-add-approver-email'),
    restrict: document.getElementById('inbox-add-sender-restricted'),
    requireAuth: document.getElementById('inbox-add-require-auth'),
    section: document.getElementById('inbox-add-sender-section'),
    mxSection: document.getElementById('inbox-add-require-auth-section'),
    addBtn: document.getElementById('inbox-add-sender-add')
  });
  var addDomainEl = document.querySelector('#inbox-dialog [name=domain]');
  function refreshAddRequireAuth() {
    var opt = addDomainEl && addDomainEl.options[addDomainEl.selectedIndex];
    editor.setMX(!!(opt && opt.getAttribute('data-mx') === '1'));
  }
  if (addDomainEl) {
    addDomainEl.addEventListener('change', refreshAddRequireAuth);
  }
  var addAliasList = document.getElementById('inbox-add-alias-list');
  var addDefault = document.getElementById('inbox-add-default-sender');
  function addPrimary() {
    var localEl = document.querySelector('#inbox-dialog [name=local]');
    var domainEl = document.querySelector('#inbox-dialog [name=domain]');
    var displayEl = document.querySelector('#inbox-dialog [name=display]');
    var local = localEl ? localEl.value.trim().toLowerCase() : '';
    var domainName = domainEl ? domainEl.value.trim().toLowerCase() : '';
    return {
      address: local && domainName ? local + '@' + domainName : '',
      name: displayEl ? displayEl.value.trim() : ''
    };
  }
  function refreshAddSender() {
    var primary = addPrimary();
    buildDefaultSenderSelect(addDefault, primary.name, primary.address, aliasNameByAddress(addAliasList), currentAliasValues(addAliasList), addDefault ? addDefault.value : '');
  }
  var addDisplay = document.querySelector('#inbox-dialog [name=display]');
  if (addDisplay) {
    addDisplay.addEventListener('input', refreshAddSender);
  }
  var aliasEditor = initAliasEditor({
    list: addAliasList,
    addBtn: document.getElementById('inbox-add-alias-add'),
    editor: document.getElementById('inbox-add-alias-editor'),
    form: {
      name: document.getElementById('inbox-add-alias-name'),
      local: document.getElementById('inbox-add-alias-local'),
      domain: document.getElementById('inbox-add-alias-domain'),
      error: document.getElementById('inbox-add-alias-error'),
      save: document.getElementById('inbox-add-alias-save'),
      cancel: document.getElementById('inbox-add-alias-cancel')
    },
    onChange: refreshAddSender
  });
  var add = document.getElementById('add-inbox');
  if (add) {
    add.addEventListener('click', function () {
      form.reset();
      editor.reset();
      refreshAddRequireAuth();
      aliasEditor.reset();
      refreshAddSender();
      if (dlg._resetTabs) {
        dlg._resetTabs();
      }
      dlg.showModal();
    });
  }
  var cancel = document.getElementById('inbox-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      dlg.close();
    });
  }
  bindInboxSettingsShell(dlg);
})();

(function () {
  var dlg = document.getElementById('add-domain-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var input = document.getElementById('add-domain-name');
  var panel = document.getElementById('add-domain-inherit');
  var parentEl = document.getElementById('add-domain-parent');
  var controls = document.getElementById('add-domain-inherit-controls');
  var names = (form.getAttribute('data-domain-names') || '')
    .split(',')
    .map(function (s) { return s.trim().toLowerCase(); })
    .filter(Boolean);
  // detect reveals the reuse-parent checkboxes when the typed name is a
  // subdomain of an existing domain; server-side detection is authoritative, so
  // with JS off the domain is still created as a subdomain with inheritance on.
  function detect() {
    if (!input || !panel) {
      return;
    }
    var v = (input.value || '').trim().toLowerCase().replace(/\.$/, '');
    var parent = '';
    names.forEach(function (d) {
      if (v.length > d.length + 1 && v.slice(-(d.length + 1)) === '.' + d) {
        if (d.length > parent.length) {
          parent = d;
        }
      }
    });
    if (parent) {
      if (parentEl) {
        parentEl.textContent = parent;
      }
      document.querySelectorAll('.add-domain-parent').forEach(function (el) {
        el.textContent = parent;
      });
      panel.hidden = false;
      if (controls) {
        controls.value = '1';
      }
    } else {
      panel.hidden = true;
      if (controls) {
        controls.value = '';
      }
    }
  }
  if (input) {
    input.addEventListener('input', detect);
  }
  var add = document.getElementById('add-domain');
  if (add) {
    add.addEventListener('click', function () {
      form.reset();
      detect();
      dlg.showModal();
    });
  }
  var cancel = document.getElementById('add-domain-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      dlg.close();
    });
  }
})();

(function () {
  var dlg = document.getElementById('domain-delete-dialog');
  if (!dlg) {
    return;
  }
  var form = document.getElementById('domain-delete-form');
  var input = document.getElementById('domain-delete-input');
  var submit = document.getElementById('domain-delete-submit');
  var nameEl = document.getElementById('domain-delete-name');
  var expected = '';
  function matches() {
    return input.value.trim().toLowerCase() === expected.toLowerCase();
  }
  function sync() {
    submit.disabled = !matches();
  }
  document.querySelectorAll('.open-delete-domain').forEach(function (btn) {
    btn.addEventListener('click', function () {
      expected = btn.getAttribute('data-name') || '';
      form.setAttribute('action', '/ui/domains/' + btn.getAttribute('data-domain') + '/delete');
      nameEl.textContent = expected;
      input.value = '';
      sync();
      dlg.showModal();
      input.focus();
    });
  });
  input.addEventListener('input', sync);
  form.addEventListener('submit', function (e) {
    if (!matches()) {
      e.preventDefault();
      sync();
    }
  });
})();

(function () {
  var dlg = document.getElementById('inbox-delete-dialog');
  if (!dlg) {
    return;
  }
  var form = document.getElementById('inbox-delete-form');
  var input = document.getElementById('inbox-delete-input');
  var submit = document.getElementById('inbox-delete-submit');
  var addressEl = document.getElementById('inbox-delete-address');
  var expected = '';
  function matches() {
    return input.value.trim().toLowerCase() === expected.toLowerCase();
  }
  function sync() {
    submit.disabled = !matches();
  }
  document.querySelectorAll('.open-delete-inbox').forEach(function (btn) {
    btn.addEventListener('click', function () {
      expected = btn.getAttribute('data-address') || '';
      form.setAttribute('action', '/ui/inboxes/' + btn.getAttribute('data-id') + '/delete');
      addressEl.textContent = expected;
      input.value = '';
      sync();
      dlg.showModal();
      input.focus();
    });
  });
  input.addEventListener('input', sync);
  form.addEventListener('submit', function (e) {
    if (!matches()) {
      e.preventDefault();
      sync();
    }
  });
})();

(function () {
  var dlg = document.getElementById('client-delete-dialog');
  if (!dlg) {
    return;
  }
  var form = document.getElementById('client-delete-form');
  var label = document.getElementById('client-delete-label');
  document.querySelectorAll('.open-delete-client').forEach(function (btn) {
    btn.addEventListener('click', function () {
      form.setAttribute('action', '/ui/' + btn.getAttribute('data-kind') + '/' + btn.getAttribute('data-id') + '/delete');
      var type = btn.getAttribute('data-type') || '';
      var name = btn.getAttribute('data-name') || '';
      label.textContent = name ? name + ' (' + type + ')' : type;
      dlg.showModal();
    });
  });
})();

(function () {
  var form = document.getElementById('bulk-form');
  if (!form) {
    return;
  }
  var all = document.getElementById('select-all');
  var boxes = document.querySelectorAll('input[name=ids][form=bulk-form]');
  function refreshAll() {
    if (!all) {
      return;
    }
    var n = 0;
    boxes.forEach(function (box) {
      if (box.checked) {
        n++;
      }
    });
    all.checked = n > 0 && n === boxes.length;
    all.indeterminate = n > 0 && n < boxes.length;
  }
  if (all) {
    all.addEventListener('change', function () {
      boxes.forEach(function (box) {
        box.checked = all.checked;
      });
    });
  }
  boxes.forEach(function (box) {
    box.addEventListener('change', refreshAll);
  });
})();

(function () {
  document.querySelectorAll('button[data-confirm]').forEach(function (btn) {
    btn.addEventListener('click', function (e) {
      if (!window.confirm(btn.getAttribute('data-confirm'))) {
        e.preventDefault();
      }
    });
  });
})();

// clearUrlParams strips dialog-opening query params from the address bar so a
// refresh after cancelling a URL-driven dialog does not re-open it. Replace
// is safe to call while the page is navigating (the browser ignores it), so
// submit/redirect flows are unaffected.
function clearUrlParams(names) {
  if (!window.history || !window.history.replaceState || !window.URLSearchParams) {
    return;
  }
  var url = new URL(window.location.href);
  var changed = false;
  names.forEach(function (name) {
    if (url.searchParams.has(name)) {
      url.searchParams.delete(name);
      changed = true;
    }
  });
  if (changed) {
    window.history.replaceState(null, '', url.pathname + url.search + url.hash);
  }
}

(function () {
  var dlg = document.getElementById('inbox-edit-dialog');
  if (!dlg) {
    return;
  }
  // splitBytes picks the largest binary unit that divides a byte count evenly,
  // so a cap entered as "50 MB" round-trips as "50" + MB rather than 51200 KB.
  function splitBytes(n) {
    var units = [['tb', 1099511627776], ['gb', 1073741824], ['mb', 1048576], ['kb', 1024], ['b', 1]];
    for (var i = 0; i < units.length; i++) {
      if (n % units[i][1] === 0) {
        return { value: n / units[i][1], unit: units[i][0] };
      }
    }
    return { value: n, unit: 'b' };
  }
  var form = document.getElementById('inbox-edit-form');
  var deleteBtn = document.getElementById('inbox-edit-delete');
  var address = document.getElementById('inbox-edit-address');
  var display = form.querySelector('[name=display]');
  var usage = document.getElementById('inbox-edit-usage');
  var trashOverride = document.getElementById('inbox-trash-retention-override');
  var trashSection = document.getElementById('inbox-trash-retention-section');
  var trashDays = document.getElementById('inbox-trash-retention-days');
  var quotaValue = document.getElementById('inbox-edit-quota-value');
  var quotaUnit = document.getElementById('inbox-edit-quota-unit');
  var quotaUnlimited = document.getElementById('inbox-edit-quota-unlimited');
  if (quotaUnlimited) {
    quotaUnlimited.addEventListener('change', function () {
      if (quotaValue) {
        quotaValue.disabled = quotaUnlimited.checked;
      }
      if (quotaUnit) {
        quotaUnit.disabled = quotaUnlimited.checked;
      }
    });
  }
  if (trashOverride && trashSection) {
    trashOverride.addEventListener('change', function () {
      trashSection.hidden = !trashOverride.checked;
    });
  }
  var autoMarkRead = document.getElementById('inbox-auto-mark-read');
  var autoTrash = document.getElementById('inbox-auto-trash');
  var autoTrashSection = document.getElementById('inbox-auto-trash-section');
  var autoTrashHours = document.getElementById('inbox-auto-trash-hours');
  var deliveryTrigger = document.getElementById('inbox-delivery-trigger');
  if (autoTrash && autoTrashSection) {
    autoTrash.addEventListener('change', function () {
      autoTrashSection.hidden = !autoTrash.checked;
    });
  }
  // The Connectors panel keeps its delivery-action controls in this form.
  var connectorsForm = document.getElementById('inbox-connectors-form');
  var connectorList = document.getElementById('inbox-connectors-list');
  var connectorEditor = document.getElementById('inbox-connector-editor');
  var connectorAdd = document.getElementById('inbox-connector-add');
  var editConnectors = [];
  var csrfValue = (form.querySelector('[name=_csrf]') || {}).value || '';
  var editor = initSenderEditor({
    list: document.getElementById('inbox-sender-list'),
    input: document.getElementById('inbox-sender-input'),
    note: document.getElementById('inbox-sender-note'),
    approverEmail: document.getElementById('inbox-edit-approver-email'),
    restrict: document.getElementById('inbox-sender-restricted'),
    requireAuth: document.getElementById('inbox-require-auth'),
    section: document.getElementById('inbox-sender-section'),
    mxSection: document.getElementById('inbox-edit-require-auth-section'),
    addBtn: document.getElementById('inbox-sender-add')
  });
  // The inbox settings shell owns section navigation and sub-views; see
  // bindInboxSettingsShell.
  var inboxSubmitting = false;
  var inboxSaveButton = document.getElementById('inbox-edit-save');
  // A genuine dismissal drops the URL params that reopened the dialog. A submit
  // navigates, so the browser ignores the replace and the server redirect takes
  // over the return path.
  dlg.addEventListener('close', function () {
    if (inboxSubmitting) {
      return;
    }
    clearUrlParams(['inbox', 'inbox_tab', 'alias', 'provider']);
  });
  form.addEventListener('submit', function () {
    inboxSubmitting = true;
  });
  if (connectorsForm) {
    connectorsForm.addEventListener('submit', function () {
      inboxSubmitting = true;
    });
  }
  // On the Connectors panel the footer Save posts the auto-actions form, which
  // is separate from the inbox identity form.
  if (inboxSaveButton && connectorsForm) {
    inboxSaveButton.addEventListener('click', function (event) {
      var panel = dlg.querySelector('[data-inbox-panel=connectors]');
      if (!panel || panel.hidden) {
        return;
      }
      event.preventDefault();
      inboxSubmitting = true;
      connectorsForm.requestSubmit();
    });
  }
  var editAliasList = document.getElementById('inbox-alias-list');
  var editExternalList = document.getElementById('inbox-external-alias-list');
  var editInboxID = '';
  var editPrimary = '';
  var editPrimaryName = '';
  var editDefault = document.getElementById('inbox-default-sender');
  var editDesired = '';
  // editExternal holds the secret-free external aliases for the open inbox, so
  // the default-sender select can include them and preserve an external default.
  var editExternal = [];
  function editSenderOptions() {
    var names = aliasNameByAddress(editAliasList);
    var addrs = currentAliasValues(editAliasList);
    editExternal.forEach(function (a) {
      addrs.push(a.address);
      if (a.display_name) {
        names[a.address] = a.display_name;
      }
    });
    return { names: names, addresses: addrs };
  }
  function refreshEditSender() {
    var opts = editSenderOptions();
    var desired = editDesired || (editDefault ? editDefault.value : '');
    buildDefaultSenderSelect(editDefault, editPrimaryName, editPrimary, opts.names, opts.addresses, desired);
  }
  var aliasEditor = initAliasEditor({
    list: editAliasList,
    addBtn: document.getElementById('inbox-alias-add'),
    editor: document.getElementById('inbox-alias-editor'),
    form: {
      name: document.getElementById('inbox-alias-editor-name'),
      local: document.getElementById('inbox-alias-editor-local'),
      domain: document.getElementById('inbox-alias-editor-domain'),
      error: document.getElementById('inbox-alias-editor-error'),
      save: document.getElementById('inbox-alias-editor-save'),
      cancel: document.getElementById('inbox-alias-editor-cancel')
    },
    onChange: refreshEditSender
  });

  function escapeConnector(value) {
    return String(value == null ? '' : value).replace(/[&<>"']/g, function (ch) {
      return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch];
    });
  }

  function connectorLabel(c) {
    if (c.Kind === 'hermes') {
      return 'Hermes Relay';
    }
    if (c.Kind === 'openclaw') {
      return 'OpenClaw';
    }
    return 'Webhook';
  }

  function setConnectorEditorMode(editing) {
    // The connector editor is an in-modal sub-view: the whole panels area
    // switches to it, so the footer Save follows the edit form.
    if (inboxSaveButton && connectorEditor) {
      inboxSaveButton.setAttribute('form', editing ? 'inbox-connector-edit-form' : 'inbox-connectors-form');
    }
    if (editing) {
      showInboxSubview(dlg, 'connectors');
    }
    if (connectorEditor && !editing) {
      connectorEditor.textContent = '';
    }
  }

  function renderConnectorEditor(c) {
    if (!connectorEditor || !c) {
      return;
    }
    var id = encodeURIComponent(c.ID || '');
    var inbox = escapeConnector(editInboxID);
    var common = '<input type="hidden" name="_csrf" value="' + escapeConnector(csrfValue) + '"><input type="hidden" name="inbox" value="' + inbox + '">';
    var fields = '<label>Name</label><input name="name" maxlength="128" required value="' + escapeConnector(c.Name) + '">';
    var actions = '';
    if (c.Kind === 'hermes' || c.Kind === 'openclaw') {
      var isOpenClaw = c.Kind === 'openclaw';
      var label = isOpenClaw ? 'OpenClaw' : 'Hermes Relay';
      fields += '<label>Outbound authority</label><select name="role"><option value="owner"' + (c.Role === 'owner' ? ' selected' : '') + '>Owner — ' + (isOpenClaw ? 'agent' : 'relay') + ' sends directly</option><option value="assistant"' + (c.Role === 'assistant' ? ' selected' : '') + '>Assistant — ' + (isOpenClaw ? 'agent' : 'relay') + ' drafts and requests approval</option></select>';
       actions = '<a class="btn secondary" href="/ui/clients/' + id + '/log">Delivery log</a>';
       connectorEditor.innerHTML = '<div class="card-head"><h3>' + escapeConnector(c.Name || label) + '</h3></div><form id="inbox-connector-edit-form" method="post" action="/ui/' + (isOpenClaw ? 'openclaw' : 'hermes') + '/' + id + '/edit">' + common + fields + '<div class="dialog-actions">' + actions + '</div></form><form method="post" action="/ui/' + (isOpenClaw ? 'openclaw' : 'hermes') + '/' + id + '/delete" data-inline-connector-delete>' + common + '<button class="secondary danger">Remove connector</button></form>';
    } else {
      fields += '<label>Destination URL</label><input name="url" type="url" required value="' + escapeConnector(c.URL) + '"><label>Payload</label><select name="mode"><option value="notify"' + (c.Mode === 'notify' ? ' selected' : '') + '>Notify — small JSON with the message id</option><option value="forward"' + (c.Mode === 'forward' ? ' selected' : '') + '>Forward — full raw MIME</option></select><label>Authentication</label><select name="auth" data-inline-webhook-auth><option value="signature"' + (c.AuthMode === 'signature' ? ' selected' : '') + '>Signature — signed HMAC-SHA256 header</option><option value="bearer"' + (c.AuthMode === 'bearer' ? ' selected' : '') + '>Bearer — static token</option></select><div data-inline-bearer' + (c.AuthMode === 'bearer' ? '' : ' hidden') + '><label>Bearer secret <span class="muted small">(leave blank to keep current)</span></label><input name="bearer_secret" type="password" autocomplete="new-password"></div>';
       actions = '<a class="btn secondary" href="/ui/clients/' + id + '/log">Delivery log</a>' + (c.AuthMode === 'signature' ? '<button type="button" class="secondary" data-rotate-webhook="' + escapeConnector(c.ID) + '">Rotate signing secret</button>' : '');
      var toggleText = c.Enabled ? 'Pause webhook' : 'Enable webhook';
      var nextEnabled = c.Enabled ? '0' : '1';
       connectorEditor.innerHTML = '<div class="card-head"><h3>' + escapeConnector(c.Name || 'Webhook') + '</h3></div><form id="inbox-connector-edit-form" method="post" action="/ui/webhooks/' + id + '/edit">' + common + fields + '<div class="dialog-actions">' + actions + '</div></form><div class="row"><form method="post" action="/ui/webhooks/' + id + '/toggle">' + common + '<input type="hidden" name="enabled" value="' + nextEnabled + '"><button class="secondary">' + toggleText + '</button></form><form method="post" action="/ui/webhooks/' + id + '/delete" data-inline-connector-delete>' + common + '<button class="secondary danger">Remove connector</button></form></div>';
    }
    setConnectorEditorMode(true);
    var connectorEditForm = connectorEditor.querySelector('#inbox-connector-edit-form');
    if (connectorEditForm) {
      connectorEditForm.addEventListener('submit', function () {
        inboxSubmitting = true;
      });
    }
    connectorEditor.querySelectorAll('[data-inline-connector-delete]').forEach(function (deleteForm) {
      deleteForm.addEventListener('submit', function (e) {
        if (!window.confirm('Remove this connector?')) {
          e.preventDefault();
        }
      });
    });
    var auth = connectorEditor.querySelector('[data-inline-webhook-auth]');
    if (auth) {
      auth.addEventListener('change', function () {
        var bearer = connectorEditor.querySelector('[data-inline-bearer]');
        if (bearer) {
          bearer.hidden = auth.value !== 'bearer';
          var input = bearer.querySelector('input');
          if (input) {
            input.disabled = auth.value !== 'bearer';
          }
        }
      });
    }
    var rotate = connectorEditor.querySelector('[data-rotate-webhook]');
    if (rotate) {
      rotate.addEventListener('click', function () {
        if (!window.confirm('Rotate this webhook signing secret? The current secret stops working immediately.')) {
          return;
        }
        var body = new URLSearchParams();
        body.append('_csrf', csrfValue);
        fetch('/ui/webhooks/' + encodeURIComponent(rotate.getAttribute('data-rotate-webhook')) + '/rotate', {
          method: 'POST',
          headers: {Accept: 'application/json','Content-Type':'application/x-www-form-urlencoded;charset=UTF-8'},
          body: body.toString(),
          credentials: 'same-origin'
        }).then(function (res) {
          if (!res.ok) {
            return res.text().then(function (t) { throw new Error(t || 'Could not rotate secret'); });
          }
          return res.json();
        }).then(function (data) {
          window.alert((data.label || 'New signing secret') + '\n\n' + (data.secret || ''));
        }).catch(function (err) {
          window.alert(err.message || 'Could not rotate secret');
        });
      });
    }
  }

  function renderConnectors(connectors) {
    editConnectors = Array.isArray(connectors) ? connectors : [];
    if (!connectorList || !connectorEditor) {
      return;
    }
    connectorList.textContent = '';
    setConnectorEditorMode(false);

    var settingsSVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06-.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09A1.65 1.65 0 0 0 19.4 15z"/></svg>';
    var hermesSVG = '<img class="connector-brand-image" alt="" aria-hidden="true" src="/assets/hermes-connector.png">';
    var openclawSVG = '<img class="connector-brand-image connector-brand-openclaw" alt="" aria-hidden="true" src="/assets/openclaw-connector.png">';
    var webhookSVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 16.98h-5.99c-1.1 0-1.95.94-2.48 1.9A4 4 0 0 1 2 17c.01-.7.2-1.4.57-2"/><path d="m6 17 3.13-5.78c.53-.97.1-2.18-.5-3.1a4 4 0 1 1 6.89-4.06"/><path d="m12 6 3.13 5.73C15.66 12.7 16.9 13 18 13a4 4 0 0 1 0 8"/></svg>';
    var logSVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M4 2.5h8v11H4z"/><path d="M6 5h4M6 8h4M6 11h3"/></svg>';
    var deleteSVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg>';

    function iconButton(label, svg, extraClass) {
      var button = document.createElement('button');
      button.type = 'button';
      button.className = 'secondary icon-btn' + (extraClass ? ' ' + extraClass : '');
      button.title = label;
      button.setAttribute('aria-label', label);
      button.innerHTML = svg;
      return button;
    }

    if (!editConnectors.length) {
      var empty = document.createElement('p');
      empty.className = 'muted';
      empty.textContent = 'No connectors configured for this inbox.';
      connectorList.appendChild(empty);
      return;
    }

    editConnectors.forEach(function (c) {
      var row = document.createElement('div');
      row.className = 'connector-row';

      var kindIcon = document.createElement('span');
      kindIcon.className = 'connector-type-icon';
      kindIcon.title = connectorLabel(c);
      kindIcon.setAttribute('aria-hidden', 'true');
      kindIcon.innerHTML = c.Kind === 'hermes' ? hermesSVG : (c.Kind === 'openclaw' ? openclawSVG : webhookSVG);
      row.appendChild(kindIcon);

      var text = document.createElement('div');
      text.className = 'connector-row-main';
      var status = c.Kind === 'webhook' ? (c.Enabled ? 'Active' : 'Paused') : (c.Scope || 'Configured');
      text.innerHTML = '<div class="connector-row-name">' + escapeConnector(c.Name || connectorLabel(c)) + '</div><div class="connector-row-meta">' + connectorLabel(c) + ' · ' + escapeConnector(status) + '</div>';
      row.appendChild(text);

      var settings = iconButton('Settings', settingsSVG, '');
      settings.addEventListener('click', function () {
        renderConnectorEditor(c);
      });
      row.appendChild(settings);

      var log = document.createElement('a');
      log.className = 'btn secondary icon-btn';
      log.href = '/ui/clients/' + encodeURIComponent(c.ID || '') + '/log';
      log.title = 'Delivery log';
      log.setAttribute('aria-label', 'Delivery log');
      log.innerHTML = logSVG;
      row.appendChild(log);

      var deleteForm = document.createElement('form');
      deleteForm.method = 'post';
      deleteForm.action = c.Kind === 'hermes'
        ? '/ui/hermes/' + encodeURIComponent(c.ID || '') + '/delete'
        : (c.Kind === 'openclaw'
          ? '/ui/openclaw/' + encodeURIComponent(c.ID || '') + '/delete'
          : '/ui/webhooks/' + encodeURIComponent(c.ID || '') + '/delete');
      deleteForm.style.margin = '0';

      var csrf = document.createElement('input');
      csrf.type = 'hidden';
      csrf.name = '_csrf';
      csrf.value = csrfValue;
      deleteForm.appendChild(csrf);

      var inbox = document.createElement('input');
      inbox.type = 'hidden';
      inbox.name = 'inbox';
      inbox.value = editInboxID;
      deleteForm.appendChild(inbox);

      var remove = iconButton('Delete', deleteSVG, 'danger');
      remove.type = 'submit';
      deleteForm.appendChild(remove);
      deleteForm.addEventListener('submit', function (e) {
        if (!window.confirm('Remove this connector?')) {
          e.preventDefault();
        }
      });
      row.appendChild(deleteForm);

      connectorList.appendChild(row);
    });
  }

  document.querySelectorAll('.edit-inbox').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var id = encodeURIComponent(btn.dataset.id || '');
      editInboxID = btn.dataset.id || '';
      form.action = '/ui/inboxes/' + id + '/edit';
      if (connectorsForm) {
        connectorsForm.action = '/ui/inboxes/' + id + '/auto-actions';
      }
      if (deleteBtn) {
        deleteBtn.dataset.id = btn.dataset.id || '';
        deleteBtn.dataset.address = btn.dataset.address || '';
      }
      display.value = btn.dataset.name || '';
      address.value = btn.dataset.address || '';
      editor.setApprover(btn.dataset.approverEmail || '');
      editor.setRestricted(btn.dataset.restricted === '1');
      editor.setRequireAuth(btn.dataset.requireAuth === '1');
      editor.setMX(btn.dataset.mx === '1');
      editor.setSenders(btn.dataset.allowed || '', btn.dataset.approverEmail || '');
      editor.clearInput();
      editPrimary = btn.dataset.address || '';
      editPrimaryName = btn.dataset.name || '';
      editDesired = btn.dataset.defaultSender || '';
      aliasEditor.setAliases(btn.dataset.aliases || '', btn.dataset.aliasNames || '');
      try {
        editExternal = JSON.parse(btn.dataset.externalAliases || '[]') || [];
      } catch (e) {
        editExternal = [];
      }
      renderExternalAliases(editExternalList, btn.dataset.id || '', btn.dataset.externalAliases || '[]');
      try {
        editConnectors = JSON.parse(btn.dataset.connectors || '[]') || [];
      } catch (e) {
        editConnectors = [];
      }
      if (connectorAdd) {
        connectorAdd.setAttribute('data-inbox', editInboxID);
      }
      renderConnectors(editConnectors);
      var opts = editSenderOptions();
      buildDefaultSenderSelect(editDefault, editPrimaryName, editPrimary, opts.names, opts.addresses, editDesired);
      editDesired = '';
      var extForm = document.getElementById('external-alias-form');
      if (extForm) {
        extForm.action = '/ui/inboxes/' + id + '/external-aliases';
      }
      if (usage) {
        usage.textContent = btn.dataset.usage || '—';
      }
      if (trashOverride) {
        var trashVal = btn.dataset.trashRetention || '';
        trashOverride.checked = trashVal !== '';
        if (trashSection) {
          trashSection.hidden = !trashOverride.checked;
        }
        if (trashDays) {
          trashDays.value = trashVal !== '' ? trashVal : '0';
        }
      }
      if (quotaValue) {
        var quotaBytes = parseInt(btn.dataset.storageQuota || '', 10);
        var hasQuota = btn.dataset.storageQuota !== '' && !isNaN(quotaBytes) && quotaBytes > 0;
        if (quotaUnlimited) {
          quotaUnlimited.checked = !hasQuota;
          quotaValue.disabled = !hasQuota;
          if (quotaUnit) {
            quotaUnit.disabled = !hasQuota;
          }
        }
        if (hasQuota) {
          var q = splitBytes(quotaBytes);
          quotaValue.value = q.value;
          if (quotaUnit) {
            quotaUnit.value = q.unit;
          }
        } else {
          quotaValue.value = '';
          if (quotaUnit) {
            quotaUnit.value = 'mb';
          }
        }
      }
      if (autoMarkRead) {
        autoMarkRead.checked = btn.dataset.autoMarkRead === '1';
      }
      if (autoTrash) {
        var autoHours = btn.dataset.autoTrashHours || '';
        autoTrash.checked = autoHours !== '';
        if (autoTrashSection) {
          autoTrashSection.hidden = !autoTrash.checked;
        }
        if (autoTrashHours) {
          autoTrashHours.value = autoHours !== '' ? autoHours : '24';
        }
      }
      if (deliveryTrigger) {
        // Anything but a stored "any" (including an absent value) opens on the
        // default trigger.
        deliveryTrigger.value = btn.dataset.deliveryTrigger === 'any' ? 'any' : 'all';
      }
      if (dlg._resetTabs) {
        dlg._resetTabs();
      }
      dlg.showModal();
    });
  });

  document.querySelectorAll('.open-inbox-connector').forEach(function (connectorBtn) {
    connectorBtn.addEventListener('click', function () {
      var inboxID = connectorBtn.getAttribute('data-inbox') || '';
      var target = null;
      document.querySelectorAll('.edit-inbox').forEach(function (btn) {
        if (btn.dataset.id === inboxID) {
          target = btn;
        }
      });
      if (target) {
        target.click();
        var tab = dlg.querySelector('[data-inbox-tab=connectors]');
        if (tab) {
          tab.click();
        }
      }
    });
  });

  var extEditor = document.getElementById('inbox-external-alias-editor');
  var extAdd = document.getElementById('inbox-external-alias-add');
  var extSave = document.getElementById('external-alias-save');
  function resetExternalAliasEditor() {
    var err = document.getElementById('external-alias-error');
    if (err) {
      err.hidden = true;
      err.textContent = '';
    }
    var nameEl = document.getElementById('external-alias-name');
    var addrEl = document.getElementById('external-alias-address');
    if (nameEl) {
      nameEl.value = '';
    }
    if (addrEl) {
      addrEl.value = '';
      addrEl.disabled = false;
    }
    if (extSave) {
      extSave.textContent = 'Add external alias';
    }
    var extForm = document.getElementById('external-alias-form');
    if (extForm && editInboxID) {
      extForm.action = '/ui/inboxes/' + encodeURIComponent(editInboxID) + '/external-aliases';
    }
  }
  // openExternalAliasEditor reuses the add editor for renaming: the address is
  // immutable, so it is shown disabled and only the sender name is submitted.
  function openExternalAliasEditor(aliasID, name, address) {
    if (!extEditor) {
      return;
    }
    resetExternalAliasEditor();
    var nameEl = document.getElementById('external-alias-name');
    var addrEl = document.getElementById('external-alias-address');
    if (nameEl) {
      nameEl.value = name || '';
    }
    if (addrEl) {
      addrEl.value = address || '';
      addrEl.disabled = true;
    }
    if (extSave) {
      extSave.textContent = 'Save';
    }
    var extForm = document.getElementById('external-alias-form');
    if (extForm) {
      extForm.action = '/ui/inboxes/' + encodeURIComponent(editInboxID) + '/external-aliases/' + encodeURIComponent(aliasID) + '/edit';
    }
    extEditor.hidden = false;
    if (nameEl) {
      nameEl.focus();
    }
  }
  if (extEditor && extAdd) {
    extAdd.addEventListener('click', function () {
      resetExternalAliasEditor();
      extEditor.hidden = false;
      var nameEl = document.getElementById('external-alias-name');
      if (nameEl) {
        nameEl.focus();
      }
    });
    var extCancel = document.getElementById('external-alias-cancel');
    if (extCancel) {
      extCancel.addEventListener('click', function () {
        extEditor.hidden = true;
      });
    }
    var extFormEl = document.getElementById('external-alias-form');
    if (extFormEl) {
      extFormEl.addEventListener('submit', function () {
        inboxSubmitting = true;
      });
    }
  }
  if (editExternalList) {
    editExternalList.addEventListener('click', function (e) {
      var edit = e.target.closest('.external-alias-edit');
      if (edit) {
        e.preventDefault();
        openExternalAliasEditor(edit.dataset.alias, edit.dataset.name, edit.dataset.address);
        return;
      }
      var configure = e.target.closest('.external-alias-configure');
      if (configure) {
        e.preventDefault();
        openExternalAliasDialog(dlg, configure.dataset.alias);
        return;
      }
      var remove = e.target.closest('.external-alias-remove');
      if (remove) {
        e.preventDefault();
        deleteExternalAlias(editInboxID, remove.dataset.alias, remove.dataset.address);
      }
    });
  }

  if (display) {
    display.addEventListener('input', function () {
      editPrimaryName = display.value.trim();
      refreshEditSender();
    });
  }

  var cancel = document.getElementById('inbox-edit-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      dlg.close();
    });
  }

  // Reopen the inbox edit dialog on the section named in the URL when the
  // dashboard was loaded with an inbox to open (e.g. returning from a connector
  // or alias save). An external-alias connector flash opens its sending
  // sub-view. The shell's tab listeners are attached by a later script block, so
  // defer past script execution.
  var openCard = document.querySelector('[data-open-inbox]');
  var openInbox = openCard ? openCard.getAttribute('data-open-inbox') : '';
  var openInboxTab = openCard ? (openCard.getAttribute('data-open-inbox-tab') || 'aliases') : 'aliases';
  var openAlias = openCard ? (openCard.getAttribute('data-open-alias') || '') : '';
  if (openInbox) {
    setTimeout(function () {
      var target = null;
      document.querySelectorAll('.edit-inbox').forEach(function (btn) {
        if (btn.dataset.id === openInbox) {
          target = btn;
        }
      });
      if (target) {
        target.click();
        var tab = dlg.querySelector('[data-inbox-tab=' + openInboxTab + ']');
        if (tab) {
          tab.click();
        }
        var pending = document.querySelector('.domain-dialog[data-open="1"]');
        if (pending && !pending.open && typeof pending.showModal === 'function') {
          pending.showModal();
        }
        if (openAlias) {
          openExternalAliasDialog(dlg, openAlias);
        }
      }
    }, 0);
  }
})();

// bindInboxSettingsShell wires the shared full-height inbox settings shell used
// by both the Add and Edit dialogs: vertical section navigation, a single
// scrollable panel area, in-modal sub-views (connector editor, external-alias
// sending) and a single footer save bar. It returns nothing and is idempotent
// per dialog.
function bindInboxSettingsShell(dlg) {
  if (!dlg || dlg._inboxShellBound) {
    return;
  }
  dlg._inboxShellBound = true;
  var bar = dlg.querySelector('[data-inbox-tabs]');
  if (!bar) {
    return;
  }
  var tabs = bar.querySelectorAll('[data-inbox-tab]');

  function activate(name) {
    // Leaving a panel always drops back out of any sub-view.
    dlg.classList.remove('inbox-settings--subview');
    dlg.querySelectorAll('.inbox-subview.active').forEach(function (v) {
      v.classList.remove('active');
    });
    tabs.forEach(function (t) {
      var on = t.getAttribute('data-inbox-tab') === name;
      t.classList.toggle('active', on);
      t.setAttribute('aria-selected', on ? 'true' : 'false');
    });
    dlg.querySelectorAll('[data-inbox-panel]').forEach(function (p) {
      p.hidden = p.getAttribute('data-inbox-panel') !== name;
    });
    var inboxSave = dlg.querySelector('#inbox-edit-save');
    if (inboxSave) {
      inboxSave.hidden = false;
      inboxSave.textContent = 'Save';
      inboxSave.setAttribute('form', name === 'connectors' ? 'inbox-connectors-form' : 'inbox-edit-form');
    }
  }

  tabs.forEach(function (t) {
    t.addEventListener('click', function () {
      activate(t.getAttribute('data-inbox-tab'));
    });
  });

  dlg._resetTabs = function () {
    if (tabs.length) {
      activate(tabs[0].getAttribute('data-inbox-tab'));
    }
  };

  // A required field in a hidden panel (e.g. a provider form posted from the
  // footer) surfaces its panel rather than failing silently.
  dlg.querySelectorAll('form').forEach(function (form) {
    form.addEventListener('invalid', function (e) {
      var panel = e.target.closest ? e.target.closest('[data-inbox-panel]') : null;
      if (panel) {
        activate(panel.getAttribute('data-inbox-panel'));
      }
    }, true);
  });

  dlg.querySelectorAll('.inbox-close').forEach(function (btn) {
    btn.addEventListener('click', function () {
      dlg.close();
    });
  });
  dlg.querySelectorAll('.inbox-subview-back').forEach(function (btn) {
    btn.addEventListener('click', function () {
      hideInboxSubview(dlg);
    });
  });
}

// showInboxSubview switches the whole panels area to a sub-view (the connector
// editor or the external-alias sending editor) and marks its footer submit so
// only one Save is ever shown.
function showInboxSubview(dlg, name) {
  if (!dlg) {
    return;
  }
  dlg.querySelectorAll('.inbox-subview').forEach(function (v) {
    v.classList.toggle('active', v.getAttribute('data-inbox-subview') === name);
  });
  dlg.classList.add('inbox-settings--subview');
}

// hideInboxSubview returns from a sub-view to the current section panel.
function hideInboxSubview(dlg) {
  if (!dlg) {
    return;
  }
  dlg.classList.remove('inbox-settings--subview');
  dlg.querySelectorAll('.inbox-subview.active').forEach(function (v) {
    v.classList.remove('active');
  });
  // Clear any transient connector editor markup the sub-view held.
  var editor = dlg.querySelector('#inbox-connector-editor');
  if (editor) {
    editor.textContent = '';
  }
}

(function () {
  document.querySelectorAll('dialog.inbox-settings').forEach(bindInboxSettingsShell);
})();

(function () {
  var notices = document.querySelectorAll('.notice');
  if (!notices.length) {
    return;
  }

  function dismiss(el) {
    if (el.dataset.dismissed) {
      return;
    }
    el.dataset.dismissed = '1';
    el.classList.add('dismissing');
    window.setTimeout(function () {
      if (el.parentNode) {
        el.parentNode.removeChild(el);
      }
    }, 300);
  }

  notices.forEach(function (el) {
    var timer = window.setTimeout(function () {
      dismiss(el);
    }, 5000);
    el.addEventListener('click', function () {
      window.clearTimeout(timer);
      dismiss(el);
    });
  });

  // Drop the notice from the URL so a refresh does not show it again.
  if (window.history && window.history.replaceState && window.URLSearchParams) {
    var url = new URL(window.location.href);
    if (url.searchParams.has('notice')) {
      url.searchParams.delete('notice');
      window.history.replaceState(null, '', url.pathname + url.search + url.hash);
    }
  }
})();

(function () {
  var frame = document.querySelector('[data-mailframe]');
  var show = document.querySelector('[data-remote-img-show]');
  var banner = document.querySelector('#remote-img-banner');
  if (!frame || !show) {
    return;
  }
  // The banner is rendered server-side only when the message has remote images.
  // Clicking it reloads the frame with ?remote=1, relaxing the CSP to fetch them.
  show.addEventListener('click', function () {
    try {
      var url = new URL(frame.getAttribute('src'), window.location.origin);
      url.searchParams.set('remote', '1');
      frame.dataset.remoteOptIn = '1';
      frame.src = url.toString();
      if (banner) {
        banner.hidden = true;
      }
    } catch (e) {
      // ignore
    }
  });
})();

(function () {
  var btn = document.getElementById('cf-copy');
  if (!btn) {
    return;
  }
  var code = document.getElementById('cf-code');
  var note = document.getElementById('cf-copy-note');
  btn.addEventListener('click', function () {
    var text = code ? code.textContent : '';
    if (window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function () {
        btn.textContent = 'Copied!';
        setTimeout(function () {
          btn.textContent = 'Copy code';
        }, 1500);
      }).catch(function () {
        if (note) {
          note.hidden = false;
        }
      });
      return;
    }
    if (note) {
      note.hidden = false;
    }
  });
})();

(function () {
  var dlg = document.getElementById('cf-setup-dialog');
  if (!dlg) {
    return;
  }
  var next = document.getElementById('cf-next');
  var done = document.getElementById('cf-done');
  if (next) {
    next.addEventListener('click', function () {
      var worker = document.getElementById('cf-worker-step');
      var routing = document.getElementById('cf-routing-step');
      if (worker) {
        worker.hidden = true;
      }
      if (routing) {
        routing.hidden = false;
      }
      dlg.scrollTop = 0;
    });
  }
  if (done) {
    done.addEventListener('click', function () {
      dlg.close();
    });
  }
})();

(function () {
  document.querySelectorAll('.setup-copy').forEach(function (btn) {
    var dlg = btn.closest('dialog') || document;
    // Each provider group owns its own webhook URL and note. Resolve them from
    // the button's group so a copy never picks up another provider's URL.
    var scope = btn.closest('.provider-fields') || dlg;
    var url = scope.querySelector('.setup-webhook-url');
    var note = scope.querySelector('.setup-copy-note');
    btn.addEventListener('click', function () {
      var text = url ? url.textContent : '';
      if (window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () {
          btn.textContent = 'Copied!';
          setTimeout(function () {
            btn.textContent = 'Copy webhook URL';
          }, 1500);
        }).catch(function () {
          if (note) {
            note.hidden = false;
          }
        });
        return;
      }
      if (note) {
        note.hidden = false;
      }
    });
  });
})();

(function () {
  document.querySelectorAll('.open-domain-dialog').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var dlg = document.getElementById('domain-' + btn.getAttribute('data-kind') + '-dialog-' + btn.getAttribute('data-domain'));
      if (dlg && !dlg.open) {
        dlg.showModal();
      }
    });
  });
  // Pending dialogs auto-open, unless inbox settings are also reopening: the
  // inbox-edit flow then suspends first and shows the pending dialog itself,
  // so cancelling it resumes settings instead of dropping to the dashboard.
  // The inbox script block runs before this one and defers via setTimeout, so
  // check for a pending inbox reopen synchronously here.
  var inboxReopening = false;
  var openCardEl = document.querySelector('[data-open-inbox]');
  if (openCardEl && openCardEl.getAttribute('data-open-inbox')) {
    inboxReopening = true;
  }
  document.querySelectorAll('.domain-dialog[data-open="1"], .cf-setup-dialog[data-open="1"]').forEach(function (dlg) {
    if (inboxReopening) {
      return;
    }
    if (!dlg.open) {
      dlg.showModal();
    }
  });
  document.querySelectorAll('.domain-dialog [data-close-dialog]').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var dlg = btn.closest('dialog');
      if (dlg) {
        dlg.close();
      }
    });
  });
  // A genuine dismissal of a URL-opened domain or Cloudflare dialog drops the
  // params that reopened it, so a refresh does not bring it back. The
  // external-alias sending dialogs are excluded here: their close handler lives
  // in the inbox settings block, which also restores the suspended inbox.
  document.querySelectorAll('.domain-dialog[id^="domain-"], .cf-setup-dialog').forEach(function (dlg) {
    dlg.addEventListener('close', function () {
      clearUrlParams(['domain', 'kind', 'provider']);
    });
  });
})();

(function () {
  // syncDialMXService materialises or removes the editable Receiver URLs field
  // for a Dial MX provider group. The field is only valid for the custom
  // service, so for Antler MX it is kept out of the DOM; switching the service
  // select to custom injects the field from its inert <template>. The server
  // independently rejects a supplied receiver_urls for Antler.
  function syncDialMXService(group) {
    if (!group || group.getAttribute('data-provider') !== 'dialmx') {
      return;
    }
    var service = group.querySelector('select[name="cfg_dialmx_service"]');
    if (!service) {
      return;
    }
    var tpl = group.querySelector('template.dialmx-receiver-urls-field');
    var injected = group.querySelector('.dialmx-receiver-urls');
    if (service.value === 'custom') {
      if (!injected && tpl) {
        group.insertBefore(tpl.content.cloneNode(true), tpl);
      }
    } else if (injected) {
      injected.parentNode.removeChild(injected);
    }
  }
  function sync(dlg) {
    var sel = dlg.querySelector('.provider-select');
    if (!sel) {
      return;
    }
    var value = sel.value;
    var mxPanel = dlg.querySelector('[data-mx-panel]');
    if (mxPanel) {
      mxPanel.hidden = value !== 'mx';
      mxPanel.querySelectorAll('input,select,textarea').forEach(function (input) {
        input.disabled = value !== 'mx';
      });
    }
    // The Remote MX panel carries its own account-level form (a separate
    // endpoint), so it is only shown/hidden with the provider choice; its inputs
    // stay enabled so an account admin can configure the receiver from here.
    var remoteMXPanel = dlg.querySelector('[data-remote-mx-panel]');
    if (remoteMXPanel) {
      remoteMXPanel.hidden = value !== 'remotemx';
    }
    dlg.querySelectorAll('.provider-fields').forEach(function (group) {
      var active = group.getAttribute('data-provider') === value;
      group.hidden = !active;
      group.querySelectorAll('input,select,textarea').forEach(function (el) {
        el.disabled = !active;
      });
      if (active) {
        syncDialMXService(group);
      }
    });
    var save = dlg.querySelector('[data-save-provider]');
    if (save) {
      save.disabled = !value;
      var opt = sel.options[sel.selectedIndex];
      save.textContent = opt && opt.getAttribute('data-next') ? 'Next' : 'Save';
    }
    var hint = dlg.querySelector('.provider-hint');
    if (hint) {
      hint.hidden = !!value;
    }
  }
  document.querySelectorAll('.domain-dialog').forEach(function (dlg) {
    var sel = dlg.querySelector('.provider-select');
    if (!sel) {
      return;
    }
    sel.addEventListener('change', function () {
      sync(dlg);
    });
    dlg.querySelectorAll('select[name="cfg_dialmx_service"]').forEach(function (service) {
      service.addEventListener('change', function () {
        syncDialMXService(service.closest('.provider-fields'));
      });
    });
    sync(dlg);
  });
})();

(function () {
  document.querySelectorAll('form[data-antler-domain]').forEach(function (form) {
    var dlg = form.closest('dialog');
    var provider = form.querySelector('.provider-select');
    var group = form.querySelector('[data-provider="dialmx"]');
    if (!group) { return; }
    var service = group.querySelector('[name="cfg_dialmx_service"]');
    var email = group.querySelector('[name="cfg_dialmx_contact_email"]');
    var enforcement = group.querySelector('[name="cfg_dialmx_enforcement"]');
    var save = dlg.querySelector('[data-save-provider]');
    var oldSetup = dlg.querySelector('.dialmx-setup');
    var statusMode = !!oldSetup && provider.value === 'dialmx';
    var entered = statusMode;
    var custom = service.value === 'custom';
    // The saved status view draws its connector skeleton from the names embedded
    // on the form, so it opens complete and the first poll only fills values.
    var connectors = [];
    try { connectors = JSON.parse(form.dataset.antlerConnectors || '[]') || []; } catch (err) { connectors = []; }
    var providerLabel = provider.previousElementSibling;
    var serviceLabel = service.previousElementSibling;
    var danger = dlg.querySelector('.dialog-danger');
    var rotate = dlg.querySelector('form[data-antler-regenerate]');
    if (statusMode && danger) {
      // Keep key rotation and removing the receiving config; drop the
      // provider-specific Worker-secret regenerate, which is not part of an
      // Antler MX setup.
      Array.prototype.forEach.call(danger.children, function (child) {
        var button = child.querySelector('button');
        var remove = button && button.className.indexOf('danger') !== -1;
        child.hidden = child !== rotate && !remove;
      });
    }
    var close = dlg.querySelector('[data-close-dialog]');
    if (statusMode && close) { close.textContent = 'Close'; }
    var endpoint = '/ui/domains/' + encodeURIComponent(form.dataset.antlerDomain) + '/receiving/setup';
    var step = 0, busy = false, ready = false, nextCheck = 0, manualUntil = 0, rotating = false, repairing = false, fixKind = '';
    var state = null, savedEmail = null, timer = null;
    var wizard = document.createElement('section');
    wizard.className = 'antler-wizard';
    wizard.innerHTML = '<p class="antler-progress" aria-live="polite"></p>' +
      '<section data-antler-step="0"><h3>Contact email</h3><p class="muted small">Enter a contact email for your Antler MX setup.</p></section>' +
      '<section data-antler-step="1" hidden><h3>Publish your DNS records</h3><p class="muted small">Add these records at your DNS provider. You can continue while DNS propagates.</p><div class="antler-records"></div></section>' +
      '<section data-antler-step="2" hidden><h3>Antler receivers</h3><p class="muted small">One ready receiver is enough to continue. Other receivers can connect later.</p><div class="antler-receivers"></div></section>' +
      '<section data-antler-step="3" hidden><h3>Authentication enforcement</h3><p class="muted small">Moderate marks mail as spam when DMARC fails, or both SPF and DKIM fail. Hard marks mail as spam when any of SPF, DKIM, or DMARC fails. Missing or inconclusive results alone do not count as failures.</p></section>' +
      '<section data-antler-step="4" hidden><h3>Custom receivers</h3><p class="muted small">Enter comma-separated HTTPS receiver URLs. Your receiver operator must also provide the SMTP hostname to use in your MX records.</p><label>Receiver URLs</label><input class="antler-custom-urls" placeholder="https://mx.example.com"></section>' +
      '<div class="antler-checks" hidden><p class="antler-check-note" role="status"></p><button type="button" class="secondary antler-check">Check now</button></div>' +
      '<p class="antler-error error" role="alert" hidden></p>';
    group.appendChild(wizard);
    function moveField(input, index) {
      var label = input.previousElementSibling;
      var panel = wizard.querySelector('[data-antler-step="' + index + '"]');
      if (label && label.tagName === 'LABEL') { panel.appendChild(label); }
      panel.appendChild(input);
    }
    moveField(email, 0);
    moveField(enforcement, 3);
    var advancedLabel = document.createElement('label');
    var advanced = document.createElement('input');
    advanced.type = 'checkbox'; advanced.checked = custom;
    advanced.className = 'antler-advanced';
    advanced.style = 'width:auto;margin-right:8px';
    advancedLabel.appendChild(advanced);
    advancedLabel.appendChild(document.createTextNode('Advanced: use custom receivers'));
    wizard.querySelector('[data-antler-step="0"]').appendChild(advancedLabel);
    var urls = wizard.querySelector('.antler-custom-urls');
    var existingURLs = group.querySelector('[name="cfg_dialmx_receiver_urls"]');
    urls.value = existingURLs ? existingURLs.value : '';
    advanced.addEventListener('change', function () { custom = advanced.checked; update(); });
    var panels = wizard.querySelectorAll('[data-antler-step]');
    var checksRow = wizard.querySelector('.antler-checks');
    var check = wizard.querySelector('.antler-check');
    var note = wizard.querySelector('.antler-check-note');
    var error = wizard.querySelector('.antler-error');
    // Fix lives beside Check now, right-aligned, and acts on the current issue.
    var fix = document.createElement('button');
    fix.type = 'button'; fix.className = 'secondary antler-fix'; fix.hidden = true;
    checksRow.appendChild(fix);
    // Back is part of the dialog's bottom navigation, not the upper form.
    var footer = save.closest('.dialog-actions');
    var back = document.createElement('button');
    back.type = 'button'; back.className = 'secondary antler-back'; back.textContent = 'Back'; back.hidden = true;
    if (footer) { footer.insertBefore(back, footer.firstChild); }
    function active() { return provider.value === 'dialmx'; }
    function fail(message) { error.textContent = message; error.hidden = !message; }
    function api(method, config, regenerate) {
      var options = { method: method, cache: 'no-store', headers: { 'Accept': 'application/json' } };
      if (config || regenerate) {
        options.headers['Content-Type'] = 'application/json';
        options.headers['X-CSRF-Token'] = form.querySelector('[name="_csrf"]').value;
        options.body = JSON.stringify(regenerate ? { provider: 'dialmx', regenerate_secret: true } : { provider: 'dialmx', config: config });
      }
      return fetch(endpoint, options).then(function (response) {
        if (!response.ok) { throw new Error('Unable to ' + (method === 'GET' ? 'check' : 'save') + ' setup. Please try again.'); }
        return response.json();
      });
    }
    function isReady(status) {
      return status.state === 'ready' && (!status.expires_at || Date.parse(status.expires_at) > Date.now());
    }
    // The status view only offers Save email once the contact email actually
    // differs from the saved one; otherwise Close is the only action.
    function emailDirty() {
      var saved = state && state.config ? state.config.contact_email || '' : '';
      return email.value.trim() !== saved;
    }
    // A receiver row reads its real state: red is a fact about the receiver (it
    // refused, it is gone, or the core cannot reach it at all), amber is only
    // ever "in progress" — an authorization or a physical attempt still running.
    function receiverLightClass(status) {
      if (isReady(status)) { return 'green'; }
      return ['connecting', 'deferred'].indexOf(status.state) !== -1 ? 'amber' : 'red';
    }
    function receiverLabel(status) {
      if (isReady(status)) { return 'Ready to receive'; }
      return {
        connecting: 'Authorizing',
        disconnected: 'Reconnecting',
        rejected: 'Rejected',
        unavailable: 'Unavailable',
        unreachable: 'Receiver unreachable',
        deferred: 'Waiting for capacity'
      }[status.state] || 'Waiting';
    }
    // The saved status view draws its full connector table before the first
    // poll returns, so the dialog no longer opens half-empty and fills later.
    // The connector names are the only thing not known from live status, so they
    // are embedded on the form (data-antler-connectors); every value cell starts
    // pending and is filled by the first render. While a check is in flight the
    // amber pending dots animate (see .antler-checking), which also covers every
    // wizard step's own pending rows.
    function skeletonLight(text) {
      var wrap = document.createElement('span');
      var dot = document.createElement('span');
      dot.className = 'antler-light amber';
      var label = document.createElement('span');
      label.textContent = text;
      wrap.appendChild(dot);
      wrap.appendChild(label);
      return wrap;
    }
    function skeletonCell(parent, text) {
      var td = document.createElement('td');
      td.appendChild(skeletonLight(text));
      parent.appendChild(td);
      return td;
    }
    function renderSkeleton(connectors) {
      var records = wizard.querySelector('.antler-records');
      records.replaceChildren();
      var table = document.createElement('table');
      table.className = 'antler-dns-table antler-status-table';
      var head = document.createElement('tr');
      ['Connector', 'Connection status', 'MX status'].forEach(function (title) {
        var th = document.createElement('th');
        th.textContent = title;
        head.appendChild(th);
      });
      var thead = document.createElement('thead'); thead.appendChild(head); table.appendChild(thead);
      var tbody = document.createElement('tbody');
      if (connectors.length) {
        connectors.forEach(function (mx) {
          var tr = document.createElement('tr');
          var connector = document.createElement('td');
          connector.textContent = mx.hostname;
          tr.appendChild(connector);
          skeletonCell(tr, 'Pending');
          skeletonCell(tr, 'Pending');
          tbody.appendChild(tr);
        });
      } else {
        var waiting = document.createElement('tr');
        var cell = document.createElement('td');
        cell.colSpan = 3;
        cell.textContent = 'No connectors configured';
        waiting.appendChild(cell);
        tbody.appendChild(waiting);
      }
      table.appendChild(tbody);
      records.appendChild(table);
      var auth = document.createElement('div');
      auth.className = 'antler-domain-auth';
      var title = document.createElement('strong');
      title.textContent = 'Domain authentication — TXT record';
      auth.appendChild(title);
      auth.appendChild(skeletonLight('Pending'));
      records.appendChild(auth);
    }
    function render(data) {
      state = data;
      note.dataset.updated = new Date().toLocaleTimeString();
      var instructions = data.instructions || {};
      ready = (data.status || []).some(isReady);
      var records = wizard.querySelector('.antler-records');
      records.replaceChildren();
      var domainName = form.dataset.antlerDomainName || '';
      var subdomain = !!form.dataset.antlerParent;
      // For a subdomain, spell out the record name so it is unambiguous. The
      // apex is the common case and reads best as the provider's "@".
      var mxName = subdomain ? domainName : '@';
      var dnsByKind = {};
      (data.dns || []).forEach(function (entry) { dnsByKind[entry.kind] = entry; });
      function lightFor(entry) {
        var span = document.createElement('span');
        span.className = 'antler-light ' + (!entry || entry.state === 'pending' ? 'amber' : entry.state === 'ok' ? 'green' : 'red');
        var text = document.createElement('span');
        text.textContent = !entry ? 'Waiting' : entry.state === 'ok' ? 'Matching' : entry.state === 'mismatch' ? 'Mismatch' : 'Pending';
        span.title = (entry && entry.reason) || '';
        return { span: span, text: text };
      }
      // One receiver's MX record is valid whenever that receiver's hostname is
      // among the published MX records, independent of the other receivers. The
      // aggregate MX check is "ok" once any one is published, so a secondary
      // receiver that is not pointed at must not paint every row red.
      function mxLightFor(mx, entry) {
        var span = document.createElement('span');
        var text = document.createElement('span');
        var matched = (entry && entry.matched ? entry.matched : []).map(function (host) { return String(host).toLowerCase(); });
        var mine = String(mx.hostname).toLowerCase();
        if (matched.indexOf(mine) !== -1) {
          span.className = 'antler-light green';
          text.textContent = 'Published';
        } else {
          span.className = 'antler-light amber';
          text.textContent = !entry || entry.state === 'pending' ? 'Pending' : 'Not published';
        }
        span.title = (entry && entry.reason) || '';
        return { span: span, text: text };
      }
      var table = document.createElement('table');
      table.className = 'antler-dns-table ' + (statusMode ? 'antler-status-table' : 'antler-record-table');
      var head = document.createElement('tr');
      (statusMode ? ['Connector', 'Connection status', 'MX status'] : ['Name', 'Type', 'Priority', 'Value', 'Status', '']).forEach(function (title) {
        var th = document.createElement('th');
        th.textContent = title;
        head.appendChild(th);
      });
      var thead = document.createElement('thead'); thead.appendChild(head); table.appendChild(thead);
      var tbody = document.createElement('tbody');
      function statusCell(parent, light) {
        var td = document.createElement('td');
        td.appendChild(light.span);
        td.appendChild(light.text);
        parent.appendChild(td);
        return td;
      }
      if (!statusMode) {
        function recordRow(name, type, priority, value, entry, mxHost) {
          var tr = document.createElement('tr');
          tr.dataset.dnsKind = type.toLowerCase();
          [name, type, priority, value].forEach(function (text, index) {
            var td = document.createElement('td');
            td.textContent = text;
            if (index === 0) { td.className = 'antler-name'; }
            if (index === 3) { td.className = 'antler-value'; }
            tr.appendChild(td);
          });
          statusCell(tr, mxHost ? mxLightFor({ hostname: mxHost }, entry) : lightFor(entry));
          var action = document.createElement('td');
          var copy = document.createElement('button');
          copy.type = 'button'; copy.className = 'secondary'; copy.textContent = 'Copy value';
          copy.addEventListener('click', function () {
            if (!navigator.clipboard) { fail('Select the record value and copy it manually. Clipboard access needs HTTPS.'); return; }
            navigator.clipboard.writeText(value).then(function () { copy.textContent = 'Copied!'; }).catch(function () { fail('Select the record value and copy it manually.'); });
          });
          action.appendChild(copy); tr.appendChild(action); tbody.appendChild(tr);
        }
        (instructions.mx || []).forEach(function (mx) { recordRow(mxName, 'MX', String(mx.priority), mx.hostname, dnsByKind.mx, mx.hostname); });
        if (instructions.txt_value) { recordRow(instructions.txt_name, 'TXT', '—', instructions.txt_value, dnsByKind.txt); }
      } else {
        (instructions.mx || []).forEach(function (mx) {
          var tr = document.createElement('tr');
          var connector = document.createElement('td');
          connector.textContent = mx.hostname;
          tr.appendChild(connector);
          var connection = (data.status || []).filter(function (status) {
            return status.smtp_hostname === mx.hostname;
          })[0];
          var connectionLight = connection ? {
            span: Object.assign(document.createElement('span'), { className: 'antler-light ' + receiverLightClass(connection) }),
            text: document.createElement('span')
          } : lightFor(null);
          connectionLight.text.textContent = connection ? receiverLabel(connection) : 'Waiting';
          if (connection && connection.reason) { connectionLight.text.title = connection.reason; }
          statusCell(tr, connectionLight);
          statusCell(tr, mxLightFor(mx, dnsByKind.mx));
          tbody.appendChild(tr);
        });
        if (!(instructions.mx || []).length) {
          var waiting = document.createElement('tr');
          var waitingCell = document.createElement('td');
          waitingCell.colSpan = 3;
          waitingCell.textContent = 'No connectors configured';
          waiting.appendChild(waitingCell);
          tbody.appendChild(waiting);
        }
      }
      table.appendChild(tbody);
      var txtEntry = dnsByKind.txt;
      var auth = document.createElement('div');
      auth.className = 'antler-domain-auth';
      var authTitle = document.createElement('strong');
      authTitle.textContent = 'Domain authentication — TXT record';
      auth.appendChild(authTitle);
      var authStatus = document.createElement('span');
      var txtLight = lightFor(txtEntry);
      authStatus.appendChild(txtLight.span);
      authStatus.appendChild(txtLight.text);
      auth.appendChild(authStatus);
      records.appendChild(table);
      if (statusMode) { records.appendChild(auth); }
      // Fix is only offered for a DNS record the operator must correct. A
      // receiver that is connecting, reconnecting or waiting for capacity needs
      // no action: the relay reconnects on its own, and the status table already
      // shows it as in progress, so there is nothing to fix.
      fix.hidden = true;
      if (statusMode) {
        // A partial MX set is healthy: the aggregate check is "ok" once any one
        // receiver's MX is published, so the MX prompt only appears when no
        // connector has any MX (the check is pending or mismatched).
        var dnsIssue = !txtEntry || txtEntry.state !== 'ok' ? 'txt' : (instructions.mx || []).length && (!dnsByKind.mx || dnsByKind.mx.state !== 'ok') ? 'mx' : '';
        if (dnsIssue) { fixKind = dnsIssue; fix.textContent = 'Fix DNS records'; fix.hidden = false; }
      }
      var receivers = wizard.querySelector('.antler-receivers'); receivers.replaceChildren();
      (data.status || []).forEach(function (status) {
        var row = document.createElement('p');
        row.textContent = (status.smtp_hostname || status.receiver_url) + ' — ' + receiverLabel(status) + (status.reason ? ' · ' + status.reason : '');
        var light = document.createElement('span');
        light.className = 'antler-light ' + receiverLightClass(status);
        row.prepend(light);
        receivers.appendChild(row);
      });
      if (!(data.status || []).length) { receivers.textContent = 'Waiting for receiver status…'; }
      update();
    }
    function update() {
      var enabled = active();
      dlg.classList.toggle('antler-dialog', enabled);
      if (rotate) { rotate.querySelector('button').disabled = busy || rotating || repairing; }
      fix.disabled = busy;
      provider.disabled = busy;
      service.disabled = busy || provider.value !== 'dialmx';
      provider.hidden = enabled && entered;
      if (providerLabel) { providerLabel.hidden = provider.hidden; }
      service.hidden = true;
      if (serviceLabel) { serviceLabel.hidden = true; }
      wizard.hidden = provider.value !== 'dialmx';
      // Keep the generic custom-service controls available outside the wizard.
      Array.prototype.forEach.call(group.children, function (child) {
        if (child !== wizard && child.tagName !== 'TEMPLATE') { child.hidden = enabled; }
      });
      email.type = enabled ? 'email' : 'text';
      email.required = enabled && !custom;
      email.disabled = !enabled || (custom && step !== 0);
      advanced.disabled = !enabled || busy;
      urls.disabled = !enabled;
      if (oldSetup) { oldSetup.hidden = enabled || provider.value !== 'dialmx'; }
      if (!enabled) {
        // The moved fields still belong to the custom form.
        wizard.hidden = provider.value !== 'dialmx';
        panels.forEach(function (panel, index) { panel.hidden = index !== 0 && index !== 3; });
        wizard.querySelector('.antler-progress').hidden = true;
        back.hidden = true;
        wizard.querySelector('.antler-checks').hidden = true;
        wizard.querySelectorAll('h3, [data-antler-step] > p').forEach(function (el) { el.hidden = true; });
        if (provider.value === 'dialmx') { save.textContent = 'Save'; save.disabled = false; }
        return;
      }
      wizard.querySelectorAll('h3, [data-antler-step] > p').forEach(function (el) { el.hidden = false; });
      wizard.querySelector('.antler-progress').hidden = statusMode;
      wizard.querySelector('.antler-progress').textContent = repairing ? 'Fix DNS records' : rotating ? 'New receiving key · DNS → Receivers' : custom ? 'Custom Antler MX · Contact → Receiver URLs → DNS → Receivers → Enforcement' : 'Antler MX · Contact → DNS → Receivers → Enforcement';
      wizard.querySelector('[data-antler-step="1"] h3').textContent = repairing ? 'Fix your ' + fixKind.toUpperCase() + ' record' : rotating ? 'Replace your authorization TXT record' : statusMode ? 'Antler MX status' : 'Publish your DNS records';
      wizard.querySelector('[data-antler-step="1"] > p').textContent = repairing ? 'Update this record at your DNS provider, then choose Done. The core re-checks automatically once DNS propagates.' : rotating ? 'The previous key is invalid. Replace the existing authorization TXT record with the new value below. Your MX records stay the same. Receiving can resume once DNS propagates and a receiver reconnects.' : 'Add these records at your DNS provider. You can continue while DNS propagates.';
      // The saved-status view replaces the separate receiver panel with a
      // combined connection, MX, and domain-authentication status table.
      panels.forEach(function (panel, index) {
        panel.hidden = statusMode ? (index === 2 || index === 3 || index === 4 || (index === 0 && custom)) : index !== step;
      });
      advancedLabel.hidden = statusMode;
      if (statusMode) {
        wizard.querySelector('[data-antler-step="0"] h3').hidden = true;
        wizard.querySelector('[data-antler-step="0"] p').hidden = true;
        wizard.querySelector('[data-antler-step="1"] h3').textContent = 'Antler MX status';
        wizard.querySelector('[data-antler-step="2"] p').hidden = true;
      }
      if (statusMode) { wizard.querySelector('[data-antler-step="1"] > p').hidden = true; }
      // Repair can always return to status; rotation's new-TXT step cannot go
      // back to the pre-rotation state, but its receiver step can.
      back.hidden = statusMode || (step === 0 && !entered) || (rotating && step === 1);
      back.disabled = busy;
      wizard.querySelector('.antler-checks').hidden = !statusMode && step !== 1 && step !== 2;
      save.hidden = statusMode && (custom || !emailDirty());
      save.textContent = busy ? 'Please wait…' : statusMode ? 'Save email' : repairing ? 'Done' : (rotating && step === 2) || step === 3 ? 'Finish' : 'Next';
      if (state) { ready = (state.status || []).some(isReady); }
      // Rotation finishes regardless of readiness: the receiver re-authorizes on
      // its own and the status view shows it as in progress.
      save.disabled = busy || (!statusMode && !rotating && (step === 2 || step === 3) && !ready);
      check.disabled = busy || Date.now() < manualUntil;
      check.textContent = Date.now() < manualUntil ? 'Check now (' + Math.ceil((manualUntil - Date.now()) / 1000) + 's)' : 'Check now';
      // While a check is in flight, pending amber dots animate as spinners so a
      // not-yet-resolved connector or DNS record reads as "in progress" rather
      // than a settled amber state.
      wizard.querySelector('.antler-records').classList.toggle('antler-checking', busy);
      wizard.querySelector('.antler-receivers').classList.toggle('antler-checking', busy);
    }
    function refresh(manual) {
      if (busy || !active() || !dlg.open || (!statusMode && (step === 0 || step === 3 || step === 4))) { return; }
      busy = true;
      if (manual) { manualUntil = Date.now() + 3000; }
      note.textContent = 'Checking…'; note.setAttribute('aria-busy', 'true'); update();
      api('GET').then(function (data) { render(data); fail(''); }).catch(function (err) { fail(err.message); }).finally(function () {
        busy = false; nextCheck = Date.now() + 10000;
        note.removeAttribute('aria-busy'); update();
      });
    }
    check.addEventListener('click', function () { if (Date.now() >= manualUntil) { refresh(true); } });
    fix.addEventListener('click', function () {
      if (busy) { return; }
      repairing = true; statusMode = false; entered = true; step = 1;
      fail(''); render(state); nextCheck = Date.now();
    });
    email.addEventListener('input', update);
    if (rotate) {
      rotate.addEventListener('submit', function (event) {
        if (!active()) { return; }
        var cancelled = event.defaultPrevented;
        event.preventDefault();
        if (cancelled) { return; }
        if (busy || rotating) { return; }
        // The shared data-confirm handler runs first. Cancellation must never
        // reach the rotation endpoint.
        busy = true; fail(''); update();
        api('PUT', null, true).then(function (data) {
          rotating = true; statusMode = false; entered = true; step = 1;
          state = null; ready = false;
          render(data); nextCheck = Date.now() + 10000;
        }).catch(function () {
          fail('Unable to confirm key rotation. Check the current DNS instructions before trying again; the key may already have changed.');
          nextCheck = Date.now();
        }).finally(function () { busy = false; update(); });
      });
    }
    back.addEventListener('click', function () {
      if (repairing) { repairing = false; statusMode = true; render(state); fail(''); return; }
      if (step === 0) { entered = false; }
      else if (step === 4) { step = 0; }
      else if (step === 1 && custom) { step = 4; }
      else { step = Math.max(0, step - 1); }
      fail(''); update();
    });
    form.addEventListener('submit', function (event) {
      if (!active()) { return; }
      event.preventDefault();
      if (busy || (!statusMode && !rotating && (step === 2 || step === 3) && !ready)) { return; }
      // Repair is a single DNS step: Done re-checks and returns to the status view.
      if (repairing) {
        busy = true; update();
        api('GET').then(function (data) {
          repairing = false; statusMode = true; render(data); fail('');
        }).catch(function (err) { fail(err.message); }).finally(function () { busy = false; update(); });
        return;
      }
      // Rotation finishes regardless of readiness; the receiver re-authorizes on
      // its own and the status view reports it as in progress.
      if (rotating && step === 2) {
        busy = true; update();
        api('GET').then(function (data) {
          rotating = false; statusMode = true; render(data); fail('');
        }).catch(function (err) { fail(err.message); }).finally(function () { busy = false; update(); });
        return;
      }
      if (statusMode) {
        if (custom || !emailDirty() || !email.reportValidity()) { return; }
        busy = true; update();
        api('POST', { contact_email: email.value }).then(function (data) { render(data); fail(''); })
          .catch(function (err) { fail(err.message); }).finally(function () { busy = false; update(); });
        return;
      }
      entered = true;
      if (step === 0 && custom) { step = 4; update(); return; }
      if (step === 1 || step === 2) { step++; update(); if (step === 2) { refresh(false); } return; }
      if (!custom && !email.reportValidity()) { return; }
      if (step === 4 && !urls.value.trim()) { fail('Enter at least one receiver URL.'); return; }
      if (step === 0 && savedEmail === email.value && state) { step = 1; update(); return; }
      busy = true; fail(''); update();
      var finishing = step === 3;
      var beforeSave = finishing ? api('GET').then(function (data) {
        render(data);
        if (!ready) { step = 2; throw new Error('No Antler receiver is ready now. Wait for one receiver to reconnect before finishing.'); }
      }) : api('GET').then(function (data) {
        // Reopening an existing setup must retain its enforcement until the
        // final step, and must not unnecessarily resolve a new receiver set.
        if (data.provider === 'dialmx' && data.config.service === (custom ? 'custom' : 'antler')) { state = data; }
      });
      beforeSave.then(function () {
        if (!finishing && !custom && state && state.config.contact_email === email.value) { return state; }
        var config = { service: custom ? 'custom' : 'antler', enforcement: finishing ? enforcement.value : (state && state.config.enforcement || 'moderate') };
        if (custom) { config.receiver_urls = urls.value; } else { config.contact_email = email.value; }
        return api('PUT', config);
      }).then(function (data) {
        if (!finishing) { enforcement.value = data.config.enforcement || 'moderate'; }
        savedEmail = email.value; render(data);
        if (finishing) { statusMode = true; render(data); fail(''); }
        else { step = 1; nextCheck = Date.now(); }
      }).catch(function (err) { fail(err.message); }).finally(function () { busy = false; update(); });
    });
    provider.addEventListener('change', update);
    service.addEventListener('change', update);
    // Poll only while this dialog is open; no background checks after dismissal.
    timer = window.setInterval(function () {
      if (!active() || !dlg.open) { return; }
      update();
      if ((statusMode || step === 1 || step === 2) && !busy) {
        if (Date.now() >= nextCheck) { refresh(false); }
        else { note.textContent = 'Refresh in ' + Math.ceil((nextCheck - Date.now()) / 1000) + 's' + (note.dataset.updated ? ' · Last checked ' + note.dataset.updated : ''); }
      }
    }, 1000);
    window.addEventListener('pagehide', function () { window.clearInterval(timer); });
    // A status dialog the server marked to open draws its complete connector
    // table synchronously here, before the browser paints the open dialog, so it
    // never flashes thin and then expands when the first poll lands. The auto-
    // open runs in an earlier script block, so this must render eagerly rather
    // than wait for the (queued) open event.
    if (statusMode && !state) { renderSkeleton(connectors); }
    update();
    if (statusMode && dlg.open) { refresh(true); }
    // Reopening a status or wizard dialog later re-checks immediately.
    dlg.addEventListener('open', function () {
      if (active() && !busy && (statusMode || step === 1 || step === 2)) { refresh(false); }
    });
  });
})();

(function () {
  var input = document.querySelector('input[type=file][name=attachments]');
  if (!input || typeof DataTransfer === 'undefined') {
    return;
  }
  var dropZone = document.getElementById('attach-drop');
  var overlay = document.getElementById('attach-overlay');
  var list = document.getElementById('attach-list');
  var dragDepth = 0;

  function files() {
    return Array.prototype.slice.call(input.files || []);
  }

  function render() {
    if (!list) {
      return;
    }
    list.textContent = '';
    files().forEach(function (file, index) {
      var li = document.createElement('li');

      var name = document.createElement('span');
      name.className = 'attach-name';
      name.textContent = file.name;

      var size = document.createElement('span');
      size.className = 'attach-size';
      size.textContent = humanSize(file.size);

      var remove = document.createElement('button');
      remove.type = 'button';
      remove.className = 'attach-remove';
      remove.setAttribute('aria-label', 'Remove ' + file.name);
      remove.textContent = '\u00d7';
      remove.addEventListener('click', function () {
        var next = files();
        next.splice(index, 1);
        setFiles(next);
      });

      li.appendChild(name);
      li.appendChild(size);
      li.appendChild(remove);
      list.appendChild(li);
    });
  }

  function setFiles(next) {
    var dt = new DataTransfer();
    next.forEach(function (file) {
      dt.items.add(file);
    });
    try {
      input.files = dt.files;
    } catch (err) {
      return;
    }
    render();
  }

  function addFiles(incoming) {
    var merged = files();
    Array.prototype.slice.call(incoming).forEach(function (file) {
      merged.push(file);
    });
    setFiles(merged);
  }

  function humanSize(bytes) {
    if (bytes < 1024) {
      return bytes + ' B';
    }
    var units = ['KB', 'MB', 'GB'];
    var value = bytes / 1024;
    var i = 0;
    while (value >= 1024 && i < units.length - 1) {
      value /= 1024;
      i += 1;
    }
    return (value >= 10 ? Math.round(value) : value.toFixed(1)) + ' ' + units[i];
  }

  function hasFiles(e) {
    var dt = e.dataTransfer;
    if (!dt) {
      return false;
    }
    if (dt.types) {
      return Array.prototype.indexOf.call(dt.types, 'Files') !== -1;
    }
    return !!(dt.files && dt.files.length);
  }

  function showDrop(show) {
    if (overlay) {
      overlay.hidden = !show;
      overlay.classList.toggle('active', show);
    }
    if (dropZone) {
      dropZone.classList.toggle('dragover', show);
    }
  }

  input.addEventListener('change', render);

  window.addEventListener('dragenter', function (e) {
    if (!hasFiles(e)) {
      return;
    }
    e.preventDefault();
    dragDepth += 1;
    showDrop(true);
  });

  window.addEventListener('dragover', function (e) {
    if (!hasFiles(e)) {
      return;
    }
    e.preventDefault();
    if (e.dataTransfer) {
      e.dataTransfer.dropEffect = 'copy';
    }
  });

  window.addEventListener('dragleave', function () {
    if (dragDepth <= 0) {
      return;
    }
    dragDepth -= 1;
    if (dragDepth === 0) {
      showDrop(false);
    }
  });

  window.addEventListener('drop', function (e) {
    if (!hasFiles(e)) {
      return;
    }
    e.preventDefault();
    dragDepth = 0;
    showDrop(false);
    if (e.dataTransfer) {
      addFiles(e.dataTransfer.files);
    }
  });

  render();
})();

// Operator invite dialog (account page): one dialog serves both creating an
// invitation and editing an existing operator's mailbox access.
(function () {
  var dlg = document.getElementById('operator-invite-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var email = dlg.querySelector('[name=email]');
  var submit = document.getElementById('operator-invite-submit');
  var title = document.getElementById('operator-invite-title');
  var boxes = dlg.querySelectorAll('input[name=inboxes]');
  var add = document.getElementById('add-operator');
  if (add) {
    add.addEventListener('click', function () {
      form.setAttribute('action', '/ui/account/operators/invites');
      email.value = '';
      email.readOnly = false;
      boxes.forEach(function (b) { b.checked = false; });
      if (title) { title.textContent = 'Create invitation'; }
      if (submit) { submit.textContent = 'Create invitation'; }
      dlg.showModal();
    });
  }
  document.querySelectorAll('.edit-operator').forEach(function (btn) {
    btn.addEventListener('click', function () {
      form.setAttribute('action', '/ui/account/operators/' + btn.getAttribute('data-id') + '/roles');
      email.value = btn.getAttribute('data-email') || '';
      email.readOnly = true;
      var owned = (btn.getAttribute('data-inboxes') || '').split(',');
      boxes.forEach(function (b) { b.checked = owned.indexOf(b.value) >= 0; });
      if (title) { title.textContent = 'Edit operator'; }
      if (submit) { submit.textContent = 'Save access'; }
      dlg.showModal();
    });
  });
  dlg.querySelectorAll('[data-close-dialog]').forEach(function (b) {
    b.addEventListener('click', function () { dlg.close(); });
  });
})();

// New-account invite dialog (system admin plane).
(function () {
  var dlg = document.getElementById('account-invite-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var add = document.getElementById('add-account');
  if (add) {
    add.addEventListener('click', function () {
      form.reset();
      dlg.showModal();
    });
  }
  dlg.querySelectorAll('[data-close-dialog]').forEach(function (b) {
    b.addEventListener('click', function () { dlg.close(); });
  });
})();

// Per-account storage quota dialog (system admin plane).
(function () {
  var dlg = document.getElementById('quota-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var valueInput = document.getElementById('quota-value');
  var unitSelect = document.getElementById('quota-unit');
  var usedNote = document.getElementById('quota-used');
  var title = document.getElementById('quota-title');
  var units = ['b', 'kb', 'mb', 'gb', 'tb'];
  var mult = { b: 1, kb: 1024, mb: 1048576, gb: 1073741824, tb: 1099511627776 };
  var used = 0;

  function preferredUnit(bytes) {
    var div = 1;
    var idx = 0;
    while (idx < units.length - 1 && bytes >= div * 1024) {
      div *= 1024;
      idx += 1;
    }
    return { unit: units[idx], value: bytes / div };
  }

  document.querySelectorAll('.edit-quota').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var id = btn.getAttribute('data-id');
      var quota = parseInt(btn.getAttribute('data-quota'), 10) || 0;
      used = parseInt(btn.getAttribute('data-used'), 10) || 0;
      form.setAttribute('action', '/ui/admin/accounts/' + encodeURIComponent(id) + '/quota');
      if (quota === 0) {
        valueInput.value = '0';
        unitSelect.value = 'mb';
      } else {
        var pick = preferredUnit(quota);
        valueInput.value = String(Math.round(pick.value * 1000) / 1000);
        unitSelect.value = pick.unit;
      }
      if (title) {
        title.textContent = 'Edit storage quota' + (btn.getAttribute('data-name') ? ' — ' + btn.getAttribute('data-name') : '');
      }
      if (usedNote) {
        usedNote.textContent = used > 0 ? 'Currently using ' + used.toLocaleString() + ' bytes.' : '';
      }
      dlg.showModal();
    });
  });

  form.addEventListener('submit', function (e) {
    var v = parseFloat(valueInput.value);
    var bytes = isFinite(v) && v > 0 ? Math.round(v * (mult[unitSelect.value] || 1)) : 0;
    if (bytes > 0 && used > 0 && bytes < used) {
      if (!window.confirm('This quota (' + bytes.toLocaleString() + ' bytes) is below the account\u2019s current usage (' + used.toLocaleString() + ' bytes). New mail will be rejected until usage drops. Continue?')) {
        e.preventDefault();
      }
    }
  });

  dlg.querySelectorAll('[data-close-dialog]').forEach(function (b) {
    b.addEventListener('click', function () { dlg.close(); });
  });
})();

(function () {
  var toast = null;
  var toastTimer = null;

  function showToast(message) {
    if (!toast) {
      toast = document.createElement('div');
      toast.className = 'ok notice';
      toast.setAttribute('role', 'status');
      toast.setAttribute('aria-live', 'polite');
      document.body.appendChild(toast);
    }
    toast.textContent = message;
    toast.classList.remove('dismissing');
    /* Force reflow so a repeated copy restarts the fade-out animation. */
    void toast.offsetWidth;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      toast.classList.add('dismissing');
      setTimeout(function () {
        if (toast) {
          toast.remove();
          toast = null;
        }
      }, 300);
    }, 1500);
  }

  function copyText(text) {
    if (window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text);
    }
    return Promise.reject(new Error('clipboard unavailable'));
  }

  document.querySelectorAll('[data-copy]').forEach(function (el) {
    function activate() {
      var text = el.getAttribute('data-copy');
      copyText(text).then(function () {
        el.classList.add('copied');
        setTimeout(function () { el.classList.remove('copied'); }, 1200);
        showToast('Copied to clipboard');
      }).catch(function () {
        /* Clipboard blocked; selection fallback would be intrusive here. */
      });
    }
    el.addEventListener('click', activate);
    el.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        activate();
      }
    });
  });
})();

/* Passkey (WebAuthn) ceremonies. Two buttons share this module: sign-in on the
   login page and add-passkey on the account page. Each carries data-begin and
   data-finish endpoint paths; the server owns all policy and only the browser
   glue lives here. */
(function () {
  function csrfToken() {
    var el = document.querySelector('[name=_csrf]');
    return el ? el.value : '';
  }

  function b64urlToBuf(value) {
    var pad = value.length % 4 === 0 ? '' : '='.repeat(4 - (value.length % 4));
    var base64 = (value + pad).replace(/-/g, '+').replace(/_/g, '/');
    var raw = atob(base64);
    var buf = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) {
      buf[i] = raw.charCodeAt(i);
    }
    return buf.buffer;
  }

  function bufToB64url(buf) {
    var bytes = new Uint8Array(buf);
    var str = '';
    for (var i = 0; i < bytes.length; i++) {
      str += String.fromCharCode(bytes[i]);
    }
    return btoa(str).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  function prepareCreationOptions(options) {
    options.challenge = b64urlToBuf(options.challenge);
    options.user.id = b64urlToBuf(options.user.id);
    if (options.excludeCredentials) {
      options.excludeCredentials.forEach(function (c) { c.id = b64urlToBuf(c.id); });
    }
    return options;
  }

  function prepareRequestOptions(options) {
    options.challenge = b64urlToBuf(options.challenge);
    if (options.allowCredentials) {
      options.allowCredentials.forEach(function (c) { c.id = b64urlToBuf(c.id); });
    }
    return options;
  }

  function credentialToJSON(cred) {
    var response = cred.response;
    var out = { id: cred.id, rawId: bufToB64url(cred.rawId), type: cred.type, response: {}, clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {} };
    var r = response;
    out.response.clientDataJSON = bufToB64url(r.clientDataJSON);
    if (r.attestationObject !== undefined) {
      out.response.attestationObject = bufToB64url(r.attestationObject);
    }
    if (r.authenticatorData !== undefined) {
      out.response.authenticatorData = bufToB64url(r.authenticatorData);
    }
    if (r.signature !== undefined) {
      out.response.signature = bufToB64url(r.signature);
    }
    if (r.userHandle !== undefined && r.userHandle !== null) {
      out.response.userHandle = bufToB64url(r.userHandle);
    }
    return out;
  }

  function postJSON(url, body, token) {
    return fetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken(),
        'X-WebAuthn-Challenge': token || ''
      },
      body: JSON.stringify(body)
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (!res.ok) {
          throw new Error(data.error || 'request failed');
        }
        return data;
      });
    });
  }

  function setStatus(el, msg, isError) {
    if (!el) { return; }
    el.textContent = msg || '';
    el.classList.toggle('error', !!isError);
  }

  function wire(button, statusEl, isRegister) {
    button.addEventListener('click', function () {
      setStatus(statusEl, 'Waiting for your device…', false);
      button.disabled = true;
      var rpID = '';
      var name = isRegister ? (window.prompt('Name this passkey', 'Passkey') || 'Passkey') : '';
      // Offer to make this the only sign-in method only when a password is
      // currently enabled; a passkey-only account has nothing to disable.
      var makeOnly = isRegister && button.getAttribute('data-password-enabled') === '1' &&
        window.confirm('Use this passkey as your only sign-in method? Your password will be disabled.');
      // The register endpoints are CSRF-protected; the sign-in endpoints are
      // pre-authentication and rely on the ceremony's own origin check.
      var beginHeaders = { 'Content-Type': 'application/json' };
      if (isRegister) {
        beginHeaders['X-CSRF-Token'] = csrfToken();
      }
      fetch(button.getAttribute('data-begin'), {
        method: 'POST',
        credentials: 'same-origin',
        headers: beginHeaders
      }).then(function (res) {
        return res.json().then(function (data) {
          if (!res.ok) { throw new Error(data.error || 'could not start'); }
          return data;
        });
      }).then(function (data) {
        var publicKey;
        if (isRegister) {
          publicKey = prepareCreationOptions(data.options.publicKey || data.options);
          rpID = publicKey.rp && publicKey.rp.id;
          return navigator.credentials.create({ publicKey: publicKey }).then(function (cred) {
            if (!cred) { throw new Error('no credential returned'); }
            var url = button.getAttribute('data-finish') + '?name=' + encodeURIComponent(name) + (makeOnly ? '&only=1' : '');
            return postJSON(url, credentialToJSON(cred), data.challenge_token);
          });
        }
        publicKey = prepareRequestOptions(data.options.publicKey || data.options);
        rpID = publicKey.rpId;
        return navigator.credentials.get({ publicKey: publicKey }).then(function (cred) {
          if (!cred) { throw new Error('no credential returned'); }
          return postJSON(button.getAttribute('data-finish'), credentialToJSON(cred), data.challenge_token);
        });
      }).then(function (result) {
        if (isRegister) {
          setStatus(statusEl, (result && result.password_only) ? 'Passkey added; password sign-in disabled. Reloading…' : 'Passkey added. Reloading…', false);
          window.location.reload();
        } else {
          window.location.href = (result && result.redirect) || '/';
        }
      }).catch(function (err) {
        var message = err.message || 'Passkey failed';
        // Only translate the browser's RP-domain rejection; other security
        // errors (and cancellation/device errors) retain their own explanation.
        if (err.name === 'SecurityError' && rpID &&
            /relying party|rp id|rpid|registrable domain/i.test(message)) {
          message = 'Passkeys are configured for "' + rpID + '", but you are visiting "' + window.location.hostname +
            '". Open MailMoose at its configured BASE_URL, or ask your administrator to correct BASE_URL to the UI URL and restart MailMoose. ' +
            'Use DEDICATED_RECEIVER_URL for a separate inbound receiver URL.';
        }
        setStatus(statusEl, message, true);
        button.disabled = false;
      });
    });
  }

  var signin = document.getElementById('passkey-signin');
  if (signin) {
    if (!window.PublicKeyCredential || !navigator.credentials || !window.isSecureContext) {
      signin.disabled = true;
      signin.title = 'Passkeys require HTTPS and a supported browser';
      setStatus(document.getElementById('passkey-status'), 'Passkeys require a secure (HTTPS) connection and a supported browser.', true);
    } else {
      wire(signin, document.getElementById('passkey-status'), false);
    }
  }
  var add = document.getElementById('passkey-add');
  if (add) {
    if (!window.PublicKeyCredential || !navigator.credentials || !window.isSecureContext) {
      add.disabled = true;
      add.title = 'Passkeys require HTTPS and a supported browser';
      setStatus(document.getElementById('passkey-status'), 'Passkeys require a secure (HTTPS) connection and a supported browser.', true);
    } else {
      wire(add, document.getElementById('passkey-status'), true);
    }
  }

  /* Each passkey row is a name plus a gear button that opens that passkey's
     settings dialog, where the details, rename and remove live. One dialog per
     credential is rendered server-side, so the button only has to find it. */
  document.querySelectorAll('.open-passkey-settings').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var dlg = document.getElementById('passkey-dialog-' + btn.getAttribute('data-passkey'));
      if (dlg && typeof dlg.showModal === 'function') {
        dlg.showModal();
      }
    });
  });
  document.querySelectorAll('.passkey-dialog [data-close-dialog]').forEach(function (b) {
    var dlg = b.closest('dialog');
    if (!dlg) { return; }
    b.addEventListener('click', function () { dlg.close(); });
  });

})();
