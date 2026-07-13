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
  // items: [{ file: File, rel: '相对路径（可含子目录）' }]
  const fileInput = $('file-input');
  $('btn-upload').addEventListener('click', () => fileInput.click());
  fileInput.addEventListener('change', () => {
    if (fileInput.files.length) {
      uploadItems([...fileInput.files].map((f) => ({ file: f, rel: f.name })));
      fileInput.value = '';
    }
  });

  function uploadItems(items) {
    if (!items.length) {
      toast('没有可上传的文件', true);
      return;
    }
    const fd = new FormData();
    for (const it of items) {
      // 每个文件前发送一个 path 字段，携带含子目录的相对路径
      fd.append('path', it.rel);
      fd.append('file', it.file, it.file.name);
    }

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
        text.textContent = '上传中 ' + pct + '%（共 ' + items.length + ' 个文件）';
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

  // ---- 拖拽上传（支持文件夹：递归遍历目录树，保留相对路径）----
  // walkEntry 遍历 FileSystemEntry，把文件收集为 {file, rel}
  function walkEntry(entry, prefix, out) {
    return new Promise((resolve) => {
      if (entry.isFile) {
        entry.file(
          (f) => { out.push({ file: f, rel: prefix + entry.name }); resolve(); },
          () => resolve() // 读取失败的条目跳过
        );
      } else if (entry.isDirectory) {
        const reader = entry.createReader();
        const readBatch = () => {
          // readEntries 每次最多返回约 100 条，必须循环调用直到为空
          reader.readEntries(async (ents) => {
            if (!ents.length) { resolve(); return; }
            for (const e of ents) {
              await walkEntry(e, prefix + entry.name + '/', out);
            }
            readBatch();
          }, () => resolve());
        };
        readBatch();
      } else {
        resolve();
      }
    });
  }

  async function collectDropped(dataTransfer) {
    const out = [];
    const entries = [];
    for (const item of dataTransfer.items || []) {
      if (item.kind !== 'file') continue;
      const entry = item.webkitGetAsEntry && item.webkitGetAsEntry();
      if (entry) {
        entries.push(entry);
      } else {
        const f = item.getAsFile();
        if (f) out.push({ file: f, rel: f.name });
      }
    }
    // 注意：webkitGetAsEntry 必须在 drop 事件同步阶段全部取出，
    // 之后再异步遍历目录内容
    for (const entry of entries) {
      await walkEntry(entry, '', out);
    }
    // 不支持 entry API 的旧浏览器回退到 files 列表（仅普通文件）
    if (!out.length && dataTransfer.files.length) {
      for (const f of dataTransfer.files) out.push({ file: f, rel: f.name });
    }
    return out;
  }

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
  document.addEventListener('drop', async (e) => {
    e.preventDefault();
    dragDepth = 0;
    document.body.classList.remove('dragging');
    const items = await collectDropped(e.dataTransfer);
    if (items.length) uploadItems(items);
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
