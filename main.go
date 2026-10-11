package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xiaoxin2016/goweb/internal/config"
	"github.com/xiaoxin2016/goweb/internal/server"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入，开发构建为 dev。
var version = "dev"

func main() {
	dataDir := getenv("GOWEB_DATA_DIR", "data")
	addr := getenv("GOWEB_LISTEN", ":8080")

	opts := server.Options{Version: version, DebugCode: os.Getenv("GOWEB_DEBUG_CODE") == "1"}
	for _, arg := range os.Args[1:] {
		switch arg {
		case "-v", "--version", "version":
			fmt.Println("GoWeb", version)
			return
		case "-h", "--help", "help":
			usage()
			return
		case "--ignore-email":
			opts.IgnoreEmail = true
		default:
			fmt.Fprintf(os.Stderr, "未知参数: %s\n\n", arg)
			usage()
			os.Exit(2)
		}
	}

	store, err := config.Load(dataDir)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	srv, err := server.New(store, opts)
	if err != nil {
		log.Fatalf("初始化服务失败: %v", err)
	}

	log.Printf("GoWeb %s 已启动，监听 %s", version, addr)
	switch {
	case opts.IgnoreEmail:
		log.Printf("警告：已启用 --ignore-email，登录验证码将直接打印到本控制台而不发送邮件；" +
			"任何能看到本进程日志的人都能登录，请勿用于生产环境")
	case opts.DebugCode:
		log.Printf("警告：已设置 GOWEB_DEBUG_CODE=1，邮件投递失败时将把登录验证码打印到本控制台" +
			"并允许继续登录；任何能看到本进程日志的人都能登录，请勿用于生产环境")
	}
	if len(store.Get().Auth.AdminEmails) == 0 {
		log.Printf("首次运行：请访问 http://localhost%s/console 完成初始化配置", addr)
	}
	serve(&http.Server{Addr: addr, Handler: srv}, srv)
}

// shutdownGrace 收到退出信号后，等待在途请求收尾的最长时间。
// 必须明显短于外部的强杀时限，否则来不及保存会话：docker stop 默认 10 秒后
// 发送 SIGKILL，systemd 的 TimeoutStopSec 默认 90 秒。
const shutdownGrace = 5 * time.Second

// serve 启动 HTTP 服务并等待退出信号。
//
// 收到 SIGTERM / SIGINT（systemctl stop/restart、docker stop、Ctrl+C）时：
// 停止接收新请求 → 给在途请求最多 shutdownGrace 收尾 → 把仍然有效的会话写入
// 数据目录的快照，下次启动自动恢复，升级与灰度发版不会把用户踢回登录页。
// kill -9 不给进程执行任何代码的机会，那种情况下没有快照，所有人需要重新登录。
func serve(hs *http.Server, srv *server.Server) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- hs.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
		return
	case <-ctx.Done():
	}

	log.Print("收到退出信号，正在停止服务…")
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := hs.Shutdown(shutCtx); err != nil {
		// 大文件上传/下载可能还没结束：到时即中断，不影响随后保存会话
		log.Printf("等待在途请求结束超时，已中断剩余连接: %v", err)
	}
	srv.SaveSessions()
	log.Print("已退出")
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Printf(`GoWeb %s — S3/OSS 目录浏览

用法: goweb [参数]

参数:
  --ignore-email   不通过 SMTP 投递，登录验证码直接打印到控制台
                   （其余登录流程不变；仅供内网调试，勿用于生产）
  -v, --version    显示版本
  -h, --help       显示本帮助

环境变量:
  GOWEB_LISTEN     监听地址，默认 :8080
  GOWEB_DATA_DIR   数据目录，默认 data
  GOWEB_DEBUG_CODE 设为 1 时，仅在邮件投递失败的情况下退回到
                   --ignore-email 的行为（验证码打印到控制台并允许继续登录）；
                   未设置则投递失败即报错，不允许继续
`, version)
}
