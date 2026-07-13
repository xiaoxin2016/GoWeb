// 登录页：发送验证码 → 输入验证码 → 登录。
// 发送后主按钮切换为「登录」，重发降级为底部的小号文字链接（带 60s 倒计时）。
(function () {
  'use strict';
  const $ = (id) => document.getElementById(id);

  const emailInput = $('email');
  const codeInput = $('code');
  const btnSend = $('btn-send');
  const btnVerify = $('btn-verify');
  const btnResend = $('btn-resend');
  const resendRow = $('resend-row');
  const msg = $('login-msg');

  const api = async (url, body) => {
    const resp = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'fetch' },
      body: JSON.stringify(body),
    });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(data.error || ('请求失败 (' + resp.status + ')'));
    return data;
  };

  let cooldownTimer = null;
  function startCooldown() {
    let remain = 60;
    btnResend.disabled = true;
    btnResend.textContent = '重新发送（' + remain + 's）';
    clearInterval(cooldownTimer);
    cooldownTimer = setInterval(() => {
      remain--;
      if (remain <= 0) {
        clearInterval(cooldownTimer);
        btnResend.disabled = false;
        btnResend.textContent = '重新发送';
      } else {
        btnResend.textContent = '重新发送（' + remain + 's）';
      }
    }, 1000);
  }

  async function sendCode(trigger) {
    const email = emailInput.value.trim();
    if (!email || !email.includes('@')) {
      msg.textContent = '请输入有效的邮箱地址';
      return;
    }
    trigger.disabled = true;
    msg.textContent = '正在发送验证码…';
    try {
      const data = await api('/api/auth/send-code', { email });
      msg.textContent = data.message || '验证码已发送，请查收邮件';
      // 切换到验证阶段：主按钮变为「登录」，重发降级为文字链接
      btnSend.classList.add('hidden');
      $('code-row').classList.remove('hidden');
      btnVerify.classList.remove('hidden');
      resendRow.classList.remove('hidden');
      emailInput.readOnly = true;
      codeInput.value = '';
      codeInput.focus();
      startCooldown();
    } catch (err) {
      msg.textContent = err.message;
      trigger.disabled = false;
    }
  }

  async function verify() {
    const email = emailInput.value.trim();
    const code = codeInput.value.trim();
    if (code.length !== 6) {
      msg.textContent = '请输入 6 位验证码';
      codeInput.focus();
      return;
    }
    btnVerify.disabled = true;
    msg.textContent = '正在验证…';
    try {
      await api('/api/auth/verify', { email, code });
      msg.textContent = '登录成功，正在跳转…';
      location.href = '/files/';
    } catch (err) {
      msg.textContent = err.message;
      btnVerify.disabled = false;
    }
  }

  btnSend.addEventListener('click', () => sendCode(btnSend));
  btnResend.addEventListener('click', () => sendCode(btnResend));
  btnVerify.addEventListener('click', verify);
  codeInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') verify(); });
  emailInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !btnSend.classList.contains('hidden') && !btnSend.disabled) sendCode(btnSend);
  });
})();
