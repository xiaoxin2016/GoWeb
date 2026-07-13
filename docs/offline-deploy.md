# GoWeb 离线部署安装手册

本手册面向**目标服务器无法访问公网**（内网/隔离网络）的部署场景。
GoWeb 编译产物是**单个静态二进制文件**，模板与静态资源已内嵌，不依赖任何运行时环境，
离线部署非常简单。

三种方式按推荐程度排序：

| 方式 | 适用场景 | 目标机要求 |
|---|---|---|
| [方式一：二进制拷贝](#方式一二进制拷贝推荐) | 绝大多数场景 | 无（不需要 Go、不需要 Docker） |
| [方式二：Docker 镜像离线导入](#方式二docker-镜像离线导入) | 目标机已有 Docker，统一容器化运维 | Docker / Podman |
| [方式三：源码离线构建](#方式三源码离线构建) | 需要在内网修改代码、二次开发 | Go 1.24+ |

准备工作都在**一台可以联网的机器**上完成，然后通过 U 盘、内网文件服务器、跳板机 scp 等方式
把产物带入隔离网络。

---

## 方式一：二进制拷贝（推荐）

### 1. 在联网机器上编译

需要 Go 1.24 或更高版本。按目标服务器的系统/架构交叉编译：

```bash
git clone https://github.com/xiaoxin2016/GoWeb.git
cd GoWeb

# Linux x86_64（最常见）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o goweb-linux-amd64 .

# Linux ARM64（飞腾/鲲鹏/树莓派等）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o goweb-linux-arm64 .

# Windows x86_64
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o goweb-windows-amd64.exe .
```

> `CGO_ENABLED=0` 生成纯静态二进制，不依赖 glibc，任意 Linux 发行版
> （CentOS、Ubuntu、麒麟、统信 UOS、Alpine 等）都可直接运行。

可选：记录校验值，方便入网后核对文件完整性：

```bash
sha256sum goweb-linux-amd64 > goweb-linux-amd64.sha256
```

### 2. 拷贝到目标服务器

把二进制文件（连同 `.sha256`）通过 U 盘或跳板机传到目标机，例如放到 `/opt/goweb/`：

```bash
# 在目标机上
sha256sum -c goweb-linux-amd64.sha256     # 校验完整性
sudo mkdir -p /opt/goweb/data
sudo mv goweb-linux-amd64 /opt/goweb/goweb
sudo chmod +x /opt/goweb/goweb
```

### 3. 试运行

```bash
cd /opt/goweb
GOWEB_LISTEN=:8080 GOWEB_DATA_DIR=/opt/goweb/data ./goweb
```

看到 `GoWeb 文件浏览已启动` 即成功。浏览器访问 `http://<服务器IP>:8080/console`
完成初始化配置（见[初始化配置](#初始化配置)），确认无误后 `Ctrl+C` 停止，改用 systemd 常驻运行。

### 4. 配置 systemd 常驻服务

创建专用运行账户并写入服务单元：

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin goweb
sudo chown -R goweb:goweb /opt/goweb/data

sudo tee /etc/systemd/system/goweb.service > /dev/null <<'EOF'
[Unit]
Description=GoWeb S3/OSS 目录浏览
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=goweb
Group=goweb
WorkingDirectory=/opt/goweb
Environment=GOWEB_LISTEN=:8080
Environment=GOWEB_DATA_DIR=/opt/goweb/data
ExecStart=/opt/goweb/goweb
Restart=on-failure
RestartSec=3

# 安全加固（可选）
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/opt/goweb/data
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now goweb
sudo systemctl status goweb          # 查看运行状态
journalctl -u goweb -f               # 跟踪日志
```

---

## 方式二：Docker 镜像离线导入

### 1. 在联网机器上构建并导出镜像

```bash
git clone https://github.com/xiaoxin2016/GoWeb.git
cd GoWeb

# 若目标机是 ARM64，加 --platform linux/arm64
docker build -t goweb:1.0 .
docker save goweb:1.0 | gzip > goweb-1.0.tar.gz
```

### 2. 拷贝到目标服务器并导入

```bash
# 在目标机上
docker load -i goweb-1.0.tar.gz
```

### 3. 运行

```bash
docker run -d \
  --name goweb \
  --restart unless-stopped \
  -p 8080:8080 \
  -v goweb-data:/app/data \
  goweb:1.0
```

或使用 docker-compose（把下面内容存为 `docker-compose.yml` 一并带入内网）：

```yaml
services:
  goweb:
    image: goweb:1.0
    container_name: goweb
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - goweb-data:/app/data

volumes:
  goweb-data:
```

```bash
docker compose up -d
```

---

## 方式三：源码离线构建

适合需要在内网做二次开发的场景。Go 模块依赖需要提前打包带入。

### 1. 在联网机器上打包源码与依赖

```bash
git clone https://github.com/xiaoxin2016/GoWeb.git
cd GoWeb
go mod vendor        # 把全部依赖下载到 vendor/ 目录
cd ..
tar czf goweb-src-vendored.tar.gz GoWeb
```

同时下载与目标机匹配的 Go 工具链安装包（如 `go1.24.x.linux-amd64.tar.gz`，
从 https://go.dev/dl/ 获取）一并带入。

### 2. 在目标机上安装 Go 并构建

```bash
# 安装 Go 工具链
sudo tar -C /usr/local -xzf go1.24.*.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin

# 解压源码并离线构建（-mod=vendor 只用本地依赖，不访问网络）
tar xzf goweb-src-vendored.tar.gz
cd GoWeb
CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags="-s -w" -o goweb .
```

后续部署步骤与[方式一](#方式一二进制拷贝推荐)第 3、4 步相同。

---

## 初始化配置

无论哪种方式，首次启动后系统处于**初始化模式**（未设置管理员，控制台免登录开放）：

1. 浏览器访问 `http://<服务器IP>:8080/console`
2. **对象存储（S3/OSS）**：填写内网对象存储信息并点「测试连接」
   - 内网 MinIO：Endpoint 填 `http://<minio地址>:9000`，勾选 **Path-Style 寻址**
   - 内网 Ceph RGW / 其他 S3 兼容网关：同上，按实际协议端口填写
   - 「根目录前缀」填要展示的文件夹（如 `share/public`），所有浏览与操作都被限制在该前缀内
3. **邮件服务（SMTP）**：填写**内网可达**的 SMTP 服务器，点「发送测试邮件」验证
   - 离线环境通常使用企业内部邮件服务器（Exchange、Postfix 等）
   - 登录验证码依赖邮件送达，请务必先把测试邮件跑通
4. **登录权限**：填入管理员邮箱、允许登录的邮箱（支持 `*@your-company.com` 通配）并保存
   —— **保存管理员后鉴权立即生效**，此后控制台仅管理员可见

### 离线环境专项检查

| 检查项 | 说明 |
|---|---|
| 服务器时钟 | S3 签名对时间敏感，偏差超过 15 分钟会请求失败。无外网 NTP 时请配置内网 NTP 或手动校时（`timedatectl` / `chronyc sources` 检查） |
| HTTPS 证书 | 若内网对象存储/SMTP 使用自签名证书，最简单的做法是在控制台勾选对应的「**接受自签名证书**」选项（仅限可信内网）。更规范的做法是把内网 CA 加入系统信任：证书放入 `/usr/local/share/ca-certificates/` 后执行 `update-ca-certificates`（Debian/Ubuntu）或 `/etc/pki/ca-trust/source/anchors/` + `update-ca-trust`（RHEL 系）；Docker 部署需把 CA 挂载/构建进镜像 |
| 防火墙 | 放行服务端口（默认 8080），并确认目标机到对象存储端口（9000 等）、SMTP 端口（25/465/587）可达：`curl -v telnet://<host>:<port>` |
| SELinux | RHEL 系如启用 SELinux，二进制放非常规目录可能被拒绝执行，可 `chcon -t bin_t /opt/goweb/goweb` 或调整策略 |

---

## 反向代理与 HTTPS（建议）

生产环境建议置于 Nginx 之后并启用 HTTPS（内网 CA 签发即可），
会话 Cookie 在 HTTPS 下会自动携带 `Secure` 标记：

```nginx
server {
    listen 443 ssl;
    server_name files.your-company.local;

    ssl_certificate     /etc/nginx/ssl/files.crt;
    ssl_certificate_key /etc/nginx/ssl/files.key;

    client_max_body_size 0;          # 不限制上传大小（按需调整）
    proxy_request_buffering off;     # 上传流式转发，避免大文件占满代理磁盘

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_read_timeout 1800s;    # 大文件上传/下载给足时间
        proxy_send_timeout 1800s;
    }
}
```

---

## 备份、升级与回滚

**需要备份的只有数据目录**（默认 `/opt/goweb/data`，Docker 为 `goweb-data` 卷），
其中包含：

- `config.json` —— 全部配置（含 AK/SK、SMTP 密码，注意保管）
- `secret` —— 会话签名密钥（丢失后所有用户需重新登录，无其他影响）

```bash
tar czf goweb-data-$(date +%F).tar.gz -C /opt/goweb data
```

**升级**：用新版二进制替换旧文件并重启即可，配置自动沿用：

```bash
sudo systemctl stop goweb
sudo cp goweb-linux-amd64 /opt/goweb/goweb && sudo chmod +x /opt/goweb/goweb
sudo systemctl start goweb
```

**回滚**：换回旧版二进制重启即可（建议升级前保留旧二进制为 `goweb.bak`）。
Docker 方式则 load 旧版本镜像后重新 `docker run` / 修改 compose 中的 tag。

---

## 常见问题

**Q：管理员配置了错误的 SMTP，自己也登录不进去了怎么办？**
在服务器上直接编辑 `<数据目录>/config.json` 修正 `smtp` 段后重启服务；
或临时设置环境变量 `GOWEB_DEBUG_CODE=1` 重启，邮件发送失败时验证码会打印到服务日志
（`journalctl -u goweb`），登录后修复配置，**修复后务必去掉该变量**。

**Q：如何把系统恢复到初始化模式重新配置？**
停止服务，删除 `<数据目录>/config.json` 中 `auth.admin_emails` 的内容（或整个文件）后重启。

**Q：上传大文件失败？**
检查反向代理的 `client_max_body_size` 与超时设置（见上文 Nginx 示例）；
GoWeb 本身对大小无限制，大文件自动走 S3 分片上传。

**Q：目录里看不到任何文件？**
确认「根目录前缀」填写正确（不含桶名、不以 `/` 开头），并用控制台「测试连接」验证；
注意前缀区分大小写。
