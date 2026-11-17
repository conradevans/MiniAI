'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const controls = require('./frontend/action_controls.js');

// Small DOM adapter for executing the actual served application without an npm
// dependency. HTML parsing is unnecessary: action cards use DOM/textContent.
class Element {
  constructor(tag = 'div', root = false) {
    Object.assign(this, { tagName: tag, root, children: [], parent: null,
      dataset: {}, style: {}, events: {}, className: '', value: '', disabled: false, text: '' });
    this.classList = {
      add: (name) => { this.className += ' ' + name; },
      remove: (name) => { this.className = this.className.split(' ').filter((v) => v !== name).join(' '); },
    };
  }
  get isConnected() { return this.root || Boolean(this.parent?.isConnected); }
  set textContent(value) { this.text = String(value); }
  get textContent() { return this.text + this.children.map((child) => child.textContent).join(' '); }
  set innerHTML(value) { this.text = value; this.replaceChildren(); }
  appendChild(child) { child.parent = this; this.children.push(child); return child; }
  append(...children) { children.forEach((child) => this.appendChild(child)); }
  insertBefore(child, reference) { child.parent = this; this.children.splice(this.children.indexOf(reference), 0, child); }
  replaceChildren(...children) { this.children.forEach((child) => { child.parent = null; }); this.children = []; this.append(...children); }
  remove() { if (this.parent) this.parent.children = this.parent.children.filter((child) => child !== this); this.parent = null; }
  setAttribute(name, value) { this[name] = value; }
  addEventListener(name, callback) { this.events[name] = callback; }
  matches(selector) {
    if (selector.includes(':')) return false;
    if (!selector.startsWith('.')) return this.tagName === selector;
    return selector.slice(1).split('.').every((name) => this.className.split(' ').includes(name));
  }
  querySelectorAll(selector) {
    const selectors = selector.split(',').map((part) => part.trim());
    return this.children.flatMap((child) => [
      ...(selectors.some((part) => child.matches(part)) ? [child] : []),
      ...child.querySelectorAll(selector),
    ]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest(selector) { return this.matches(selector) ? this : this.parent?.closest(selector); }
  focus() {}
  scrollTo() {}
  click() { if (!this.disabled) this.events.click?.({ preventDefault() {}, stopPropagation() {} }); }
}

const id = 'action_0123456789abcdef0123456789abcdef';
const historyID = 'history_test';
function record(state, category = '') {
  return { action_id: id, kind: 'restart_application', origin: 'assistant_suggested', safety_class: 'recoverable',
    target: { type: 'application', id: 'miniai-restart-canary' }, history_id: historyID,
    confirmation_required: true, warning_required: false, supports_execution: true, state,
    execution_outcome: { executing: 'outcome_unknown', failed: 'failed', succeeded: 'succeeded' }[state],
    failure_category: category };
}
const flush = async () => { for (let i = 0; i < 12; i++) await new Promise((resolve) => setImmediate(resolve)); };

async function loadPage(server, executeGate = null, executeResponse = null) {
  const roots = new Map();
  const document = {
    getElementById(id) { if (!roots.has(id)) roots.set(id, new Element('div', true)); return roots.get(id); },
    createElement: (tag) => new Element(tag), querySelectorAll: () => [], addEventListener() {},
  };
  const requests = [];
  const fetch = async (path, options = {}) => {
    const method = options.method || 'GET';
    requests.push({ method, path });
    let body;
    let status = 200;
    if (path.includes('/confirm')) { server.action = record('authorized'); body = server.action; }
    else if (path.includes('/execute')) {
      if (executeGate) await executeGate;
      if (executeResponse) return executeResponse(server);
      body = server.action;
      status = body.state === 'failed' ? 502 : body.state === 'executing' ? 202 : 200;
    }
    else if (path.startsWith('/api/admin/actions?')) body = { history_id: historyID, actions: [server.action] };
    else if (path.startsWith('/api/admin/actions/')) body = server.action;
    else if (path.startsWith('/api/v1/chats?')) body = { chats: [{ id: 'chat', title: 'canary', updated_at: '2026-11-17' }] };
    else if (path === '/api/v1/chats/chat') body = { chat: { id: 'chat' }, messages: [{ role: 'assistant', content: 'Canary investigation', history_entry_id: historyID }] };
    else if (path === '/api/v1/status') body = {};
    else throw new Error('unexpected request: ' + method + ' ' + path);
    return { ok: status < 400, status, text: async () => JSON.stringify(body) };
  };
  const window = { MiniAIActionControls: controls, addEventListener() {}, setTimeout() {} };
  vm.runInNewContext(fs.readFileSync(require.resolve('./frontend/app.js'), 'utf8'), {
    document, window, fetch, setInterval() {}, clearInterval() {}, requestAnimationFrame: (fn) => fn(),
  });
  await flush();
  return { requests, conversation: roots.get('conversation'), button(label) {
    return roots.get('conversation').querySelectorAll('button').find((button) => button.textContent === label);
  } };
}

test('actual page reload restores failed, unknown and success cards using GET only', async () => {
  for (const [state, category, label] of [
    ['executing', 'timeout', 'Restart outcome unknown'],
    ['failed', 'upstream_rejected', 'Restart failed'],
    ['succeeded', '', 'Restart completed'],
  ]) {
    const server = { action: record(state, category) };
    const page = await loadPage(server);
    assert.match(page.conversation.textContent, new RegExp(label));
    if (state === 'failed') assert.match(page.conversation.textContent, /rejected/);
    assert.equal(page.button('Execute restart'), undefined);
    if (state === 'executing') {
      page.button('Refresh status').click();
      await flush();
      assert.match(page.conversation.textContent, /cannot confirm/);
      assert.equal(page.requests.filter((r) => r.path === '/api/admin/actions/' + id).length, 1);
    }
    assert.ok(page.requests.every((r) => r.method === 'GET'));
    const reloaded = await loadPage(server);
    assert.match(reloaded.conversation.textContent, new RegExp(label));
    assert.ok(reloaded.requests.every((r) => r.method === 'GET'));
  }
});

test('actual confirm is authorization only; double execute sends one POST and preserves unknown', async () => {
  const server = { action: record('awaiting_confirmation') };
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  const page = await loadPage(server, gate);
  page.button('Confirm restart').click();
  await flush();
  assert.equal(page.requests.filter((r) => r.path.endsWith('/execute')).length, 0);
  assert.ok(page.button('Execute restart'));
  const execute = page.button('Execute restart');
  execute.click();
  execute.click();
  server.action = record('executing', 'invalid_response');
  release();
  await flush();
  assert.equal(page.requests.filter((r) => r.path.endsWith('/execute')).length, 1);
  assert.match(page.conversation.textContent, /Restart outcome unknown/);
  page.button('Refresh status').click();
  await flush();
  assert.equal(page.requests.filter((r) => r.method === 'POST').length, 2);
  assert.equal(page.button('Execute restart'), undefined);
});

test('a gateway HTML error reconciles durable failure with GET and never retries Execute', async () => {
  const server = { action: record('authorized') };
  const page = await loadPage(server, null, () => {
    server.action = record('failed', 'upstream_rejected');
    return { ok: false, status: 502, text: async () => '<!DOCTYPE html><title>Bad Gateway</title>' };
  });
  page.button('Execute restart').click();
  await flush();
  assert.match(page.conversation.textContent, /Restart failed/);
  assert.match(page.conversation.textContent, /rejected/);
  assert.doesNotMatch(page.conversation.textContent, /DOCTYPE|Bad Gateway/);
  assert.deepEqual(page.requests.filter((request) => request.path.startsWith('/api/admin/actions/' + id)), [
    { method: 'POST', path: '/api/admin/actions/' + id + '/execute' },
    { method: 'GET', path: '/api/admin/actions/' + id },
  ]);
  assert.equal(page.button('Execute restart'), undefined);
});
