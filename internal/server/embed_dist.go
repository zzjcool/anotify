//go:build embeddist

package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// embedDist 为 true：生产构建内嵌指纹后的前端（make fe → internal/server/dist）。
const embedDist = true

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
