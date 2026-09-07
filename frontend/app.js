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
    if (message.role === 'assistant') appendAssistant(message.content, message.evidence || []);
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

  function appendAssistant(content = '', evidence = []) {
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
    if (evidence.length) wrap.appendChild(renderEvidence(evidence));
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
      if (state.currentChatId) {
        const detail = await api(`/api/v1/chats/${encodeURIComponent(state.currentChatId)}`);
        els.chatTitle.textContent = detail.chat?.title || 'Chat';
        const stored = detail.messages?.[detail.messages.length - 1];
        if (stored?.role === 'assistant' && stored.evidence?.length) {
          assistant.wrap.querySelector('.tool-box')?.remove();
          assistant.wrap.appendChild(renderEvidence(stored.evidence));
        }
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
