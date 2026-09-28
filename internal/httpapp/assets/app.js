(function () {
  var dlg = document.getElementById('key-dialog');
  if (!dlg) {
    return;
  }
  var sel = document.getElementById('key-type');
  var form = document.getElementById('key-form');
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
  var adminSnapshot = null;
  var currentKind = 'api';

  function sync() {
    document.querySelectorAll('.key-fields').forEach(function (fs) {
      var active = fs.getAttribute('data-type') === sel.value;
      fs.style.display = active ? '' : 'none';
      fs.querySelectorAll('input,select').forEach(function (el) {
        el.disabled = !active;
      });
    });
    dlg.classList.toggle('key-dialog--wide', sel.value === 'api');
    syncHermesRisk();
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

  function inboxNeedsRiskAck() {
    if (!hermesInbox) {
      return false;
    }
    var opt = hermesInbox.options[hermesInbox.selectedIndex];
    return !opt || opt.getAttribute('data-allowlist') !== '1';
  }

  // Gate creating a Hermes relay connection for an inbox with no allowed
  // senders: show the red warning and keep Create disabled until the operator
  // acknowledges that the agent will reply to anyone. Skipped when editing an
  // existing connection (the inbox is fixed and already accepted).
  function syncHermesRisk() {
    if (!hermesWarning || !hermesAckRow || !hermesAck || !submitBtn) {
      return;
    }
    var editing = form.getAttribute('action') !== '/ui/keys';
    var warn = sel.value === 'hermes' && !editing && inboxNeedsRiskAck();
    hermesWarning.hidden = !warn;
    // The row carries an inline display:flex, which wins over [hidden]; toggle
    // display directly so it actually disappears when an allow list is set.
    hermesAckRow.style.display = warn ? 'flex' : 'none';
    if (!warn) {
      hermesAck.checked = false;
      submitBtn.disabled = false;
      return;
    }
    submitBtn.disabled = !hermesAck.checked;
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

  function openCreate() {
    form.reset();
    form.action = '/ui/keys';
    idInput.value = '';
    adminSnapshot = null;
    currentKind = 'api';
    sel.disabled = false;
    if (submitBtn) {
      submitBtn.textContent = 'Add Client';
    }
    if (rotateBtn) {
      rotateBtn.hidden = true;
    }
    showForm();
    sync();
    syncAdmin();
    dlg.showModal();
  }

  function openEdit(btn) {
    form.reset();
    adminSnapshot = null;
    var kind = btn.dataset.kind === 'hermes' ? 'hermes' : (btn.dataset.kind === 'webhook' ? 'webhook' : 'api');
    currentKind = kind;
    var segment = kind === 'hermes' ? 'hermes' : (kind === 'webhook' ? 'webhooks' : 'keys');
    form.action = '/ui/' + segment + '/' + btn.dataset.id + '/edit';
    idInput.value = btn.dataset.id;
    sel.value = kind;
    sel.disabled = true;
    if (submitBtn) {
      submitBtn.textContent = 'Save';
    }
    if (rotateBtn) {
      rotateBtn.hidden = kind === 'hermes';
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
      var inbox = form.querySelector('.key-fields[data-type=hermes] select[name=inbox]');
      if (inbox && btn.dataset.inbox) {
        inbox.value = btn.dataset.inbox;
      }
      var roleSel = form.querySelector('.key-fields[data-type=hermes] select[name=role]');
      if (roleSel) {
        roleSel.value = btn.dataset.role || 'owner';
      }
    }
    sync();
    syncAdmin();
    if (kind === 'hermes' || kind === 'webhook') {
      var inbox2 = form.querySelector('.key-fields[data-type=' + kind + '] select[name=inbox]');
      if (inbox2) {
        inbox2.disabled = true;
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
      window.location.reload();
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

// aliasEditors maps an inbox dialog id ("add"/"edit") to its active alias
// editor, so the shared popup can commit into the right list.
var aliasEditors = {};

// aliasDialog is the single shared add/edit popup. It drives whichever inbox
// dialog is open (the Add-inbox and Edit-inbox dialogs each register an editor
// with initAliasEditor and delegate their popup interaction here).
var aliasDialog = (function () {
  var dlg = document.getElementById('alias-dialog');
  if (!dlg) {
    return { open: function () {}, close: function () {}, error: function () {} };
  }
  var titleEl = document.getElementById('alias-dialog-title');
  var errEl = document.getElementById('alias-error');
  var nameEl = document.getElementById('alias-name');
  var localEl = document.getElementById('alias-local');
  var domainEl = document.getElementById('alias-domain');
  var saveEl = document.getElementById('alias-save');
  var cancelEl = document.getElementById('alias-cancel');
  var current = null;

  function save() {
    if (!current) {
      return;
    }
    current.editor.commit(current.row, nameEl.value, localEl.value, domainEl.value);
  }
  function onKey(e) {
    if (e.key === 'Enter') {
      e.preventDefault();
      save();
    }
  }
  saveEl.addEventListener('click', save);
  cancelEl.addEventListener('click', function () {
    dlg.close();
  });
  nameEl.addEventListener('keydown', onKey);
  localEl.addEventListener('keydown', onKey);

  return {
    open: function (editor, row, name, local, domain) {
      current = { editor: editor, row: row };
      errEl.textContent = '';
      errEl.hidden = true;
      titleEl.textContent = row ? 'Edit alias' : 'Add alias';
      saveEl.textContent = row ? 'Save' : 'Add';
      nameEl.value = name || '';
      localEl.value = local || '';
      if (domain) {
        domainEl.value = domain;
      }
      dlg.showModal();
      nameEl.focus();
    },
    close: function () {
      dlg.close();
    },
    error: function (msg) {
      errEl.textContent = msg;
      errEl.hidden = false;
    }
  };
})();

// initAliasEditor renders an inbox dialog's alias list (a name with the email
// address beneath it, plus edit and remove buttons) and delegates add/edit to
// the shared aliasDialog. Each alias is submitted as a repeated `alias` field
// (the full address) plus a parallel repeated `alias_name` field, zipped by
// index server-side.
var initAliasEditor = (function () {
  return function (opts) {
    var list = opts.list;

    function rows() {
      if (!list) {
        return [];
      }
      return Array.prototype.slice.call(list.querySelectorAll('li.alias-row'));
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
        aliasDialog.open(aliasEditors[opts.id], li, name, at > 0 ? address.slice(0, at) : address, at > 0 ? address.slice(at + 1) : '');
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

    function splitAddress(address) {
      var at = address.lastIndexOf('@');
      if (at <= 0) {
        return { local: address, domain: '' };
      }
      return { local: address.slice(0, at), domain: address.slice(at + 1) };
    }

    function commit(row, rawName, rawLocal, rawDomain) {
      var name = (rawName || '').trim();
      var local = (rawLocal || '').trim().toLowerCase();
      var domain = (rawDomain || '').trim().toLowerCase();
      if (!name) {
        aliasDialog.error('Name is required.');
        return;
      }
      if (name.length > 128) {
        aliasDialog.error('Name must be 128 characters or fewer.');
        return;
      }
      if (name.indexOf(',') !== -1 || /[\r\n]/.test(name) || /[\x00-\x1f\x7f]/.test(name)) {
        aliasDialog.error('Name may not contain commas, newlines or control characters.');
        return;
      }
      if (!local || local.indexOf('@') !== -1 || local.indexOf(' ') !== -1) {
        aliasDialog.error('Enter a valid email local part.');
        return;
      }
      if (!domain) {
        aliasDialog.error('Choose a domain.');
        return;
      }
      var address = local + '@' + domain;
      var duplicate = rows().some(function (r) {
        return r !== row && r.dataset.address === address;
      });
      if (duplicate) {
        aliasDialog.error('That alias already exists.');
        return;
      }
      if (row) {
        row.dataset.address = address;
        row.dataset.name = name;
        var nameEl = row.querySelector('.alias-row-name');
        nameEl.textContent = name;
        nameEl.classList.remove('muted');
        row.querySelector('.alias-row-addr').textContent = address;
        row.querySelector('input[name=alias_name]').value = name;
        row.querySelector('input[name=alias]').value = address;
      } else if (list) {
        list.appendChild(makeRow(name, address));
      }
      refreshEmpty();
      aliasDialog.close();
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
      if (list) {
        list.innerHTML = '';
      }
      refreshEmpty();
    }

    if (opts.addBtn) {
      opts.addBtn.addEventListener('click', function () {
        var domainEl = document.getElementById('alias-domain');
        aliasDialog.open(aliasEditors[opts.id], null, '', '', domainEl ? domainEl.value : '');
      });
    }

    var editor = { commit: commit };
    aliasEditors[opts.id] = editor;
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

// openExternalAliasDialog shows the connector popup for one external alias. The
// dialogs are rendered server-side on the dashboard, keyed by alias id.
function openExternalAliasDialog(aliasID) {
  var dlg = document.getElementById('external-alias-sending-dialog-' + aliasID);
  if (dlg && !dlg.open) {
    dlg.showModal();
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
    id: 'add',
    addBtn: document.getElementById('inbox-add-alias-add'),
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
})();

(function () {
  var dlg = document.getElementById('add-domain-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var add = document.getElementById('add-domain');
  if (add) {
    add.addEventListener('click', function () {
      form.reset();
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

(function () {
  var dlg = document.getElementById('inbox-edit-dialog');
  if (!dlg) {
    return;
  }
  var form = document.getElementById('inbox-edit-form');
  var deleteForm = document.getElementById('inbox-edit-delete-form');
  var address = document.getElementById('inbox-edit-address');
  var display = form.querySelector('[name=display]');
  var usage = document.getElementById('inbox-edit-usage');
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
  // suspendedFromInbox tracks whether an external-alias secondary (Add, Edit
  // or Connector popup) was launched from inbox settings. Cancelling it
  // resumes the inbox edit dialog on the Aliases tab instead of dropping to
  // the bare dashboard. Submitting clears the flag: the page navigates and the
  // server redirect (/?inbox=) takes over the return path.
  var suspendedFromInbox = false;
  function suspendInbox() {
    suspendedFromInbox = true;
    if (dlg.open) {
      dlg.close();
    }
  }
  function resumeInbox() {
    if (!suspendedFromInbox) {
      return;
    }
    suspendedFromInbox = false;
    if (!dlg.open) {
      dlg.showModal();
    }
    var tab = dlg.querySelector('[data-inbox-tab=aliases]');
    if (tab) {
      tab.click();
    }
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
    id: 'edit',
    addBtn: document.getElementById('inbox-alias-add'),
    onChange: refreshEditSender
  });

  document.querySelectorAll('.edit-inbox').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var id = encodeURIComponent(btn.dataset.id || '');
      editInboxID = btn.dataset.id || '';
      form.action = '/ui/inboxes/' + id + '/edit';
      deleteForm.action = '/ui/inboxes/' + id + '/delete';
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
      if (dlg._resetTabs) {
        dlg._resetTabs();
      }
      dlg.showModal();
    });
  });

  var extDlg = document.getElementById('external-alias-dialog');
  var extAdd = document.getElementById('inbox-external-alias-add');
  var extTitle = document.getElementById('external-alias-dialog-title');
  var extSave = document.getElementById('external-alias-save');
  function resetExternalAliasDialog() {
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
    if (extTitle) {
      extTitle.textContent = 'Add external sending alias';
    }
    if (extSave) {
      extSave.textContent = 'Add external alias';
    }
    var extForm = document.getElementById('external-alias-form');
    if (extForm && editInboxID) {
      extForm.action = '/ui/inboxes/' + encodeURIComponent(editInboxID) + '/external-aliases';
    }
  }
  // openExternalAliasEditor reuses the add dialog for renaming: the address is
  // immutable, so it is shown disabled and only the sender name is submitted.
  function openExternalAliasEditor(aliasID, name, address) {
    if (!extDlg) {
      return;
    }
    resetExternalAliasDialog();
    var nameEl = document.getElementById('external-alias-name');
    var addrEl = document.getElementById('external-alias-address');
    if (nameEl) {
      nameEl.value = name || '';
    }
    if (addrEl) {
      addrEl.value = address || '';
      addrEl.disabled = true;
    }
    if (extTitle) {
      extTitle.textContent = 'Edit external alias';
    }
    if (extSave) {
      extSave.textContent = 'Save';
    }
    var extForm = document.getElementById('external-alias-form');
    if (extForm) {
      extForm.action = '/ui/inboxes/' + encodeURIComponent(editInboxID) + '/external-aliases/' + encodeURIComponent(aliasID) + '/edit';
    }
    suspendInbox();
    extDlg.showModal();
    if (nameEl) {
      nameEl.focus();
    }
  }
  if (extDlg && extAdd) {
    extAdd.addEventListener('click', function () {
      resetExternalAliasDialog();
      suspendInbox();
      extDlg.showModal();
    });
    var extCancel = document.getElementById('external-alias-cancel');
    if (extCancel) {
      extCancel.addEventListener('click', function () {
        extDlg.close();
      });
    }
  }

  // Closing a secondary (Cancel, Esc, backdrop) resumes the suspended inbox
  // dialog; submitting clears the flag because the page navigates and the
  // server redirect takes over the return path.
  if (extDlg) {
    extDlg.addEventListener('close', resumeInbox);
    var extFormEl = document.getElementById('external-alias-form');
    if (extFormEl) {
      extFormEl.addEventListener('submit', function () {
        suspendedFromInbox = false;
      });
    }
  }
  document.querySelectorAll('dialog[id^="external-alias-sending-dialog-"]').forEach(function (connDlg) {
    connDlg.addEventListener('close', resumeInbox);
    connDlg.querySelectorAll('form').forEach(function (f) {
      f.addEventListener('submit', function () {
        suspendedFromInbox = false;
      });
    });
  });
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
        suspendInbox();
        openExternalAliasDialog(configure.dataset.alias);
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

  // Reopen the inbox edit dialog on its Aliases tab when the dashboard was
  // loaded with an inbox to open (e.g. returning from a connector save). If a
  // connector popup is also pending (validation error or fresh create), the
  // inbox suspends first so cancelling the popup resumes it. The tab listeners
  // are attached by a later IIFE, so defer past script execution.
  var openCard = document.querySelector('[data-open-inbox]');
  var openInbox = openCard ? openCard.getAttribute('data-open-inbox') : '';
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
        var tab = dlg.querySelector('[data-inbox-tab=aliases]');
        if (tab) {
          tab.click();
        }
        var pending = document.querySelector('.domain-dialog[data-open="1"]');
        if (pending && !pending.open && typeof pending.showModal === 'function') {
          suspendInbox();
          pending.showModal();
        }
      }
    }, 0);
  }
})();

// Tabbed dialog panels shared by the add and edit inbox dialogs.
(function () {
  document.querySelectorAll('[data-inbox-tabs]').forEach(function (bar) {
    var dlg = bar.closest('dialog');
    if (!dlg) {
      return;
    }
    var tabs = bar.querySelectorAll('[data-inbox-tab]');
    var panels = dlg.querySelectorAll('[data-inbox-panel]');

    function activate(name) {
      tabs.forEach(function (t) {
        var on = t.getAttribute('data-inbox-tab') === name;
        t.classList.toggle('active', on);
        t.setAttribute('aria-selected', on ? 'true' : 'false');
      });
      panels.forEach(function (p) {
        p.hidden = p.getAttribute('data-inbox-panel') !== name;
      });
      // The Aliases tab needs room for the alias rows and action buttons;
      // other tabs stay narrow for a tidier form layout.
      if (dlg.id === 'inbox-dialog' || dlg.id === 'inbox-edit-dialog') {
        dlg.classList.toggle('inbox-dialog--wide', name === 'aliases');
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

    var form = dlg.querySelector('form');
    if (form) {
      form.addEventListener('invalid', function (e) {
        var panel = e.target.closest ? e.target.closest('[data-inbox-panel]') : null;
        if (panel) {
          activate(panel.getAttribute('data-inbox-panel'));
        }
      }, true);
    }
  });
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
  document.querySelectorAll('.domain-dialog[data-open="1"]').forEach(function (dlg) {
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
})();

(function () {
  function sync(dlg) {
    var sel = dlg.querySelector('.provider-select');
    if (!sel) {
      return;
    }
    var value = sel.value;
    dlg.querySelectorAll('.provider-fields').forEach(function (group) {
      var active = group.getAttribute('data-provider') === value;
      group.hidden = !active;
      group.querySelectorAll('input,select,textarea').forEach(function (el) {
        el.disabled = !active;
      });
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
    sync(dlg);
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
