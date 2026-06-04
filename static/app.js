(function() {
    'use strict';

    // DOM Elements
    const messagesContainer = document.getElementById('messagesContainer');
    const messageInput = document.getElementById('messageInput');
    const sendButton = document.getElementById('sendButton');
    const connectionStatus = document.getElementById('connectionStatus');
    const statusDot = connectionStatus.querySelector('.status-dot');
    const statusText = connectionStatus.querySelector('.status-text');
    const logoutButton = document.getElementById('logoutButton');
    const usernameDisplay = document.getElementById('usernameDisplay');

    // WebSocket
    let ws = null;
    let reconnectTimer = null;
    let reconnectAttempts = 0;
    const MAX_RECONNECT_DELAY = 30000;

    // State
    let welcomeVisible = true;
    let currentUsername = '';

    // ─── Session Check ──────────────────────────────────────────

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
            // Proceed to connect WebSocket
            initConnection();
        } catch (e) {
            window.location.href = '/login';
        }
    }

    // ─── Logout ─────────────────────────────────────────────────

    if (logoutButton) {
        logoutButton.addEventListener('click', async function() {
            await fetch('/api/logout', { method: 'POST' });
            window.location.href = '/login';
        });
    }

    // ─── WebSocket Connection ──────────────────────────────────

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

    // ─── Message Handling ──────────────────────────────────────

    function handleMessage(msg) {
        if (msg.type === 'message') {
            removeWelcome();
            const sender = msg.username === currentUsername ? 'user-self' : 'server';
            addMessageBubble(msg.content, sender, msg.username, msg.timestamp);
        }
    }

    function sendMessage() {
        const content = messageInput.value.trim();
        if (!content) return;

        if (!ws || ws.readyState !== WebSocket.OPEN) {
            updateConnectionStatus('disconnected', '未连接，无法发送');
            return;
        }

        const msg = { type: 'message', content: content };
        ws.send(JSON.stringify(msg));

        // Show user's own message immediately
        removeWelcome();
        addMessageBubble(content, 'user', currentUsername, new Date().toISOString());

        messageInput.value = '';
        messageInput.style.height = 'auto';
        messageInput.focus();
    }

    function addMessageBubble(content, sender, username, timestamp) {
        const row = document.createElement('div');
        row.className = `message-row ${sender}`;

        // Avatar
        const avatar = document.createElement('div');
        avatar.className = 'message-avatar';
        avatar.textContent = sender === 'user' || sender === 'user-self' ? '👤' : '🤖';
        row.appendChild(avatar);

        // Content wrapper
        const contentWrapper = document.createElement('div');

        // Username label (if other user's message)
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
        const welcome = messagesContainer.querySelector('.welcome-message');
        if (welcome) {
            welcome.remove();
            welcomeVisible = false;
        }
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

    // ─── Idle Timeout ───────────────────────────────────────────

    const IDLE_CHECK_INTERVAL = 60 * 1000;      // check every 60s
    const WARNING_THRESHOLD = 60;                // show warning at 60s remaining

    let idleCheckTimer = null;
    let warningShown = false;

    function startIdleMonitoring() {
        // Periodically check session status
        idleCheckTimer = setInterval(checkSessionTimeout, IDLE_CHECK_INTERVAL);
        // Also check immediately
        setTimeout(checkSessionTimeout, 10000); // 10s after page load
    }

    async function checkSessionTimeout() {
        try {
            const resp = await fetch('/api/session');
            const data = await resp.json();

            if (!data.authenticated) {
                // Session expired
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

        // Create a subtle warning bar at the top
        const bar = document.createElement('div');
        bar.id = 'timeoutWarning';
        bar.className = 'timeout-warning';
        bar.innerHTML = `
            <span class="warning-icon">⏳</span>
            <span>会话即将在 ${mins} 分钟后过期，请进行任意操作以保持登录</span>
        `;
        document.querySelector('.app-container').prepend(bar);

        // Animate it in
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

    // User activity resets warning
    ['mousedown', 'keydown', 'touchstart', 'scroll'].forEach(evt => {
        document.addEventListener(evt, function() {
            if (warningShown) {
                dismissTimeoutWarning();
                // Ping server to refresh session
                fetch('/api/session').catch(() => {});
            }
        }, { passive: true });
    });

    // ─── Init ──────────────────────────────────────────────────

    function initConnection() {
        updateConnectionStatus('disconnected', '连接中...');
        connect();
        startIdleMonitoring();
    }

    // Start: check session first, then connect
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', checkSession);
    } else {
        checkSession();
    }
})();
