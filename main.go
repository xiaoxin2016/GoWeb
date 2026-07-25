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

	if len(os.Args) > 1 && (os.Args[1] == "-v" || os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("GoWeb", version)
		return
	}

	store, err := config.Load(dataDir)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	srv, err := server.New(store)
	if err != nil {
		log.Fatalf("初始化服务失败: %v", err)
	}

	log.Printf("GoWeb %s 已启动，监听 %s", version, addr)
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
