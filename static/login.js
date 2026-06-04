(function() {
    'use strict';

    const form = document.getElementById('loginForm');
    const usernameInput = document.getElementById('username');
    const passwordInput = document.getElementById('password');
    const errorMessage = document.getElementById('errorMessage');
    const loginButton = document.getElementById('loginButton');

    // Check if already logged in
    checkSession();

    // Show expiration message if redirected from timeout
    if (window.location.search.includes('expired=1')) {
        showError('会话已过期，请重新登录');
        // Clean URL
        window.history.replaceState({}, '', '/login');
    }

    form.addEventListener('submit', async function(e) {
        e.preventDefault();

        const username = usernameInput.value.trim();
        const password = passwordInput.value;

        if (!username || !password) {
            showError('请输入用户名和密码');
            return;
        }

        setLoading(true);
        clearError();

        try {
            const resp = await fetch('/api/login', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ username, password })
            });

            const data = await resp.json();

            if (data.success) {
                // Redirect to chat
                window.location.href = '/';
            } else {
                showError(data.message || '登录失败');
            }
        } catch (err) {
            showError('网络错误，请重试');
        } finally {
            setLoading(false);
        }
    });

    async function checkSession() {
        try {
            const resp = await fetch('/api/session');
            const data = await resp.json();
            if (data.authenticated) {
                window.location.href = '/';
            }
        } catch (e) {
            // Not logged in, stay on login page
        }
    }

    function showError(msg) {
        errorMessage.textContent = msg;
        errorMessage.style.display = 'block';
    }

    function clearError() {
        errorMessage.textContent = '';
        errorMessage.style.display = 'none';
    }

    function setLoading(loading) {
        loginButton.disabled = loading;
        if (loading) {
            loginButton.classList.add('loading');
        } else {
            loginButton.classList.remove('loading');
        }
    }

    // Allow Enter key to submit from any field
    usernameInput.addEventListener('keydown', function(e) {
        if (e.key === 'Enter') passwordInput.focus();
    });
})();
