//go:build !embeddist

package server

import (
	"fmt"
	"net/http"
)

// embedDist 为 false：默认构建不内嵌前端。
// //go:embed 在编译期就要求 dist 存在，而 dist 是 gitignore 的指纹产物。
// 开发期用 ANOTIFY_STATIC（如 ./web）走本地目录，因此 go run / go test 不必先 make fe。
// 生产单二进制使用 go build -tags embeddist（make build、Docker、e2e、make ci）。
const embedDist = false

// embeddedStatic 在默认构建中没有内嵌资源。调用方应改走 StaticDir。
func embeddedStatic() (http.FileSystem, error) {
	return nil, fmt.Errorf("此构建未内嵌前端（设置 ANOTIFY_STATIC，或用 -tags embeddist 重新构建）")
}
