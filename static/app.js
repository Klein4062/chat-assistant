// ═══════════════════════════════════════════════════════════════
// Chat Assistant — 前端聊天应用
// 功能：WebSocket 实时通信 / 多会话管理 / 流式渲染 / 自动重连
// ═══════════════════════════════════════════════════════════════
(function() {
    'use strict';

    // ─── DOM 元素引用 ────────────────────────────────────────────

    const messagesContainer = document.getElementById('messagesContainer');
    const messageInput = document.getElementById('messageInput');
    const sendButton = document.getElementById('sendButton');
    const connectionStatus = document.getElementById('connectionStatus');
    const statusDot = connectionStatus.querySelector('.status-dot');
    const statusText = connectionStatus.querySelector('.status-text');
    const logoutButton = document.getElementById('logoutButton');
    const usernameDisplay = document.getElementById('usernameDisplay');
    const convList = document.getElementById('convList');
    const newConvButton = document.getElementById('newConvButton');
    const convCount = document.getElementById('convCount');
    const sidebarToggle = document.getElementById('sidebarToggle');
    const welcomeMessage = document.getElementById('welcomeMessage');

    // ─── 状态变量 ────────────────────────────────────────────────

    let ws = null;
    let reconnectTimer = null;
    let reconnectAttempts = 0;
    const MAX_RECONNECT_DELAY = 30000;

    let welcomeVisible = true;
    let currentUsername = '';
    let currentConvID = null;
    let conversations = [];
    let pendingMessage = null;
    let toastTimer = null;
    let currentStreamConvID = null;
    let uploadedImageURL = null;

    // ─── 会话检查：验证登录状态，未登录重定向 ────────────────────

    async function checkSession() {
        try {
            const resp = await fetch('/api/session');
            const data = await resp.json();
            if (!data.authenticated) {
                window.location.href = '/login';
                return;
            }
            currentUsername = data.username;
            if (usernameDisplay) {
                usernameDisplay.textContent = currentUsername;
            }
            // Load conversations and connect
            await loadConversations();
            initConnection();
        } catch (e) {
            window.location.href = '/login';
        }
    }

    // ─── 登出 ───────────────────────────────────────────────────

    if (logoutButton) {
        logoutButton.addEventListener('click', async function() {
            await fetch('/api/logout', { method: 'POST' });
            window.location.href = '/login';
        });
    }

    // ─── WebSocket 连接管理（自动重连 + 指数退避）─────────────

    // 根据当前页面协议自动选择 ws:// 或 wss://
    function getWebSocketURL() {
        const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        return `${protocol}//${window.location.host}/ws`;
    }

    function connect() {
        if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
            return;
        }

        ws = new WebSocket(getWebSocketURL());

        ws.onopen = function() {
            console.log('WebSocket connected');
            reconnectAttempts = 0;
            updateConnectionStatus('connected', '已连接');

            dismissToast();
            showToast('✅ 已重新连接', 'success', 2000);

            if (pendingMessage && currentConvID) {
                const msg = pendingMessage;
                pendingMessage = null;
                updatePendingToSent();
                ws.send(JSON.stringify({ type: 'message', content: msg, conversation_id: currentConvID }));
            }
        };

        ws.onmessage = function(event) {
            try {
                const msg = JSON.parse(event.data);
                handleMessage(msg);
            } catch (e) {
                console.error('Failed to parse message:', e);
            }
        };

        ws.onclose = function(event) {
            console.log('WebSocket closed:', event.code, event.reason);
            updateConnectionStatus('disconnected', '已断开');
            showToast('⏳ 连接已断开，正在重连...', 'warning');
            scheduleReconnect();
        };

        ws.onerror = function(error) {
            console.error('WebSocket error:', error);
            updateConnectionStatus('disconnected', '连接错误');
        };
    }

    function scheduleReconnect() {
        if (reconnectTimer) return;
        const delay = Math.min(1000 * Math.pow(2, reconnectAttempts), MAX_RECONNECT_DELAY);
        reconnectAttempts++;
        updateConnectionStatus('disconnected', `重连中 (${reconnectAttempts})...`);
        reconnectTimer = setTimeout(function() {
            reconnectTimer = null;
            connect();
        }, delay);
    }

    function updateConnectionStatus(state, text) {
        statusDot.className = 'status-dot';
        if (state === 'connected') {
            statusDot.classList.add('connected');
        }
        statusText.textContent = text;
    }

    // ─── 会话管理：列表 / 创建 / 切换 / 删除 / 重命名 ──────────

    async function loadConversations() {
        try {
            const resp = await fetch('/api/conversations');
            if (!resp.ok) throw new Error('Failed to load');
            conversations = await resp.json();
            updateConvListUI();
            updateConvCountUI();

            if (conversations.length === 0) {
                // Auto-create first conversation
                await createConversation();
            } else {
                // Select the most recently updated conversation that has messages,
                // otherwise fall back to the most recent one
                const withMessages = conversations.find(c => c.message_count > 0);
                const selected = withMessages || conversations[0];
                selectConversation(selected.id, true);
            }
        } catch (e) {
            console.error('Failed to load conversations:', e);
        }
    }

    async function createConversation(title) {
        try {
            const resp = await fetch('/api/conversations', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ title: title || '' })
            });
            if (!resp.ok) {
                const err = await resp.json();
                showToast('⚠️ ' + (err.error || '创建失败'), 'warning', 3000);
                return null;
            }
            const conv = await resp.json();
            conversations.unshift(conv);
            updateConvListUI();
            updateConvCountUI();
            selectConversation(conv.id, true);
            return conv;
        } catch (e) {
            console.error('Failed to create conversation:', e);
            return null;
        }
    }

    async function deleteConversation(convID) {
        try {
            const resp = await fetch(`/api/conversations/${convID}`, { method: 'DELETE' });
            if (!resp.ok) throw new Error('Failed to delete');
            conversations = conversations.filter(c => c.id !== convID);
            updateConvListUI();
            updateConvCountUI();

            if (convID === currentConvID) {
                if (conversations.length > 0) {
                    selectConversation(conversations[0].id, true);
                } else {
                    await createConversation();
                }
            }
        } catch (e) {
            console.error('Failed to delete conversation:', e);
            showToast('⚠️ 删除失败', 'warning', 3000);
        }
    }

    async function renameConversation(convID, newTitle) {
        try {
            const resp = await fetch(`/api/conversations/${convID}`, {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ title: newTitle })
            });
            if (!resp.ok) throw new Error('Failed to rename');
            const conv = conversations.find(c => c.id === convID);
            if (conv) conv.title = newTitle;
            updateConvListUI();
        } catch (e) {
            console.error('Failed to rename conversation:', e);
        }
    }

    async function selectConversation(convID, loadHistory) {
        currentConvID = convID;
        currentStreamConvID = null;

        // Close sidebar on mobile after selecting a conversation
        const sidebar = document.getElementById('sidebar');
        if (sidebar && window.innerWidth <= 768 && sidebar.classList.contains('open')) {
            sidebar.classList.remove('open');
            const backdrop = document.getElementById('sidebarBackdrop');
            if (backdrop) backdrop.remove();
        }

        // Clear messages
        messagesContainer.querySelectorAll('.message-row,.welcome-message').forEach(el => {
            if (el.id !== 'welcomeMessage') el.remove();
        });
        welcomeVisible = true;
        const wm = document.getElementById('welcomeMessage');
        if (wm) wm.style.display = '';

        // Enable input
        messageInput.disabled = false;
        sendButton.disabled = false;
        streamBubble = null;
        streamContent = '';

        // Update sidebar highlight
        updateConvListUI();

        // Load history
        if (loadHistory || loadHistory === undefined) {
            try {
                const resp = await fetch(`/api/conversations/${convID}/messages`);
                if (resp.ok) {
                    const msgs = await resp.json();
                    if (msgs && msgs.length > 0) {
                        removeWelcome();
                        msgs.forEach(m => {
                            const sender = m.role === 'user' ? 'user' : 'server';
                            const name = m.role === 'user' ? currentUsername : 'AI';
                            // 检测 [image:url] 标记，渲染图片气泡
                            const imgMatch = m.content?.match(/^\[image:(.+?)\]/);
                            if (imgMatch) {
                                const imgURL = imgMatch[1];
                                const text = m.content.slice(imgMatch[0].length);
                                addImageBubble(text, imgURL, sender, name, null);
                            } else {
                                addMessageBubble(m.content, sender, name, null);
                            }
                        });
                    }
                }
            } catch (e) {
                console.error('Failed to load messages:', e);
            }
        }
    }

    function updateConvListUI() {
        if (!convList) return;
        convList.innerHTML = '';

        conversations.forEach(conv => {
            const item = document.createElement('div');
            item.className = 'conv-item';
            if (conv.id === currentConvID) {
                item.classList.add('active');
            }
            item.dataset.id = conv.id;

            item.innerHTML = `
                <div class="conv-title">${escapeHtml(conv.title)}</div>
                <div class="conv-actions">
                    <button class="conv-action-btn rename-btn" title="重命名">✎</button>
                    <button class="conv-action-btn delete-btn" title="删除">✕</button>
                </div>
            `;

            // Click to switch
            item.querySelector('.conv-title').addEventListener('click', () => {
                if (conv.id !== currentConvID) {
                    selectConversation(conv.id, true);
                }
            });

            // Rename
            item.querySelector('.rename-btn').addEventListener('click', (e) => {
                e.stopPropagation();
                const newTitle = prompt('请输入新标题：', conv.title);
                if (newTitle && newTitle.trim() && newTitle.trim() !== conv.title) {
                    renameConversation(conv.id, newTitle.trim());
                }
            });

            // Delete
            item.querySelector('.delete-btn').addEventListener('click', (e) => {
                e.stopPropagation();
                if (confirm(`确定删除会话「${conv.title}」吗？聊天记录将被永久删除。`)) {
                    deleteConversation(conv.id);
                }
            });

            convList.appendChild(item);
        });

        // Update new button state
        if (newConvButton) {
            newConvButton.disabled = conversations.length >= 3;
            newConvButton.style.opacity = conversations.length >= 3 ? '0.4' : '1';
        }
    }

    function updateConvCountUI() {
        if (convCount) {
            convCount.textContent = `${conversations.length}/3`;
        }
    }

    // ─── 消息处理：流式渲染 / 搜索结果卡片 ─────────────────────

    let streamBubble = null;
    let streamContent = '';

    function handleMessage(msg) {
        // Filter by conversation_id to prevent cross-conversation leaks
        if (msg.conversation_id && msg.conversation_id !== currentConvID) {
            return;
        }

        switch (msg.type) {
            case 'message':
                removeWelcome();
                addMessageBubble(msg.content, 'server', msg.username, msg.timestamp);
                break;

            case 'stream_start':
                currentStreamConvID = msg.conversation_id;
                removeWelcome();
                showTypingIndicator();
                streamContent = '';
                break;

            case 'stream_chunk':
                if (msg.conversation_id !== currentConvID) break;
                hideTypingIndicator();
                streamContent += msg.content;
                if (!streamBubble) {
                    streamBubble = addStreamingBubble();
                }
                updateStreamingBubble(streamBubble, streamContent);
                break;

            case 'stream_end':
                if (msg.conversation_id !== currentConvID) break;
                currentStreamConvID = null;
                hideTypingIndicator();
                if (streamBubble) {
                    finalizeStreamingBubble(streamBubble, streamContent, msg.timestamp);
                    streamBubble = null;
                    streamContent = '';
                } else if (msg.content) {
                    addMessageBubble(msg.content, 'server', 'AI', msg.timestamp);
                }
                // Refresh conversation list to update message counts
                refreshConversationList();
                break;

            case 'error':
                showToast('⚠️ ' + msg.content, 'warning', 4000);
                break;
        }
    }

    async function refreshConversationList() {
        try {
            const resp = await fetch('/api/conversations');
            if (resp.ok) {
                conversations = await resp.json();
                updateConvListUI();
                updateConvCountUI();
            }
        } catch (e) {}
    }

    function showTypingIndicator() {
        const existing = document.querySelector('.typing-row');
        if (existing) return;
        const row = document.createElement('div');
        row.className = 'message-row server typing-row';
        row.innerHTML = `
            <div class="message-avatar">🤖</div>
            <div class="message-bubble typing-indicator">
                <span></span><span></span><span></span>
            </div>
        `;
        messagesContainer.appendChild(row);
        messagesContainer.scrollTop = messagesContainer.scrollHeight;
    }

    function hideTypingIndicator() {
        const row = document.querySelector('.typing-row');
        if (row) row.remove();
    }

    function addStreamingBubble() {
        const row = document.createElement('div');
        row.className = 'message-row server streaming-row';
        row.innerHTML = `
            <div class="message-avatar">🤖</div>
            <div class="content-wrapper">
                <div class="message-username">AI</div>
                <div class="message-bubble streaming-bubble"></div>
            </div>
        `;
        messagesContainer.appendChild(row);
        messagesContainer.scrollTop = messagesContainer.scrollHeight;
        return row;
    }

    function updateStreamingBubble(row, content) {
        const bubble = row.querySelector('.streaming-bubble');
        if (bubble) {
            bubble.textContent = content;
        }
        messagesContainer.scrollTop = messagesContainer.scrollHeight;
    }

    function finalizeStreamingBubble(row, content, timestamp) {
        const bubble = row.querySelector('.streaming-bubble');
        if (bubble) {
            bubble.classList.remove('streaming-bubble');
        }
        if (timestamp) {
            const time = document.createElement('div');
            time.className = 'message-time';
            time.textContent = formatTime(timestamp);
            row.querySelector('.content-wrapper').appendChild(time);
        }
    }

    // ─── 发送消息（含断线队列 + 搜索开关）─────────────────────

    function sendMessage() {
        const content = messageInput.value.trim();
        if (!content) return;

        if (!currentConvID) {
            showToast('⚠️ 请先选择或创建一个会话', 'warning', 3000);
            return;
        }

        messageInput.value = '';
        messageInput.style.height = 'auto';
        messageInput.focus();

        if (!ws || ws.readyState !== WebSocket.OPEN) {
            pendingMessage = content;
            removeWelcome();
            addPendingMessageBubble(content);
            showToast('⏳ 连接已断开，消息将在重连后自动发送', 'warning');
            reconnectAttempts = 0;
            if (reconnectTimer) {
                clearTimeout(reconnectTimer);
                reconnectTimer = null;
            }
            connect();
            return;
        }

        const msg = { type: 'message', content: content, conversation_id: currentConvID };
        if (uploadedImageURL) {
            msg.image_url = uploadedImageURL;
        }
        ws.send(JSON.stringify(msg));

        removeWelcome();
        if (uploadedImageURL) {
            addImageBubble(content, uploadedImageURL, 'user', currentUsername, new Date().toISOString());
        } else {
            addMessageBubble(content, 'user', currentUsername, new Date().toISOString());
        }
        clearImagePreview();
    }

    function addImageBubble(content, imageURL, sender, username, timestamp) {
        const row = document.createElement('div');
        row.className = `message-row ${sender}`;
        const avatar = sender === 'user' ? (currentUsername || '?').charAt(0).toUpperCase() : '🤖';
        let html = `<div class="message-avatar">${avatar}</div><div class="content-wrapper">`;
        html += `<div class="message-username">${escapeHtml(username)}</div>`;
        html += `<img src="${escapeHtml(imageURL)}" class="message-image" alt="上传图片" loading="lazy">`;
        if (content) html += `<div class="message-text">${escapeHtml(content)}</div>`;
        html += `<div class="message-time">${formatTime(timestamp)}</div></div>`;
        row.innerHTML = html;
        messagesContainer.appendChild(row);
        messagesContainer.scrollTop = messagesContainer.scrollHeight;
    }

    // ─── 消息气泡渲染 ─────────────────────────────────────────

    function addMessageBubble(content, sender, username, timestamp) {
        const row = document.createElement('div');
        row.className = `message-row ${sender}`;

        const avatar = document.createElement('div');
        avatar.className = 'message-avatar';
        avatar.textContent = sender === 'user' || sender === 'user-self' ? '👤' : '🤖';
        row.appendChild(avatar);

        const contentWrapper = document.createElement('div');

        if (username && sender !== 'user' && sender !== 'user-self') {
            const nameLabel = document.createElement('div');
            nameLabel.className = 'message-username';
            nameLabel.textContent = username;
            contentWrapper.appendChild(nameLabel);
        }

        const bubble = document.createElement('div');
        bubble.className = 'message-bubble';
        bubble.textContent = content;
        contentWrapper.appendChild(bubble);

        if (timestamp) {
            const time = document.createElement('div');
            time.className = 'message-time';
            time.textContent = formatTime(timestamp);
            contentWrapper.appendChild(time);
        }

        row.appendChild(contentWrapper);
        messagesContainer.appendChild(row);
        messagesContainer.scrollTop = messagesContainer.scrollHeight;
    }

    function removeWelcome() {
        if (!welcomeVisible) return;
        const welcome = document.getElementById('welcomeMessage');
        if (welcome) {
            welcome.style.display = 'none';
            welcomeVisible = false;
        }
    }

    // ─── Toast 通知（断线/重连/错误提示）──────────────────────

    function showToast(message, type, duration) {
        dismissToast();

        const toast = document.createElement('div');
        toast.id = 'toast';
        toast.className = `toast toast-${type}`;
        toast.textContent = message;
        document.body.appendChild(toast);

        requestAnimationFrame(() => {
            requestAnimationFrame(() => toast.classList.add('visible'));
        });

        if (duration) {
            toastTimer = setTimeout(dismissToast, duration);
        }
    }

    function dismissToast() {
        if (toastTimer) {
            clearTimeout(toastTimer);
            toastTimer = null;
        }
        const toast = document.getElementById('toast');
        if (toast) {
            toast.classList.remove('visible');
            setTimeout(() => {
                if (toast.parentNode) toast.remove();
            }, 300);
        }
    }

    // ─── 待发送消息气泡（断线时暂存展示）───────────────────────

    function addPendingMessageBubble(content) {
        const existing = document.getElementById('pendingMessage');
        if (existing) existing.remove();

        const row = document.createElement('div');
        row.className = 'message-row user';
        row.id = 'pendingMessage';
        row.innerHTML = `
            <div class="message-avatar">👤</div>
            <div class="content-wrapper">
                <div class="message-bubble">${escapeHtml(content)}</div>
                <div class="message-time pending-label">⏳ 等待重连发送...</div>
            </div>
        `;
        messagesContainer.appendChild(row);
        messagesContainer.scrollTop = messagesContainer.scrollHeight;
    }

    function updatePendingToSent() {
        const row = document.getElementById('pendingMessage');
        if (row) {
            row.removeAttribute('id');
            const label = row.querySelector('.pending-label');
            if (label) {
                label.textContent = formatTime(new Date().toISOString());
                label.classList.remove('pending-label');
                label.classList.add('message-time');
            }
        }
    }

    function escapeHtml(str) {
        const div = document.createElement('div');
        div.textContent = str;
        return div.innerHTML;
    }

    function formatTime(timestamp) {
        try {
            const date = new Date(timestamp);
            const now = new Date();
            const isToday = date.toDateString() === now.toDateString();
            const timeStr = date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' });
            if (isToday) return timeStr;
            return date.toLocaleDateString('zh-CN', { month: 'short', day: 'numeric' }) + ' ' + timeStr;
        } catch (e) {
            return '';
        }
    }

    // ─── 图片上传 ─────────────────────────────────────────────

    const imageInput = document.getElementById('imageInput');
    const uploadButton = document.getElementById('uploadButton');
    const imagePreview = document.getElementById('imagePreview');
    const previewImg = document.getElementById('previewImg');
    const removeImageBtn = document.getElementById('removeImage');

    uploadButton.addEventListener('click', () => imageInput.click());

    imageInput.addEventListener('change', function() {
        if (this.files && this.files[0]) uploadImage(this.files[0]);
    });

    // 粘贴图片
    document.addEventListener('paste', function(e) {
        if (!currentConvID) return;
        const items = e.clipboardData?.items;
        if (!items) return;
        for (const item of items) {
            if (item.type.startsWith('image/')) {
                e.preventDefault();
                uploadImage(item.getAsFile());
                break;
            }
        }
    });

    // 拖拽上传
    messagesContainer.addEventListener('dragover', e => e.preventDefault());
    messagesContainer.addEventListener('drop', function(e) {
        e.preventDefault();
        if (!currentConvID) return;
        const file = e.dataTransfer?.files?.[0];
        if (file && file.type.startsWith('image/')) uploadImage(file);
    });

    async function uploadImage(file) {
        if (file.size > 10 * 1024 * 1024) {
            showToast('⚠️ 图片不能超过 10 MB', 'warning', 3000);
            return;
        }

        uploadButton.classList.add('uploading');
        const formData = new FormData();
        formData.append('image', file);

        try {
            const resp = await fetch('/api/upload', { method: 'POST', body: formData });
            const data = await resp.json();
            if (data.url) {
                uploadedImageURL = data.url;
                previewImg.src = data.url;
                imagePreview.style.display = 'inline-block';
                messageInput.focus();
            } else {
                showToast('⚠️ ' + (data.error || '上传失败'), 'warning', 3000);
            }
        } catch (err) {
            showToast('⚠️ 上传失败：' + err.message, 'warning', 3000);
        }
        uploadButton.classList.remove('uploading');
    }

    removeImageBtn.addEventListener('click', clearImagePreview);

    function clearImagePreview() {
        uploadedImageURL = null;
        previewImg.src = '';
        imagePreview.style.display = 'none';
        imageInput.value = '';
    }

    // ─── Event Listeners ───────────────────────────────────────

    sendButton.addEventListener('click', sendMessage);
    messageInput.addEventListener('keydown', function(e) {
        if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault();
            sendMessage();
        }
    });
    messageInput.addEventListener('input', function() {
        this.style.height = 'auto';
        this.style.height = Math.min(this.scrollHeight, 120) + 'px';
    });

    // New conversation button
    if (newConvButton) {
        newConvButton.addEventListener('click', function() {
            createConversation();
        });
    }

    // Sidebar toggle for mobile
    if (sidebarToggle) {
        sidebarToggle.addEventListener('click', function(e) {
            e.stopPropagation();
            toggleSidebar();
        });
    }

    function toggleSidebar() {
        const sidebar = document.getElementById('sidebar');
        if (!sidebar) return;
        const isOpen = sidebar.classList.toggle('open');
        // Show/hide backdrop
        let backdrop = document.getElementById('sidebarBackdrop');
        if (isOpen) {
            if (!backdrop) {
                backdrop = document.createElement('div');
                backdrop.id = 'sidebarBackdrop';
                backdrop.className = 'sidebar-backdrop';
                document.body.appendChild(backdrop);
                backdrop.addEventListener('click', function() {
                    sidebar.classList.remove('open');
                    backdrop.remove();
                });
            }
        } else {
            if (backdrop) backdrop.remove();
        }
    }

    // Close sidebar when clicking outside on mobile
    document.addEventListener('click', function(e) {
        const sidebar = document.getElementById('sidebar');
        if (!sidebar || !sidebar.classList.contains('open')) return;
        if (window.innerWidth > 768) return;
        // Close if click is outside sidebar and not on toggle
        if (!sidebar.contains(e.target) && e.target !== sidebarToggle) {
            sidebar.classList.remove('open');
            const backdrop = document.getElementById('sidebarBackdrop');
            if (backdrop) backdrop.remove();
        }
    });

    // ─── 空闲超时监控（10分钟无操作自动退出）──────────────────

    const IDLE_CHECK_INTERVAL = 60 * 1000;
    const WARNING_THRESHOLD = 60;

    let idleCheckTimer = null;
    let warningShown = false;

    function startIdleMonitoring() {
        idleCheckTimer = setInterval(checkSessionTimeout, IDLE_CHECK_INTERVAL);
        setTimeout(checkSessionTimeout, 10000);
    }

    async function checkSessionTimeout() {
        try {
            const resp = await fetch('/api/session');
            const data = await resp.json();

            if (!data.authenticated) {
                clearInterval(idleCheckTimer);
                window.location.href = '/login?expired=1';
                return;
            }

            if (data.remaining_secs !== undefined && data.remaining_secs <= WARNING_THRESHOLD && !warningShown) {
                showTimeoutWarning(data.remaining_secs);
            }
        } catch (e) {
            console.error('Session check failed:', e);
        }
    }

    function showTimeoutWarning(remainingSecs) {
        warningShown = true;
        const mins = Math.ceil(remainingSecs / 60);

        const bar = document.createElement('div');
        bar.id = 'timeoutWarning';
        bar.className = 'timeout-warning';
        bar.innerHTML = `
            <span class="warning-icon">⏳</span>
            <span>会话即将在 ${mins} 分钟后过期，请进行任意操作以保持登录</span>
        `;
        document.querySelector('.app-container').prepend(bar);

        setTimeout(() => bar.classList.add('visible'), 100);
    }

    function dismissTimeoutWarning() {
        const bar = document.getElementById('timeoutWarning');
        if (bar) {
            bar.classList.remove('visible');
            setTimeout(() => bar.remove(), 300);
        }
        warningShown = false;
    }

    ['mousedown', 'keydown', 'touchstart', 'scroll'].forEach(evt => {
        document.addEventListener(evt, function() {
            if (warningShown) {
                dismissTimeoutWarning();
                fetch('/api/session').catch(() => {});
            }
            const toast = document.getElementById('toast');
            if (toast && toast.classList.contains('toast-success')) {
                dismissToast();
            }
        }, { passive: true });
    });

    // ─── Init ──────────────────────────────────────────────────

    function initConnection() {
        updateConnectionStatus('disconnected', '连接中...');
        connect();
        startIdleMonitoring();
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', checkSession);
    } else {
        checkSession();
    }
})();
