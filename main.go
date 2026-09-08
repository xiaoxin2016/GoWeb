package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/xiaoxin2016/goweb/internal/config"
	"github.com/xiaoxin2016/goweb/internal/server"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入，开发构建为 dev。
var version = "dev"

func main() {
	dataDir := getenv("GOWEB_DATA_DIR", "data")
	addr := getenv("GOWEB_LISTEN", ":8080")

	var opts server.Options
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
	if opts.IgnoreEmail {
		log.Printf("警告：已启用 --ignore-email，登录验证码将直接打印到本控制台而不发送邮件；" +
			"任何能看到本进程日志的人都能登录，请勿用于生产环境")
	}
	if len(store.Get().Auth.AdminEmails) == 0 {
		log.Printf("首次运行：请访问 http://localhost%s/console 完成初始化配置", addr)
	}
	if err := http.ListenAndServe(addr, srv); err != nil {
		log.Fatal(err)
	}
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
`, version)
}
