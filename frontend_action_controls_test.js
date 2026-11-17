'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const controls = require('./frontend/action_controls.js');

const actionID = 'action_0123456789abcdef0123456789abcdef';
const historyID = 'history_0123456789abcdef';

function liveSuggestion(overrides = {}) {
  return {
    action_id: actionID,
    kind: 'restart_application',
    target: { type: 'application', id: 'myscheduler' },
    state: 'awaiting_confirmation',
    confirmation_required: true,
    warning_required: false,
    supports_execution: true,
    reason: 'Restarting is supported by the collected recovery evidence.',
    ...overrides,
  };
}

function restoredAction(state = 'awaiting_confirmation', overrides = {}) {
  return {
    ...liveSuggestion({ reason: undefined }),
    origin: 'assistant_suggested',
    safety_class: 'recoverable',
    history_id: historyID,
    state,
    ...overrides,
  };
}

test('SSE done consumes only the supported assistant restart shape', () => {
  const rendered = [];
  assert.equal(controls.consumeDoneAction(
    { action_suggestion: liveSuggestion() },
    (action) => rendered.push(action),
  ), true);
  assert.equal(rendered.length, 1);
  assert.equal(rendered[0].action_id, actionID);
  assert.equal(rendered[0].reason, 'Restarting is supported by the collected recovery evidence.');

  for (const action_suggestion of [
    liveSuggestion({ kind: 'deploy_repository' }),
    liveSuggestion({ target: { type: 'repository', id: 'myscheduler' } }),
    liveSuggestion({ target: { type: 'application', id: '<script>' } }),
    liveSuggestion({ confirmation_required: false }),
    liveSuggestion({ warning_required: true }),
    liveSuggestion({ supports_execution: false }),
    liveSuggestion({ state: 'authorized' }),
    liveSuggestion({ reason: '' }),
  ]) {
    assert.equal(controls.consumeDoneAction({ action_suggestion }, () => {
      throw new Error('malformed suggestion rendered');
    }), false);
  }

  assert.equal(controls.consumeDoneAction({ tokens: 10 }, () => {
    throw new Error('normal answer rendered an action');
  }), false);
});

test('confirmation, dismissal, and execution have distinct request contracts', () => {
  const confirm = controls.requestFor('confirm', actionID);
  const dismiss = controls.requestFor('dismiss', actionID);
  const execute = controls.requestFor('execute', actionID);

  assert.deepEqual(confirm, {
    method: 'POST',
    path: '/api/admin/actions/' + actionID + '/confirm',
    body: '{"confirm":true}',
  });
  assert.deepEqual(dismiss, {
    method: 'POST',
    path: '/api/admin/actions/' + actionID + '/cancel',
    body: '',
  });
  assert.deepEqual(execute, {
    method: 'POST',
    path: '/api/admin/actions/' + actionID + '/execute',
    body: '',
  });
  assert.doesNotMatch(confirm.path, /execute/);
  assert.doesNotMatch(execute.path, /confirm/);
  assert.equal(controls.requestFor('confirm-and-execute', actionID), null);
});

test('controls follow authoritative state without implicit transitions', () => {
  const awaiting = controls.restoredAction(restoredAction(), historyID);
  const authorized = controls.restoredAction(restoredAction('authorized'), historyID);
  assert.deepEqual(controls.controlsFor(awaiting), ['confirm', 'dismiss']);
  assert.deepEqual(controls.controlsFor(authorized), ['execute']);
  assert.deepEqual(controls.presentationFor(authorized), {
    label: 'Restart authorized',
    status: 'The restart is authorized but has not run yet.',
  });

  for (const state of ['executing', 'succeeded', 'failed', 'cancelled']) {
    const action = controls.restoredAction(restoredAction(state), historyID);
    assert.deepEqual(controls.controlsFor(action), state === 'executing' ? ['refresh'] : []);
    assert.ok(controls.presentationFor(action));
  }
});

test('restored actions remain bound to their History owner and assistant policy', () => {
  const action = controls.restoredAction(restoredAction('succeeded'), historyID);
  assert.equal(action.history_id, historyID);
  assert.equal(action.state, 'succeeded');
  assert.equal(controls.restoredAction(restoredAction(), 'another_history'), null);
  assert.equal(controls.restoredAction(restoredAction('authorized', {
    origin: 'direct_user',
  }), historyID), null);
  assert.equal(controls.restoredAction(restoredAction('authorized', {
    safety_class: 'irreversible',
  }), historyID), null);
  assert.equal(controls.restoredAction(restoredAction('authorized', {
    supports_execution: false,
  }), historyID), null);
});

test('unknown outcome and safe categories survive GET and History restoration', () => {
  const wire = restoredAction('executing', {
    execution_outcome: 'outcome_unknown', failure_category: 'timeout',
    execution_started_at: '2026-11-17T10:00:00Z',
    outcome_recorded_at: '2026-11-17T10:02:00Z',
  });
  const first = controls.restoredAction(wire, historyID);
  const reload = controls.restoredAction(JSON.parse(JSON.stringify(wire)), historyID);
  assert.deepEqual(first, reload);
  assert.equal(reload.execution_outcome, 'outcome_unknown');
  assert.equal(reload.failure_category, 'timeout');
  assert.equal(reload.execution_started_at, wire.execution_started_at);
  assert.match(controls.presentationFor(reload).label, /unknown/);
  assert.match(controls.presentationFor(reload).status, /cannot confirm/);
  assert.deepEqual(controls.controlsFor(reload), ['refresh']);
  assert.deepEqual(controls.requestFor('refresh', actionID), {
    method: 'GET', path: '/api/admin/actions/' + actionID, body: '',
  });
  assert.equal(controls.requestFor('retry', actionID), null);
  // A fresh target observation is not an execution receipt.
  const running = controls.restoredAction({ ...wire, target_status: 'running' }, historyID);
  assert.equal(running.execution_outcome, 'outcome_unknown');
  assert.doesNotMatch(controls.presentationFor(running).status, /completed successfully/);
});

test('failed cards explain closed categories and reject conflicting execution truth', () => {
  const action = controls.restoredAction(restoredAction('failed', {
    execution_outcome: 'failed', failure_category: 'upstream_rejected',
  }), historyID);
  assert.equal(controls.presentationFor(action).label, 'Restart failed');
  assert.match(controls.presentationFor(action).status, /rejected/);
  assert.deepEqual(controls.controlsFor(action), []);
  for (const fields of [
    { execution_outcome: 'succeeded' },
    { failure_category: '<script>private error</script>' },
    { execution_started_at: 'bad' },
  ]) {
    assert.equal(controls.restoredAction(restoredAction('executing', fields), historyID), null);
  }
});
