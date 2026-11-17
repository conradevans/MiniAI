(function (root, factory) {
  'use strict';

  const controls = factory();
  if (typeof module === 'object' && module.exports) module.exports = controls;
  else root.MiniAIActionControls = controls;
})(typeof globalThis !== 'undefined' ? globalThis : this, () => {
  'use strict';

  const actionIDPattern = /^action_[0-9a-f]{32}$/;
  const applicationIDPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
  const historyIDPattern = /^[A-Za-z0-9_-]{1,128}$/;
  const supportedStates = new Set([
    'awaiting_confirmation',
    'authorized',
    'executing',
    'succeeded',
    'failed',
    'cancelled',
  ]);

  function supportedBase(value) {
    return value && typeof value === 'object' && !Array.isArray(value)
      && actionIDPattern.test(value.action_id)
      && value.kind === 'restart_application'
      && value.target && typeof value.target === 'object' && !Array.isArray(value.target)
      && value.target.type === 'application'
      && applicationIDPattern.test(value.target.id)
      && supportedStates.has(value.state)
      && value.confirmation_required === true
      && value.warning_required === false
      && value.supports_execution === true;
  }

  function normalizedBase(value) {
    return {
      action_id: value.action_id,
      kind: value.kind,
      target: { type: value.target.type, id: value.target.id },
      state: value.state,
      confirmation_required: true,
      warning_required: false,
      supports_execution: true,
    };
  }

  function actionFromDoneEvent(done) {
    if (!done || typeof done !== 'object' || Array.isArray(done)) return null;
    const value = done.action_suggestion;
    if (!supportedBase(value) || value.state !== 'awaiting_confirmation') return null;
    if (typeof value.reason !== 'string') return null;
    const reason = value.reason.trim();
    if (!reason || Array.from(reason).length > 240 || /[\u0000-\u001f\u007f]/.test(reason)) return null;
    return { ...normalizedBase(value), reason };
  }

  function consumeDoneAction(done, render) {
    if (typeof render !== 'function') return false;
    const action = actionFromDoneEvent(done);
    if (!action) return false;
    render(action);
    return true;
  }

  function restoredAction(value, expectedHistoryID = '') {
    if (!supportedBase(value)
      || value.origin !== 'assistant_suggested'
      || value.safety_class !== 'recoverable'
      || !historyIDPattern.test(value.history_id || '')) {
      return null;
    }
    if (expectedHistoryID && value.history_id !== expectedHistoryID) return null;
    return {
      ...normalizedBase(value),
      origin: value.origin,
      safety_class: value.safety_class,
      history_id: value.history_id,
    };
  }

  function presentationFor(action) {
    if (!supportedBase(action)) return null;
    switch (action.state) {
      case 'awaiting_confirmation':
        return { label: 'Recommended action', status: '' };
      case 'authorized':
        return { label: 'Restart authorized', status: 'The restart is authorized but has not run yet.' };
      case 'executing':
        return { label: 'Restart in progress', status: 'The authorized restart is currently running.' };
      case 'succeeded':
        return { label: 'Restart completed', status: 'The restart completed successfully.' };
      case 'failed':
        return { label: 'Restart failed', status: 'The restart did not complete successfully.' };
      case 'cancelled':
        return { label: 'Restart dismissed', status: 'This recommendation was dismissed.' };
      default:
        return null;
    }
  }

  function controlsFor(action) {
    if (!supportedBase(action)) return [];
    if (action.state === 'awaiting_confirmation') return ['confirm', 'dismiss'];
    if (action.state === 'authorized') return ['execute'];
    return [];
  }

  function requestFor(control, actionID) {
    if (!actionIDPattern.test(actionID || '')) return null;
    const base = `/api/admin/actions/${encodeURIComponent(actionID)}`;
    if (control === 'confirm') {
      return { method: 'POST', path: `${base}/confirm`, body: '{"confirm":true}' };
    }
    if (control === 'dismiss') {
      return { method: 'POST', path: `${base}/cancel`, body: '' };
    }
    if (control === 'execute') {
      return { method: 'POST', path: `${base}/execute`, body: '' };
    }
    return null;
  }

  return Object.freeze({
    actionFromDoneEvent,
    consumeDoneAction,
    restoredAction,
    presentationFor,
    controlsFor,
    requestFor,
  });
});
