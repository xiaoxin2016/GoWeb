// 控制台：测试 S3 连接、发送测试邮件（使用表单当前值，无需先保存）。
(function () {
  'use strict';
  const $ = (id) => document.getElementById(id);

  const toast = (msg, isErr) => {
    const t = $('toast');
    t.textContent = msg;
    t.classList.toggle('err', !!isErr);
    t.classList.remove('hidden');
    clearTimeout(t._timer);
    t._timer = setTimeout(() => t.classList.add('hidden'), 4000);
  };

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

  const btnS3 = $('btn-test-s3');
  btnS3.addEventListener('click', async () => {
    const f = $('form-s3');
    btnS3.disabled = true;
    btnS3.textContent = '测试中…';
    try {
      const data = await api('/api/console/test-s3', {
        endpoint: f.endpoint.value,
        region: f.region.value,
        access_key: f.access_key.value,
        secret_key: f.secret_key.value,
        bucket: f.bucket.value,
        root_prefix: f.root_prefix.value,
        path_style: f.path_style.checked,
        insecure_tls: f.insecure_tls.checked,
      });
      toast(data.message || '连接成功');
    } catch (err) {
      toast(err.message, true);
    } finally {
      btnS3.disabled = false;
      btnS3.textContent = '测试连接';
    }
  });

  const btnSyslog = $('btn-test-syslog');
  btnSyslog.addEventListener('click', async () => {
    const f = $('form-syslog');
    if (!f.address.value.trim()) {
      toast('请先填写 rsyslog 服务器地址', true);
      return;
    }
    btnSyslog.disabled = true;
    btnSyslog.textContent = '发送中…';
    try {
      const data = await api('/api/console/test-syslog', {
        network: f.network.value,
        address: f.address.value,
        tag: f.tag.value,
        facility: f.facility.value,
      });
      toast(data.message || '测试消息已发送');
    } catch (err) {
      toast(err.message, true);
    } finally {
      btnSyslog.disabled = false;
      btnSyslog.textContent = '发送测试消息';
    }
  });

  const btnSMTP = $('btn-test-smtp');
  btnSMTP.addEventListener('click', async () => {
    const f = $('form-smtp');
    const to = $('test-mail-to').value.trim();
    if (!to) {
      toast('请先填写测试收件邮箱', true);
      return;
    }
    btnSMTP.disabled = true;
    btnSMTP.textContent = '发送中…';
    try {
      const data = await api('/api/console/test-smtp', {
        host: f.host.value,
        port: parseInt(f.port.value, 10) || 0,
        username: f.username.value,
        password: f.password.value,
        from: f.from.value,
        encryption: f.encryption.value,
        insecure_tls: f.insecure_tls.checked,
        to,
      });
      toast(data.message || '测试邮件已发送');
    } catch (err) {
      toast(err.message, true);
    } finally {
      btnSMTP.disabled = false;
      btnSMTP.textContent = '发送测试邮件';
    }
  });
})();
