package playbook

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
)

// FilesDigest 返回包体的 canonical sha256（`sha256:<64 hex>`）：`{相对路径: 正文}` 按路径排序、逐条写「路径长度:路径正文长度:正文」再取哈希（键序与排版无关，正文变一个字节就变）。
// 它是回执 `playbook.filesDigest` 与版本历史 `digest` 列的唯一算法 —— 两处不同源就会出现「装的和记的不是同一版」。
func FilesDigest(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		body := files[p]
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte(":"))
		h.Write([]byte(p))
		h.Write([]byte(strconv.Itoa(len(body))))
		h.Write([]byte(":"))
		h.Write([]byte(body))
	}
	return DigestPrefix + hex.EncodeToString(h.Sum(nil))
}
