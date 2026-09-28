(() => {
  'use strict';

  const state = {
    sessionId: null,
    currentChatId: null,
    chats: [],
    sending: false,
    heartbeat: null,
    statusTimer: null,
    elapsedTimer: null,
    sendStarted: 0,
    liveToolEvents: [],
    historyCache: new Map(),
    historyRequests: new Map(),
  };

  const $ = (id) => document.getElementById(id);
  const els = {
    chatList: $('chatList'), chatTitle: $('chatTitle'), conversation: $('conversation'), emptyState: $('emptyState'),
    composer: $('composer'), input: $('messageInput'), send: $('sendButton'), newChat: $('newChatButton'),
    refreshChats: $('refreshChats'), runtimePill: $('runtimePill'), runtimeDot: $('runtimeDot'), runtimeLabel: $('runtimeLabel'),
    runtimeSub: $('runtimeSub'), modelHint: $('modelHint'), activityLine: $('activityLine'), activityText: $('activityText'),
    elapsed: $('elapsed'), toast: $('toast'), sidebar: $('sidebar'), openSidebar: $('openSidebar'), closeSidebar: $('closeSidebar'),
  };

  async function api(path, options = {}) {
    const response = await fetch(path, {
      cache: 'no-store',
      headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
      ...options,
    });
    if (response.status === 204) return null;
    const text = await response.text();
    let body = null;
    try { body = text ? JSON.parse(text) : null; } catch { body = { error: text }; }
    if (!response.ok) {
      const error = new Error(body?.error || `Request failed (${response.status})`);
      error.status = response.status;
      throw error;
    }
    return body;
  }

  function showToast(message) {
    els.toast.textContent = message;
    els.toast.classList.add('show');
    window.setTimeout(() => els.toast.classList.remove('show'), 1800);
  }

  async function ensureSession() {
    if (state.sessionId) return state.sessionId;
    const data = await api('/api/v1/session', { method: 'POST', body: '{}' });
    state.sessionId = data.session_id;
    if (state.heartbeat) clearInterval(state.heartbeat);
    state.heartbeat = setInterval(async () => {
      if (!state.sessionId) return;
      try {
        await fetch(`/api/v1/session/${encodeURIComponent(state.sessionId)}/heartbeat`, { method: 'POST', cache: 'no-store' });
      } catch {}
    }, Math.max(5000, (data.heartbeat_interval_s || 15) * 1000));
    return state.sessionId;
  }

  async function closeSession() {
    const id = state.sessionId;
    state.sessionId = null;
    if (!id) return;
    try { await fetch(`/api/v1/session/${encodeURIComponent(id)}`, { method: 'DELETE', keepalive: true }); } catch {}
  }

  async function refreshStatus() {
    try {
      const status = await api('/api/v1/status');
      const loaded = status.ollama?.loaded_models || [];
      const policy = status.model_policy || {};
      const primaryLoaded = loaded.some((name) => String(name).includes('qwen3:8b'));
      const fallbackLoaded = loaded.some((name) => String(name).includes('qwen3:4b'));

      els.runtimeDot.className = 'status-dot';
      if (!status.ollama?.reachable || !policy.allowed) {
        els.runtimeDot.classList.add('error');
        els.runtimeLabel.textContent = !status.ollama?.reachable ? 'Ollama unavailable' : 'Resource hold';
        els.runtimeSub.textContent = policy.reason || 'Production apps have priority';
      } else if (primaryLoaded) {
        els.runtimeLabel.textContent = '8B · Warm';
        els.runtimeSub.textContent = 'Local · CPU';
      } else if (fallbackLoaded) {
        els.runtimeLabel.textContent = '4B · Fallback';
        els.runtimeSub.textContent = 'Local · CPU';
      } else {
        els.runtimeDot.classList.add('sleeping');
        els.runtimeLabel.textContent = 'Sleeping';
        els.runtimeSub.textContent = `${policy.model?.includes('8b') ? '8B ready' : '4B ready'} · ${status.system?.available_memory_gib ?? '?'} GiB free`;
      }
    } catch {
      els.runtimeDot.className = 'status-dot error';
      els.runtimeLabel.textContent = 'Status unavailable';
      els.runtimeSub.textContent = 'MiniAI API';
    }
  }

  function groupFor(dateText) {
    const d = new Date(dateText);
    const now = new Date();
    const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
    const that = new Date(d.getFullYear(), d.getMonth(), d.getDate());
    const days = Math.round((today - that) / 86400000);
    if (days <= 0) return 'Today';
    if (days === 1) return 'Yesterday';
    if (days <= 7) return 'Previous 7 days';
    if (days <= 30) return 'Previous 30 days';
    return 'Older';
  }

  async function loadChats() {
    try {
      const data = await api('/api/v1/chats?limit=200');
      state.chats = data.chats || [];
      renderChatList();
    } catch (err) {
      showToast(err.message);
    }
  }

  function renderChatList() {
    els.chatList.innerHTML = '';
    let lastGroup = '';
    for (const chat of state.chats) {
      const group = groupFor(chat.updated_at);
      if (group !== lastGroup) {
        const label = document.createElement('div');
        label.className = 'chat-group-label';
        label.textContent = group;
        els.chatList.appendChild(label);
        lastGroup = group;
      }
      const row = document.createElement('div');
      row.className = `chat-row${chat.id === state.currentChatId ? ' active' : ''}`;
      const select = document.createElement('button');
      select.className = 'chat-select';
      select.textContent = chat.title || 'New chat';
      select.title = chat.title || 'New chat';
      select.addEventListener('click', () => openChat(chat.id));
      const menu = document.createElement('button');
      menu.className = 'icon-button chat-menu';
      menu.textContent = '•••';
      menu.setAttribute('aria-label', 'Chat options');
      menu.addEventListener('click', (event) => {
        event.stopPropagation();
        chatOptions(chat);
      });
      row.append(select, menu);
      els.chatList.appendChild(row);
    }
  }

  async function chatOptions(chat) {
    const action = window.prompt('Type “rename” or “delete”.');
    if (!action) return;
    if (action.toLowerCase() === 'rename') {
      const title = window.prompt('Chat title:', chat.title || '');
      if (!title?.trim()) return;
      try {
        await api(`/api/v1/chats/${encodeURIComponent(chat.id)}`, { method: 'PATCH', body: JSON.stringify({ title: title.trim() }) });
        await loadChats();
        if (state.currentChatId === chat.id) els.chatTitle.textContent = title.trim();
      } catch (err) { showToast(err.message); }
    }
    if (action.toLowerCase() === 'delete') {
      if (!window.confirm(`Delete “${chat.title || 'New chat'}”?`)) return;
      try {
        await api(`/api/v1/chats/${encodeURIComponent(chat.id)}`, { method: 'DELETE' });
        if (state.currentChatId === chat.id) await newChat(false);
        await loadChats();
      } catch (err) { showToast(err.message); }
    }
  }

  async function newChat(create = true) {
    if (state.sending) return;
    state.currentChatId = null;
    els.chatTitle.textContent = 'New chat';
    clearConversation();
    renderChatList();
    els.input.focus();
    if (create) {
      try {
        const chat = await api('/api/v1/chats', { method: 'POST', body: '{}' });
        state.currentChatId = chat.id;
        els.chatTitle.textContent = chat.title || 'New chat';
        await loadChats();
      } catch (err) { showToast(err.message); }
    }
    closeMobileSidebar();
  }

  async function openChat(id) {
    if (state.sending || !id) return;
    try {
      const detail = await api(`/api/v1/chats/${encodeURIComponent(id)}`);
      state.currentChatId = id;
      els.chatTitle.textContent = detail.chat?.title || 'Chat';
      renderChatList();
      renderConversation(detail.messages || []);
      closeMobileSidebar();
    } catch (err) { showToast(err.message); }
  }

  function clearConversation() {
    els.conversation.querySelectorAll('.message').forEach((el) => el.remove());
    els.emptyState.hidden = false;
  }

  function renderConversation(messages) {
    clearConversation();
    if (!messages.length) return;
    els.emptyState.hidden = true;
    for (const message of messages) appendStoredMessage(message);
    scrollBottom(false);
  }

  function appendStoredMessage(message) {
    if (message.role === 'user') appendUser(message.content);
    if (message.role === 'assistant') appendAssistant(message.content, message.evidence || [], message.history_entry_id);
  }

  function appendUser(content) {
    els.emptyState.hidden = true;
    const wrap = document.createElement('div');
    wrap.className = 'message user';
    const bubble = document.createElement('div');
    bubble.className = 'user-bubble';
    bubble.textContent = content;
    wrap.appendChild(bubble);
    els.conversation.appendChild(wrap);
    scrollBottom();
  }

  function appendAssistant(content = '', evidence = [], historyEntryId = '') {
    els.emptyState.hidden = true;
    const wrap = document.createElement('div');
    wrap.className = 'message assistant';
    const head = document.createElement('div');
    head.className = 'assistant-head';
    head.innerHTML = '<div class="assistant-avatar">AI</div><div><strong>MiniAI</strong><br><span>Local · read-only</span></div>';
    const body = document.createElement('div');
    body.className = 'assistant-body';
    body.innerHTML = renderMarkdown(content);
    wrap.append(head, body);
    const historyId = String(historyEntryId || '').trim();
    if (historyId) wrap.appendChild(renderExecutionDetails(historyId));
    else if (evidence.length) wrap.appendChild(renderEvidence(evidence));
    els.conversation.appendChild(wrap);
    wireCopyButtons(wrap);
    return { wrap, body };
  }

  function renderEvidence(evidence) {
    const details = document.createElement('details');
    details.className = 'evidence-box';
    const summary = document.createElement('summary');
    summary.textContent = `Sources · ${evidence.length}`;
    const list = document.createElement('div');
    list.className = 'evidence-list';
    for (const item of evidence) {
      const row = document.createElement('div');
      row.className = 'evidence-item';
      const name = document.createElement('div');
      name.className = 'tool-name';
      name.textContent = item.tool_name || 'source';
      const info = document.createElement('div');
      info.innerHTML = `<div class="source-path">${escapeHTML(item.path || item.source || item.app || '')}</div><div>${escapeHTML(item.summary || '')}</div>`;
      row.append(name, info);
      list.appendChild(row);
    }
    details.append(summary, list);
    return details;
  }

  function renderExecutionDetails(historyEntryId) {
    const historyId = String(historyEntryId || '').trim();
    const details = document.createElement('details');
    details.className = 'execution-box';
    details.dataset.historyId = historyId;
    const summary = document.createElement('summary');
    summary.textContent = 'Details';
    const content = document.createElement('div');
    content.className = 'execution-content';
    details.append(summary, content);

    if (state.historyCache.has(historyId)) renderExecutionContent(details, summary, content, state.historyCache.get(historyId));
    details.addEventListener('toggle', () => {
      if (details.open && !details.dataset.loaded && !details.dataset.loading) {
        loadExecutionDetails(details, summary, content, historyId);
      }
    });
    return details;
  }

  async function fetchHistoryEntry(historyId) {
    if (state.historyCache.has(historyId)) return state.historyCache.get(historyId);
    if (state.historyRequests.has(historyId)) return state.historyRequests.get(historyId);
    const request = api(`/api/v1/history/${encodeURIComponent(historyId)}`)
      .then((history) => {
        state.historyCache.set(historyId, history);
        state.historyRequests.delete(historyId);
        return history;
      })
      .catch((error) => {
        state.historyRequests.delete(historyId);
        throw error;
      });
    state.historyRequests.set(historyId, request);
    return request;
  }

  async function loadExecutionDetails(details, summary, content, historyId) {
    details.dataset.loading = '1';
    content.replaceChildren(makeElement('div', 'execution-loading', 'Loading execution details…'));
    try {
      const history = await fetchHistoryEntry(historyId);
      if (!details.isConnected || details.dataset.historyId !== historyId) return;
      renderExecutionContent(details, summary, content, history);
    } catch {
      if (!details.isConnected || details.dataset.historyId !== historyId) return;
      renderExecutionError(details, summary, content, historyId);
    } finally {
      delete details.dataset.loading;
    }
  }

  function renderExecutionError(details, summary, content, historyId) {
    summary.textContent = 'Details';
    const error = makeElement('div', 'execution-error');
    error.setAttribute('role', 'status');
    error.appendChild(makeElement('span', '', 'Execution details unavailable.'));
    const retry = makeElement('button', 'execution-retry', 'Retry');
    retry.type = 'button';
    retry.addEventListener('click', () => loadExecutionDetails(details, summary, content, historyId));
    error.appendChild(retry);
    content.replaceChildren(error);
  }

  function renderExecutionContent(details, summary, content, history) {
    const reads = Array.isArray(history?.evidence_reads) ? history.evidence_reads : [];
    summary.textContent = executionSummary(history, reads);
    content.replaceChildren();

    const execution = makeElement('section', 'execution-section');
    execution.appendChild(makeElement('h4', 'execution-section-title', 'Execution'));
    const facts = makeElement('div', 'execution-facts');
    appendExecutionFact(facts, 'Status', friendlyStatus(history?.status), statusClass(history?.status));
    const mode = friendlyMode(history);
    appendExecutionFact(facts, 'Mode', mode);
    if (history?.model_invoked || mode === 'Deterministic' || history?.model) {
      appendExecutionFact(facts, 'Model', history?.model_invoked ? friendlyModel(history.model, history.model_mode) : 'Not used');
    }
    const confidence = friendlyConfidence(history?.confidence);
    if (confidence) appendExecutionFact(facts, 'Confidence', confidence, `confidence-${confidence.toLowerCase()}`);
    const duration = executionDuration(history);
    if (duration) appendExecutionFact(facts, duration.label, duration.value);
    execution.appendChild(facts);

    const targets = Array.isArray(history?.targets) ? history.targets.filter((target) => target?.display_name || target?.canonical_id) : [];
    if (targets.length) {
      const targetBlock = makeElement('div', 'execution-targets');
      targetBlock.appendChild(makeElement('div', 'execution-label', targets.length === 1 ? 'Target' : 'Targets'));
      const targetList = makeElement('div', 'execution-target-list');
      for (const target of targets) {
        const item = makeElement('div', 'execution-target');
        item.append(
          makeElement('span', 'execution-target-kind', humanizeIdentifier(target.kind || 'target')),
          makeElement('span', 'execution-target-name', target.display_name || target.canonical_id),
        );
        targetList.appendChild(item);
      }
      targetBlock.appendChild(targetList);
      execution.appendChild(targetBlock);
    }

    const auditNotes = [];
    if (history?.request_redacted || history?.result_redacted) auditNotes.push('Audit copy redacted for safety.');
    if (history?.request_truncated || history?.result_truncated) auditNotes.push('Audit copy truncated to storage limit.');
    if (auditNotes.length) {
      const notes = makeElement('div', 'execution-notes');
      for (const note of auditNotes) notes.appendChild(makeElement('p', '', note));
      execution.appendChild(notes);
    }
    content.appendChild(execution);

    if (reads.length) content.appendChild(renderHistoryEvidence(reads));

    const technical = renderExecutionTechnical(history);
    if (technical) content.appendChild(technical);
    details.dataset.loaded = '1';
  }

  function executionSummary(history, reads) {
    const parts = ['Details'];
    const status = friendlyStatus(history?.status);
    const confidence = friendlyConfidence(history?.confidence);
    const mode = friendlyMode(history);
    if (status && status !== 'Completed') parts.push(status);
    if (confidence) parts.push(`${confidence} confidence`);
    else if (mode === 'Deterministic') parts.push(mode);
    else if (!reads.length && history?.model_invoked) parts.push(friendlyModel(history.model));
    else if (mode && mode !== 'Conversation') parts.push(mode);
    if (reads.length) parts.push(`${reads.length} source${reads.length === 1 ? '' : 's'}`);
    return parts.join(' · ');
  }

  function friendlyStatus(status) {
    const labels = {
      running: 'Running', succeeded: 'Completed', failed: 'Failed', canceled: 'Canceled', interrupted: 'Interrupted',
    };
    return labels[String(status || '').toLowerCase()] || humanizeIdentifier(status || 'Unknown');
  }

  function statusClass(status) {
    const value = String(status || '').toLowerCase();
    if (value === 'succeeded') return 'status-completed';
    if (value === 'failed') return 'status-failed';
    if (value === 'canceled' || value === 'interrupted') return 'status-muted';
    return '';
  }

  function friendlyMode(history) {
    const answerMode = String(history?.answer_mode || '').toLowerCase();
    if (answerMode === 'deterministic') return 'Deterministic';
    if (answerMode === 'phase2c' || answerMode === 'agent') return 'Investigation';
    if (answerMode === 'conversation' || history?.kind === 'conversation') return 'Conversation';
    if (history?.kind === 'investigation') return 'Investigation';
    return humanizeIdentifier(answerMode || history?.kind || 'Execution');
  }

  function friendlyModel(model, modelMode = '') {
    const value = String(model || '').trim();
    const match = value.match(/^qwen3:(\d+(?:\.\d+)?b)/i);
    const name = match ? `Qwen3 ${match[1].toUpperCase()}` : (value || 'Local model');
    const mode = String(modelMode || '').toLowerCase();
    if (mode === 'primary') return `${name} · Primary`;
    if (mode === 'fallback') return `${name} · Fallback`;
    return name;
  }

  function friendlyConfidence(confidence) {
    const value = String(confidence || '').toLowerCase();
    if (value === 'high') return 'High';
    if (value === 'medium') return 'Medium';
    if (value === 'low') return 'Low';
    return '';
  }

  function executionDuration(history) {
    if (positiveNumber(history?.investigation_seconds)) {
      return { label: 'Investigation', value: formatDuration(Number(history.investigation_seconds)) };
    }
    if (positiveNumber(history?.total_seconds)) {
      return { label: 'Model time', value: formatDuration(Number(history.total_seconds)) };
    }
    const started = Date.parse(history?.started_at || '');
    const completed = Date.parse(history?.completed_at || '');
    if (Number.isFinite(started) && Number.isFinite(completed) && completed > started) {
      return { label: 'Elapsed', value: formatDuration((completed - started) / 1000) };
    }
    return null;
  }

  function formatDuration(seconds) {
    if (seconds >= 60) {
      let minutes = Math.floor(seconds / 60);
      let remainder = Math.round(seconds - minutes * 60);
      if (remainder === 60) { minutes += 1; remainder = 0; }
      return remainder ? `${minutes}m ${remainder}s` : `${minutes}m`;
    }
    const rounded = Math.round(seconds * 10) / 10;
    return `${rounded}s`;
  }

  function positiveNumber(value) {
    return Number.isFinite(Number(value)) && Number(value) > 0;
  }

  function appendExecutionFact(container, label, value, valueClass = '') {
    if (!value) return;
    const fact = makeElement('div', 'execution-fact');
    fact.append(
      makeElement('div', 'execution-label', label),
      makeElement('div', `execution-value${valueClass ? ` ${valueClass}` : ''}`, value),
    );
    container.appendChild(fact);
  }

  function renderHistoryEvidence(reads) {
    const section = makeElement('section', 'execution-section execution-evidence');
    section.appendChild(makeElement('h4', 'execution-section-title', 'Evidence'));
    const followUp = reads.filter((read) => Number(read?.evidence_round) === 2);
    if (followUp.length) {
      const firstPass = reads.filter((read) => Number(read?.evidence_round) !== 2);
      if (firstPass.length) appendEvidenceGroup(section, 'First pass', firstPass);
      appendEvidenceGroup(section, 'Follow-up', followUp);
    } else {
      section.appendChild(renderHistoryEvidenceList(reads));
    }
    return section;
  }

  function appendEvidenceGroup(section, label, reads) {
    section.appendChild(makeElement('h5', 'execution-evidence-group', label));
    section.appendChild(renderHistoryEvidenceList(reads));
  }

  function renderHistoryEvidenceList(reads) {
    const list = makeElement('div', 'execution-evidence-list');
    for (const read of reads) {
      const item = makeElement('div', 'execution-evidence-item');
      item.appendChild(makeElement('div', 'execution-evidence-name', friendlyCapability(read?.capability)));
      const body = makeElement('div', 'execution-evidence-body');
      const context = evidenceContext(read?.arguments);
      if (context) body.appendChild(makeElement('div', 'execution-evidence-context', context));
      if (read?.safe_summary) body.appendChild(makeElement('div', 'execution-evidence-summary', read.safe_summary));
      const readState = evidenceReadState(read);
      if (readState) body.appendChild(makeElement('div', 'execution-evidence-state', readState));
      item.appendChild(body);
      list.appendChild(item);
    }
    return list;
  }

  function friendlyCapability(capability) {
    const labels = {
      get_app_context: 'ReactorLab', list_apps: 'Applications', get_platform_overview: 'Platform overview',
      read_host_history: 'Host history', read_temperature_history: 'Temperature history',
      read_infrastructure_events: 'Infrastructure events', read_activity: 'Recent activity',
      read_recovery: 'Restart & recovery', read_service_history: 'Service history',
      read_application_history: 'Application history', read_deployment_history: 'Deployment history',
      list_databases: 'Databases', read_database_backups: 'Database backups',
      list_repository: 'Repository', search_repository: 'Repository search', read_repository_file: 'Repository file',
      read_runtime_logs: 'Runtime logs', read_deployment_logs: 'Deployment logs',
    };
    return labels[capability] || humanizeIdentifier(capability || 'Source');
  }

  function evidenceContext(args) {
    if (!args || typeof args !== 'object') return '';
    const values = [];
    const push = (value) => {
      if (['string', 'number', 'boolean'].includes(typeof value) && String(value).trim()) values.push(String(value).trim());
    };
    push(args.app);
    push(args.service);
    push(args.database_id);
    push(args.path);
    push(args.range);
    if (args.from || args.to) {
      const from = ['string', 'number'].includes(typeof args.from) ? String(args.from) : '';
      const to = ['string', 'number'].includes(typeof args.to) ? String(args.to) : '';
      push(from && to ? `${from} → ${to}` : (from || to));
    }
    return [...new Set(values)].join(' · ');
  }

  function evidenceReadState(read) {
    const values = [];
    const status = String(read?.status || '').toLowerCase();
    const availability = String(read?.availability || '').toLowerCase();
    if (status && status !== 'completed') values.push(humanizeIdentifier(status));
    if (availability && availability !== 'available') values.push(humanizeIdentifier(availability));
    return values.join(' · ');
  }

  function renderExecutionTechnical(history) {
    const rows = [];
    if (history?.route) rows.push(['Route', friendlyRoute(history.route)]);
    if (Number(history?.evidence_rounds) > 0) rows.push(['Evidence passes', String(history.evidence_rounds)]);
    if (Number(history?.tool_calls) > 0) rows.push(['Reads', String(history.tool_calls)]);
    if (Number(history?.second_round_reads) > 0) rows.push(['Follow-up reads', String(history.second_round_reads)]);
    if (Number(history?.reasoner_calls) > 0) rows.push(['Reasoner calls', String(history.reasoner_calls)]);
    if (Number(history?.planner_calls) > 0) rows.push(['Planner calls', String(history.planner_calls)]);
    if (history?.answer_validation) rows.push(['Validation', friendlyValidation(history.answer_validation)]);
    if (history?.model_invoked && Number(history?.prompt_tokens) > 0) rows.push(['Prompt', `${formatInteger(history.prompt_tokens)} tokens`]);
    if (history?.model_invoked && Number(history?.generated_tokens) > 0) rows.push(['Generated', `${formatInteger(history.generated_tokens)} tokens`]);

    const links = Array.isArray(history?.links) ? history.links : [];
    if (!rows.length && !links.length && !history?.links_truncated) return null;
    const details = document.createElement('details');
    details.className = 'execution-technical';
    const summary = document.createElement('summary');
    summary.textContent = 'Technical';
    const body = makeElement('div', 'execution-technical-body');
    if (rows.length) {
      const grid = makeElement('div', 'execution-technical-grid');
      for (const [label, value] of rows) appendExecutionFact(grid, label, value);
      body.appendChild(grid);
    }
    if (links.length) {
      const linkBlock = makeElement('div', 'execution-links');
      linkBlock.appendChild(makeElement('div', 'execution-label', 'Related executions'));
      for (const link of links) {
        linkBlock.appendChild(makeElement('div', 'execution-link', `${friendlyRelation(link?.relation)} related execution`));
      }
      body.appendChild(linkBlock);
    }
    if (history?.links_truncated) body.appendChild(makeElement('p', 'execution-technical-note', 'Additional links not shown.'));
    details.append(summary, body);
    return details;
  }

  function friendlyValidation(validation) {
    const labels = {
      valid: 'Validated', invalid: 'Fallback used (validation failed)', transport_error: 'Model unavailable / transport issue',
    };
    return labels[String(validation || '').toLowerCase()] || humanizeIdentifier(validation);
  }

  function friendlyRoute(route) {
    const labels = {
      current_platform_health: 'Platform health', thermal_investigation: 'Thermal investigation',
      restart_investigation: 'Restart & recovery', restart_recovery_investigation: 'Restart & recovery',
      application_current: 'Application status', application_current_status: 'Application status',
      application_performance: 'Application performance', application_performance_investigation: 'Application performance',
      deployment_correlation: 'Deployment correlation', deployment_correlation_investigation: 'Deployment correlation',
      database_investigation: 'Database investigation', database_backup_investigation: 'Database investigation',
      repository_investigation: 'Repository investigation', repository_source_investigation: 'Repository investigation',
      exact_current_fact: 'Exact current fact',
    };
    return labels[route] || humanizeIdentifier(route);
  }

  function friendlyRelation(relation) {
    const labels = { undo_of: 'Undo of', recovery_for: 'Recovery for', preview_for: 'Preview for', code_change_for: 'Code change for' };
    return labels[relation] || humanizeIdentifier(relation || 'Related to');
  }

  function humanizeIdentifier(value) {
    const text = String(value || '').replace(/[_-]+/g, ' ').trim();
    return text ? text.charAt(0).toUpperCase() + text.slice(1) : '';
  }

  function formatInteger(value) {
    return new Intl.NumberFormat().format(Math.max(0, Math.round(Number(value) || 0)));
  }

  function makeElement(tag, className = '', text = '') {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (text !== '') element.textContent = String(text);
    return element;
  }

  function replaceLiveToolsWithStoredDetails(assistant, stored) {
    if (!assistant?.wrap?.isConnected || stored?.role !== 'assistant') return;
    const historyId = String(stored.history_entry_id || '').trim();
    const evidence = Array.isArray(stored.evidence) ? stored.evidence : [];
    if (historyId) {
      assistant.wrap.querySelector('.tool-box')?.remove();
      assistant.wrap.querySelector('.evidence-box')?.remove();
      assistant.wrap.querySelector('.execution-box')?.remove();
      assistant.wrap.appendChild(renderExecutionDetails(historyId));
    } else if (evidence.length) {
      assistant.wrap.querySelector('.tool-box')?.remove();
      assistant.wrap.querySelector('.execution-box')?.remove();
      assistant.wrap.appendChild(renderEvidence(evidence));
    }
  }

  function renderLiveTools(container, events) {
    let details = container.querySelector('.tool-box');
    if (!details) {
      details = document.createElement('details');
      details.className = 'tool-box';
      container.appendChild(details);
    }
    const completed = events.filter((e) => e.phase === 'result');
    const names = [...new Set(completed.map((e) => friendlyTool(e.name)))];
    details.innerHTML = '';
    const summary = document.createElement('summary');
    summary.textContent = completed.length
      ? `Checked ${completed.length} source${completed.length === 1 ? '' : 's'}${names.length ? ` — ${names.join(' · ')}` : ''}`
      : 'Inspecting sources…';
    const list = document.createElement('div');
    list.className = 'tool-list';
    for (const event of events.filter((e) => e.phase === 'result')) {
      const row = document.createElement('div');
      row.className = 'tool-item';
      row.innerHTML = `<div class="tool-name">${escapeHTML(event.name)}</div><div>${escapeHTML(event.summary || 'checked')}</div>`;
      list.appendChild(row);
    }
    details.append(summary, list);
  }

  function friendlyTool(name) {
    if (name?.includes('repository')) return 'Repository';
    if (name === 'get_app_context' || name === 'list_apps') return 'ReactorLab';
    if (name?.includes('runtime_logs')) return 'Runtime logs';
    if (name?.includes('deployment_logs')) return 'Deploy logs';
    return 'Source';
  }

  async function ensureChat() {
    if (state.currentChatId) return state.currentChatId;
    const chat = await api('/api/v1/chats', { method: 'POST', body: '{}' });
    state.currentChatId = chat.id;
    els.chatTitle.textContent = chat.title || 'New chat';
    await loadChats();
    return chat.id;
  }

  async function sendMessage(message) {
    message = String(message || '').trim();
    if (!message || state.sending) return;
    state.sending = true;
    updateComposerState();
    appendUser(message);
    els.input.value = '';
    resizeTextarea();
    state.liveToolEvents = [];

    let assistant = appendAssistant('');
    startActivity('Loading local model…');
    setRuntimeLoading();

    try {
      const [sessionId, chatId] = await Promise.all([ensureSession(), ensureChat()]);
      const response = await fetch('/api/v1/chat/stream', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: sessionId, chat_id: chatId, message }),
      });
      if (!response.ok || !response.body) {
        const text = await response.text();
        let err = text;
        try { err = JSON.parse(text).error || text; } catch {}
        throw new Error(err || `Request failed (${response.status})`);
      }

      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';
      let eventName = '';
      let answer = '';

      const consumeBlock = (block) => {
        let dataText = '';
        for (const line of block.split('\n')) {
          if (line.startsWith('event: ')) eventName = line.slice(7).trim();
          if (line.startsWith('data: ')) dataText += line.slice(6);
        }
        if (!dataText) return;
        let data;
        try { data = JSON.parse(dataText); } catch { return; }

        if (eventName === 'meta') {
          if (data.agent) startActivity('Investigating with read-only tools…');
          else startActivity(data.mode === 'fallback' ? 'Using 4B fallback…' : 'Generating locally…');
          els.runtimeLabel.textContent = data.mode === 'fallback' ? '4B · Loading' : '8B · Loading';
        }
        if (eventName === 'tool') {
          state.liveToolEvents.push(data);
          const toolName = friendlyTool(data.name);
          startActivity(data.phase === 'start' ? `Checking ${toolName}…` : 'Reasoning over evidence…');
          renderLiveTools(assistant.wrap, state.liveToolEvents);
        }
        if (eventName === 'token') {
          answer += data.content || '';
          assistant.body.innerHTML = renderMarkdown(answer);
          wireCopyButtons(assistant.body);
          startActivity('Generating answer…');
          scrollBottom();
        }
        if (eventName === 'error') throw new Error(data.error || 'MiniAI error');
        if (eventName === 'done') {
          stopActivity();
          const tps = data.tokens_per_second;
          els.modelHint.textContent = tps ? `${data.model} · ${tps} tok/s` : `${data.model || 'Local model'} · complete`;
        }
      };

      while (true) {
        const { value, done } = await reader.read();
        buffer += decoder.decode(value || new Uint8Array(), { stream: !done });
        let idx;
        while ((idx = buffer.indexOf('\n\n')) >= 0) {
          const block = buffer.slice(0, idx);
          buffer = buffer.slice(idx + 2);
          consumeBlock(block);
        }
        if (done) break;
      }

      await loadChats();
      if (state.currentChatId === chatId) {
        const detail = await api(`/api/v1/chats/${encodeURIComponent(chatId)}`);
        if (state.currentChatId !== chatId) return;
        els.chatTitle.textContent = detail.chat?.title || 'Chat';
        const messages = detail.messages || [];
        let stored = null;
        for (let index = messages.length - 1; index >= 0; index -= 1) {
          if (messages[index]?.role === 'assistant') { stored = messages[index]; break; }
        }
        replaceLiveToolsWithStoredDetails(assistant, stored);
      }
    } catch (err) {
      stopActivity();
      assistant.body.innerHTML = `<p><strong>MiniAI couldn't complete that request.</strong></p><p>${escapeHTML(err.message)}</p>`;
      showToast(err.message);
      if (err.status === 410) {
        state.sessionId = null;
      }
    } finally {
      state.sending = false;
      updateComposerState();
      refreshStatus();
      scrollBottom();
      els.input.focus();
    }
  }

  function setRuntimeLoading() {
    els.runtimeDot.className = 'status-dot loading';
    els.runtimeLabel.textContent = 'Loading…';
    els.runtimeSub.textContent = 'Local model';
  }

  function startActivity(text) {
    if (!state.sendStarted) state.sendStarted = Date.now();
    els.activityLine.hidden = false;
    els.activityText.textContent = text;
    if (!state.elapsedTimer) {
      state.elapsedTimer = setInterval(() => {
        const seconds = Math.floor((Date.now() - state.sendStarted) / 1000);
        els.elapsed.textContent = `${seconds}s`;
      }, 1000);
    }
  }

  function stopActivity() {
    els.activityLine.hidden = true;
    if (state.elapsedTimer) clearInterval(state.elapsedTimer);
    state.elapsedTimer = null;
    state.sendStarted = 0;
    els.elapsed.textContent = '';
  }

  function updateComposerState() {
    els.send.disabled = state.sending || !els.input.value.trim();
    els.input.disabled = state.sending;
  }

  function resizeTextarea() {
    els.input.style.height = 'auto';
    els.input.style.height = `${Math.min(180, els.input.scrollHeight)}px`;
    updateComposerState();
  }

  function scrollBottom(smooth = true) {
    requestAnimationFrame(() => {
      els.conversation.scrollTo({ top: els.conversation.scrollHeight, behavior: smooth ? 'smooth' : 'auto' });
    });
  }

  function escapeHTML(value) {
    return String(value ?? '').replace(/[&<>'"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' }[c]));
  }

  function renderMarkdown(markdown) {
    const source = String(markdown || '').replace(/\r\n/g, '\n');
    const blocks = [];
    const tokenized = source.replace(/```([^\n]*)\n([\s\S]*?)```/g, (_, lang, code) => {
      const token = `@@CODEBLOCK_${blocks.length}@@`;
      blocks.push({ lang: lang.trim(), code: code.replace(/\n$/, '') });
      return token;
    });

    let html = escapeHTML(tokenized);
    html = html.replace(/^### (.+)$/gm, '<h3>$1</h3>')
      .replace(/^## (.+)$/gm, '<h2>$1</h2>')
      .replace(/^# (.+)$/gm, '<h1>$1</h1>')
      .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
      .replace(/`([^`\n]+)`/g, '<code>$1</code>');

    const lines = html.split('\n');
    const out = [];
    let list = null;
    const closeList = () => { if (list) { out.push(`</${list}>`); list = null; } };
    for (const raw of lines) {
      const line = raw.trimEnd();
      if (!line.trim()) { closeList(); continue; }
      if (/^@@CODEBLOCK_\d+@@$/.test(line.trim())) { closeList(); out.push(line.trim()); continue; }
      const bullet = line.match(/^\s*[-*] (.+)$/);
      const ordered = line.match(/^\s*\d+\. (.+)$/);
      if (bullet || ordered) {
        const type = ordered ? 'ol' : 'ul';
        if (list !== type) { closeList(); out.push(`<${type}>`); list = type; }
        out.push(`<li>${(bullet || ordered)[1]}</li>`);
        continue;
      }
      closeList();
      if (/^<h[1-3]>/.test(line)) out.push(line);
      else out.push(`<p>${line}</p>`);
    }
    closeList();
    html = out.join('');

    blocks.forEach((block, index) => {
      const lang = escapeHTML(block.lang || 'text');
      const label = /^(bash|sh|shell|zsh|console)$/i.test(block.lang) ? 'Copy command' : 'Copy';
      const code = escapeHTML(block.code);
      const replacement = `<div class="code-wrap"><div class="code-head"><span>${lang}</span><button class="copy-button" type="button">${label}</button></div><pre><code>${code}</code></pre></div>`;
      html = html.replace(`@@CODEBLOCK_${index}@@`, replacement);
    });
    return html || '<p></p>';
  }

  function wireCopyButtons(root) {
    root.querySelectorAll('.copy-button:not([data-wired])').forEach((button) => {
      button.dataset.wired = '1';
      button.addEventListener('click', async () => {
        const code = button.closest('.code-wrap')?.querySelector('code')?.textContent || '';
        try {
          await navigator.clipboard.writeText(code);
          const old = button.textContent;
          button.textContent = 'Copied';
          setTimeout(() => { button.textContent = old; }, 1200);
        } catch { showToast('Copy failed'); }
      });
    });
  }

  function closeMobileSidebar() { els.sidebar.classList.remove('open'); }

  els.composer.addEventListener('submit', (event) => {
    event.preventDefault();
    sendMessage(els.input.value);
  });
  els.input.addEventListener('input', resizeTextarea);
  els.input.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      if (!state.sending && els.input.value.trim()) sendMessage(els.input.value);
    }
  });
  els.newChat.addEventListener('click', () => newChat(true));
  els.refreshChats.addEventListener('click', loadChats);
  els.openSidebar?.addEventListener('click', () => els.sidebar.classList.add('open'));
  els.closeSidebar?.addEventListener('click', closeMobileSidebar);
  document.querySelectorAll('[data-prompt]').forEach((button) => button.addEventListener('click', () => sendMessage(button.dataset.prompt)));

  window.addEventListener('pagehide', () => { closeSession(); });
  document.addEventListener('visibilitychange', () => { if (!document.hidden) refreshStatus(); });

  async function init() {
    resizeTextarea();
    await Promise.all([loadChats(), refreshStatus()]);
    state.statusTimer = setInterval(refreshStatus, 15000);
    if (state.chats.length) await openChat(state.chats[0].id);
    else clearConversation();
  }

  init();
})();
