# GoWeb — S3/OSS 目录浏览

一个使用 Go 编写的对象存储目录浏览系统，界面风格参考 [h5ai](https://github.com/lrsjng/h5ai/)。
可以浏览 S3 兼容对象存储（AWS S3、阿里云 OSS、腾讯云 COS、MinIO 等）中某个指定文件夹下的
全部目录和文件，并提供上传、下载、删除等基础功能。

## 功能

- **目录浏览**：h5ai 风格的文件列表（图标、大小、修改时间、面包屑导航），文件夹优先排序，
  支持深色模式，仅浏览配置的根前缀（文件夹）内的内容
- **文件操作**：
  - 上传：按钮选择或拖拽到页面（**支持拖拽整个文件夹**，自动保留目录结构），
    多文件、带进度条，服务端流式转发不落盘，大文件自动分片
  - 下载：服务端流式代理，无需暴露存储凭证或桶权限
  - 删除：单个删除或勾选批量删除，文件夹递归删除
  - 重命名（**仅管理员**）：文件与文件夹均可改名，文件夹递归搬迁其下全部内容
  - 新建文件夹
- **邮件验证码登录**：无密码，输入邮箱 → 收取 6 位验证码 → 登录；
  验证码 10 分钟有效、一次性、限制重发频率与尝试次数；会话为 HMAC 签名令牌。
  配置默认邮箱域名后，用户可只填邮箱名（如 `zhangsan`）登录，系统自动补全为
  `zhangsan@默认域`；该域名不会出现在登录页，用户登录成功前不会被暴露
- **目录级权限**：可把根目录下的一级目录设为只读 —— 普通用户仅能浏览与下载，
  上传、删除、新建文件夹仅管理员可操作（含其所有子目录，服务端强制校验）；
  重命名无论目录是否只读都仅管理员可用
- **控制台**（仅管理员）：
  - 对象存储配置：Endpoint、Region、AK/SK、Bucket、根目录前缀、Path-Style 寻址、
    自签名证书兼容（跳过 TLS 校验），可在线测试连接
  - SMTP 配置：服务器、端口、加密方式（SSL / STARTTLS / 无）、账号密码、发件人、
    自签名证书兼容，可发送测试邮件；认证机制按服务器通告自动选择（PLAIN / LOGIN / CRAM-MD5），
    25 端口明文连接亦可认证
  - 登录权限：管理员邮箱、允许登录的邮箱（支持 `*@example.com` 通配）、会话有效期、
    默认邮箱域名
  - 目录权限：把根目录下的一级目录设为只读
  - 公告栏：公告内容、展示样式（提示/警示/紧急）、是否允许用户关闭
  - 审计日志：查询页面 + rsyslog 外发配置（UDP/TCP、Tag、Facility），可发送测试消息
- **公告栏**：以条带形式展示在登录后页面顶部，适合发布维护通知等信息；
  三种醒目程度可选，可设为允许用户关闭（公告内容变更后会重新展示）
- **操作审计**：记录每位用户的文件下载、上传、删除、重命名操作
  （含时间、用户、路径、来源 IP、结果）；持久化于数据目录 `audit.log`（JSON Lines），
  并可通过 syslog 协议（RFC 3164，消息体为 JSON）实时外发到 rsyslog 服务器

> 内网/无外网环境部署请参阅 **[离线部署安装手册](docs/offline-deploy.md)**。

## 快速开始

### 一键部署（推荐）

```bash
sudo ./install.sh                # 安装或升级，注册 systemd 服务并启动
sudo ./install.sh --port 9090    # 指定监听端口
sudo ./install.sh uninstall      # 卸载（--purge 连同数据目录一起删除）
```

脚本会自动定位二进制（脚本同目录的 `goweb` / `goweb-linux-<arch>`，或 `--binary` 指定；
都没有且装了 Go 时从源码现场构建），安装到 `/opt/goweb`，以 `nobody` 用户注册 systemd
服务并完成健康检查。重复运行即为升级（自动备份旧版本为 `goweb.bak`，配置沿用）。
无 systemd 的环境会仅安装文件并给出手动启动命令。

### 多租户部署

采用多实例方式实现租户隔离：**每个租户一个完全独立的实例**（独立端口、配置、
S3/SMTP 凭证、管理员、会话密钥与审计日志），互不影响：

```bash
sudo ./install.sh --name tenant-a --port 9081    # 租户 A（服务名 goweb-tenant-a）
sudo ./install.sh --name tenant-b --port 9082    # 租户 B（服务名 goweb-tenant-b）
sudo ./install.sh uninstall --name tenant-a      # 卸载某个租户
```

每个实例安装在 `/opt/goweb-<name>`，各自访问 `http://IP:端口/console` 独立完成初始化。
升级某个租户：重新运行对应的安装命令即可。

前置 Nginx 按域名分发（每个租户一个域名，Cookie 天然按域隔离）：

```nginx
server {
    listen 80;
    server_name files-a.example.com;
    location / { proxy_pass http://127.0.0.1:9081; proxy_set_header Host $host; proxy_set_header X-Real-IP $remote_addr; }
}
server {
    listen 80;
    server_name files-b.example.com;
    location / { proxy_pass http://127.0.0.1:9082; proxy_set_header Host $host; proxy_set_header X-Real-IP $remote_addr; }
}
```

生产环境建议参照上文 Nginx 示例补充 HTTPS 与上传相关参数
（`client_max_body_size`、`proxy_request_buffering off`、超时）。

### 手动运行

```bash
go build -o goweb .
./goweb
```

默认监听 `:8080`，数据（配置文件、签名密钥）保存在 `./data/` 目录。

**首次运行**处于「初始化模式」：访问 `http://localhost:8080/console`（此时无需登录），
依次配置对象存储、SMTP，并在「登录权限」中填入管理员邮箱。保存管理员邮箱后系统即启用鉴权，
之后所有页面都需要邮箱验证码登录，控制台仅管理员可见。

### 配置 systemd 常驻服务

生产环境建议用 systemd 常驻运行（在线与离线部署均适用），以 `nobody` 用户运行：

```bash
sudo mkdir -p /opt/goweb/data
sudo cp goweb /opt/goweb/goweb && sudo chmod +x /opt/goweb/goweb
sudo chown -R nobody /opt/goweb/data

sudo tee /etc/systemd/system/goweb.service > /dev/null <<'EOF'
[Unit]
Description=GoWeb S3/OSS 目录浏览
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=nobody
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

### Docker

```bash
docker build -t goweb .
docker run -d -p 8080:8080 -v goweb-data:/app/data goweb
```

### 命令行参数

| 参数 | 说明 |
|---|---|
| `--ignore-email` | 不通过 SMTP 投递，登录验证码直接打印到服务端控制台，登录流程其余环节不变 |
| `-v`, `--version` | 显示版本 |
| `-h`, `--help` | 显示帮助 |

`--ignore-email` 适合尚未配置邮件服务的内网环境与排障：

```bash
./goweb --ignore-email
# 2026/01/01 10:00:00 [ignore-email] zhangsan@test.com 的登录验证码: 123456（10 分钟内有效）
```

> **注意**：开启后任何能看到本进程日志（含 `journalctl`）的人都能登录任意被允许的邮箱，
> 启动时会打印醒目告警，请勿用于生产环境。systemd 部署可在服务文件的
> `ExecStart=/opt/goweb/goweb` 后追加该参数。

### 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GOWEB_LISTEN` | `:8080` | 监听地址 |
| `GOWEB_DATA_DIR` | `data` | 数据目录（配置与密钥） |
| `GOWEB_DEBUG_CODE` | 关闭 | 设为 `1` 时，邮件发送失败会把验证码打印到日志（仅供调试，生产环境勿开） |

### 常见对象存储配置示例

| 服务 | Endpoint | Path-Style |
|---|---|---|
| AWS S3 | 留空（填写 Region 即可） | 否 |
| 阿里云 OSS | `https://oss-cn-hangzhou.aliyuncs.com` | 否 |
| 腾讯云 COS | `https://cos.ap-guangzhou.myqcloud.com` | 否 |
| MinIO | `http://your-minio:9000` | 是 |

## 项目结构

```
main.go                 入口
internal/config/        配置加载/保存（data/config.json，控制台在线修改）
internal/audit/         操作审计：本地 JSONL + 内存缓冲 + syslog(rsyslog) 外发
internal/auth/          邮箱验证码与会话令牌
internal/mailer/        SMTP 邮件发送
internal/storage/       S3 兼容存储的列目录/上传/下载/删除/重命名
internal/server/        HTTP 路由、鉴权中间件与各页面/接口处理
web/                    内嵌的模板与静态资源（html/template + 原生 JS）
```

## 测试

```bash
go test ./...
```

存储层使用 [gofakes3](https://github.com/johannesboyne/gofakes3) 内存 S3 服务做端到端测试，
覆盖列目录、上传、下载、递归删除、重命名与根前缀隔离。

## 安全说明

- 所有文件操作接口都要求登录，路径经过校验（拒绝 `..`、空段等），并被限制在配置的根前缀内
- 管理员功能（控制台各页面与接口、重命名等）对非管理员**不可见**：统一由中间件
  返回与未知路由逐字一致的 404，而非 403 —— 不暴露端点的存在，也不给探测者
  可枚举的目标；被拒绝的请求会记入服务端日志供管理员追溯
- 邮箱输入在提交阶段严格校验：本地部分只放行字母、数字与 `.` `-` `_`，
  域名只放行字母、数字与 `-` `.`；`? ! = # & +`、引号、尖括号、空格
  以及任何控制字符（含 `\r` `\n` `\t`）一律拒绝，绝不交给邮件投递系统。
  邮件层另有一道防线，拒绝收件人/发件人/主题中的换行，杜绝邮件头注入
- 未登录用户请求验证码时返回统一提示，不泄露邮箱是否在允许名单中；
  重发限流对名单内外的邮箱一视同仁，避免通过“发送过于频繁”枚举允许名单
- 默认邮箱域名仅存于服务端配置：登录页、前端脚本与任何未鉴权响应都不包含该域名，
  邮箱名的补全在服务端完成
- 会话 Cookie 为 `HttpOnly` + `SameSite=Lax`；HTTPS 下自动携带 `Secure` 标记（建议生产环境置于
  反向代理后启用 HTTPS）
- AK/SK 与 SMTP 密码保存在服务端 `data/config.json`（权限 0600），控制台页面不回显密文
