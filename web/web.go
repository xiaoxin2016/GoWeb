// Package web 内嵌前端模板与静态资源。
package web

import "embed"

//go:embed templates static
var FS embed.FS
