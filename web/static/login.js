// 登录页：发送验证码 → 输入验证码 → 登录。
(function () {
  'use strict';
  const $ = (id) => document.getElementById(id);

  const emailInput = $('email');
  const codeInput = $('code');
  const btnSend = $('btn-send');
  const btnVerify = $('btn-verify');
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

  let cooldown = 0;
  let cooldownTimer = null;
  function startCooldown() {
    cooldown = 60;
    btnSend.disabled = true;
    cooldownTimer = setInterval(() => {
      cooldown--;
      if (cooldown <= 0) {
        clearInterval(cooldownTimer);
        btnSend.disabled = false;
        btnSend.textContent = '重新发送';
      } else {
        btnSend.textContent = '重新发送（' + cooldown + 's）';
      }
    }, 1000);
  }

  btnSend.addEventListener('click', async () => {
    const email = emailInput.value.trim();
    if (!email || !email.includes('@')) {
      msg.textContent = '请输入有效的邮箱地址';
      return;
    }
    btnSend.disabled = true;
    msg.textContent = '正在发送验证码…';
    try {
      const data = await api('/api/auth/send-code', { email });
      msg.textContent = data.message || '验证码已发送，请查收邮件';
      $('code-row').classList.remove('hidden');
      btnVerify.classList.remove('hidden');
      codeInput.focus();
      startCooldown();
    } catch (err) {
      msg.textContent = err.message;
      btnSend.disabled = false;
    }
  });

  async function verify() {
    const email = emailInput.value.trim();
    const code = codeInput.value.trim();
    if (code.length !== 6) {
      msg.textContent = '请输入 6 位验证码';
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

  btnVerify.addEventListener('click', verify);
  codeInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') verify(); });
  emailInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !btnSend.disabled) btnSend.click();
  });
})();
