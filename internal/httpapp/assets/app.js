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
    sel.disabled = false;
    if (submitBtn) {
      submitBtn.textContent = 'Create';
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
    var kind = btn.dataset.kind === 'hermes' ? 'hermes' : 'api';
    form.action = '/ui/' + (kind === 'hermes' ? 'hermes' : 'keys') + '/' + btn.dataset.id + '/edit';
    idInput.value = btn.dataset.id;
    sel.value = kind;
    sel.disabled = true;
    if (submitBtn) {
      submitBtn.textContent = 'Save';
    }
    if (rotateBtn) {
      rotateBtn.hidden = kind !== 'api';
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
    } else {
      var inbox = form.querySelector('.key-fields[data-type=hermes] select[name=inbox]');
      if (inbox && btn.dataset.inbox) {
        inbox.value = btn.dataset.inbox;
      }
    }
    sync();
    syncAdmin();
    if (kind === 'hermes') {
      var inbox2 = form.querySelector('.key-fields[data-type=hermes] select[name=inbox]');
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
      if (!window.confirm('Rotate this API key? The current key stops working immediately.')) {
        return;
      }
      var csrfInput = form.querySelector('[name=_csrf]');
      var body = new URLSearchParams();
      if (csrfInput) {
        body.append('_csrf', csrfInput.value);
      }
      rotateBtn.disabled = true;
      fetch('/ui/keys/' + encodeURIComponent(id) + '/rotate', {
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
      setAllRoles(btn.getAttribute('data-set-role'));
    });
  });
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

(function () {
  var dlg = document.getElementById('inbox-dialog');
  if (!dlg) {
    return;
  }
  var form = dlg.querySelector('form');
  var add = document.getElementById('add-inbox');
  if (add) {
    add.addEventListener('click', function () {
      form.reset();
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
  var list = document.getElementById('inbox-sender-list');
  var input = document.getElementById('inbox-sender-input');
  var note = document.getElementById('inbox-sender-note');

  function refreshSenderNote() {
    if (!note) {
      return;
    }
    var count = list.querySelectorAll('input[name=allowed]').length;
    note.textContent = count === 0
      ? 'Anyone can email this inbox. Add an allowed sender to restrict who can email it.'
      : 'Only these addresses can email this inbox.';
  }

  function refreshSenderEmpty() {
    var empty = list.querySelector('.empty');
    if (list.querySelectorAll('input[name=allowed]').length === 0) {
      if (!empty) {
        var li = document.createElement('li');
        li.className = 'empty';
        li.textContent = 'Anyone can email this inbox. Add an address below to restrict who can email it.';
        list.appendChild(li);
      }
    } else if (empty) {
      empty.remove();
    }
  }

  function addSender(value) {
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

  function setSenders(raw) {
    list.innerHTML = '';
    (raw || '').split(',').forEach(function (entry) {
      addSender(entry);
    });
    refreshSenderNote();
    refreshSenderEmpty();
  }

  document.querySelectorAll('.edit-inbox').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var id = encodeURIComponent(btn.dataset.id || '');
      form.action = '/ui/inboxes/' + id + '/edit';
      deleteForm.action = '/ui/inboxes/' + id + '/delete';
      display.value = btn.dataset.name || '';
      address.value = btn.dataset.address || '';
      setSenders(btn.dataset.allowed || '');
      input.value = '';
      dlg.showModal();
    });
  });

  var add = document.getElementById('inbox-sender-add');
  if (add) {
    add.addEventListener('click', function () {
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
  var cancel = document.getElementById('inbox-edit-cancel');
  if (cancel) {
    cancel.addEventListener('click', function () {
      dlg.close();
    });
  }
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
  var btn = document.getElementById('setup-copy');
  if (!btn) {
    return;
  }
  var url = document.getElementById('setup-webhook-url');
  var note = document.getElementById('setup-copy-note');
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
})();
