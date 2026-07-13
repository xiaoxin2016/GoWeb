// 文件浏览页交互：上传（按钮 + 拖拽）、删除、新建文件夹。
(function () {
  'use strict';

  const dropZone = document.getElementById('drop-zone');
  if (!dropZone) return;
  const dir = dropZone.dataset.dir || '';

  const $ = (id) => document.getElementById(id);
  const toast = (msg, isErr) => {
    const t = $('toast');
    t.textContent = msg;
    t.classList.toggle('err', !!isErr);
    t.classList.remove('hidden');
    clearTimeout(t._timer);
    t._timer = setTimeout(() => t.classList.add('hidden'), 3500);
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

  // ---- 上传 ----
  const fileInput = $('file-input');
  $('btn-upload').addEventListener('click', () => fileInput.click());
  fileInput.addEventListener('change', () => {
    if (fileInput.files.length) uploadFiles(fileInput.files);
  });

  function uploadFiles(files) {
    const fd = new FormData();
    let total = 0;
    for (const f of files) { fd.append('file', f); total += f.size; }

    const box = $('upload-progress');
    const fill = $('progress-fill');
    const text = $('progress-text');
    box.classList.remove('hidden');

    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/api/upload?dir=' + encodeURIComponent(dir));
    xhr.setRequestHeader('X-Requested-With', 'fetch');
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) {
        const pct = Math.round((e.loaded / e.total) * 100);
        fill.style.width = pct + '%';
        text.textContent = '上传中 ' + pct + '%（共 ' + files.length + ' 个文件）';
      }
    };
    xhr.onload = () => {
      box.classList.add('hidden');
      fill.style.width = '0';
      if (xhr.status === 200) {
        toast('上传成功');
        setTimeout(() => location.reload(), 600);
      } else {
        let msg = '上传失败';
        try { msg = JSON.parse(xhr.responseText).error || msg; } catch (e) {}
        toast(msg, true);
      }
    };
    xhr.onerror = () => {
      box.classList.add('hidden');
      toast('网络错误，上传失败', true);
    };
    xhr.send(fd);
  }

  // ---- 拖拽上传 ----
  let dragDepth = 0;
  document.addEventListener('dragenter', (e) => {
    e.preventDefault();
    if (e.dataTransfer && [...e.dataTransfer.types].includes('Files')) {
      dragDepth++;
      document.body.classList.add('dragging');
    }
  });
  document.addEventListener('dragleave', (e) => {
    e.preventDefault();
    if (--dragDepth <= 0) { dragDepth = 0; document.body.classList.remove('dragging'); }
  });
  document.addEventListener('dragover', (e) => e.preventDefault());
  document.addEventListener('drop', (e) => {
    e.preventDefault();
    dragDepth = 0;
    document.body.classList.remove('dragging');
    if (e.dataTransfer.files.length) uploadFiles(e.dataTransfer.files);
  });

  // ---- 新建文件夹 ----
  $('btn-mkdir').addEventListener('click', async () => {
    const name = prompt('新文件夹名称：');
    if (!name) return;
    try {
      await api('/api/mkdir', { dir, name: name.trim() });
      toast('文件夹已创建');
      setTimeout(() => location.reload(), 500);
    } catch (err) {
      toast(err.message, true);
    }
  });

  // ---- 删除 ----
  async function deletePaths(paths) {
    const label = paths.length === 1 ? '“' + paths[0] + '”' : '选中的 ' + paths.length + ' 项';
    if (!confirm('确定删除 ' + label + ' 吗？文件夹将被递归删除，此操作不可恢复。')) return;
    try {
      await api('/api/delete', { paths });
      toast('删除成功');
      setTimeout(() => location.reload(), 500);
    } catch (err) {
      toast(err.message, true);
    }
  }

  document.querySelectorAll('.action-delete').forEach((btn) => {
    btn.addEventListener('click', () => {
      const row = btn.closest('tr');
      deletePaths([row.dataset.path]);
    });
  });

  // ---- 多选 ----
  const checkAll = $('check-all');
  const rowChecks = [...document.querySelectorAll('.row-check')];
  const btnDelSel = $('btn-del-selected');

  function refreshSelection() {
    const selected = rowChecks.filter((c) => c.checked);
    btnDelSel.classList.toggle('hidden', selected.length === 0);
    btnDelSel.textContent = '删除选中（' + selected.length + '）';
    if (checkAll) {
      checkAll.checked = rowChecks.length > 0 && selected.length === rowChecks.length;
    }
  }
  if (checkAll) {
    checkAll.addEventListener('change', () => {
      rowChecks.forEach((c) => { c.checked = checkAll.checked; });
      refreshSelection();
    });
  }
  rowChecks.forEach((c) => c.addEventListener('change', refreshSelection));
  btnDelSel.addEventListener('click', () => {
    const paths = rowChecks.filter((c) => c.checked)
      .map((c) => c.closest('tr').dataset.path);
    if (paths.length) deletePaths(paths);
  });
})();
