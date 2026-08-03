package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// distFS 内嵌前端产物。构建单二进制前由指纹脚本生成：
//
//	node scripts/hash.mjs web internal/server/dist
//
// dist 为构建产物（gitignore），开发期用 StaticDir 走本地目录即可编译。

//go:embed all:dist
var distFS embed.FS

// embeddedStatic 返回内嵌的 dist 静态文件系统。
func embeddedStatic() (http.FileSystem, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, err
	}
	return http.FS(sub), nil
}
