package server

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderQRASCII(t *testing.T) {
	url := "https://example.com/cli-auth.html?s=cas_abcdef1234567890"
	out, err := renderQRASCII(url)
	if err != nil {
		t.Fatalf("renderQRASCII: %v", err)
	}
	if out == "" {
		t.Fatal("输出为空")
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 10 {
		t.Errorf("二维码行数太少: %d", len(lines))
	}
	// 每行宽度应一致
	w0 := utf8.RuneCountInString(lines[0])
	for i, l := range lines {
		w := utf8.RuneCountInString(l)
		if w != w0 {
			t.Errorf("第 %d 行宽度 %d != %d", i, w, w0)
		}
	}
	// 应只包含半块字符和空格
	for _, l := range lines {
		for _, c := range l {
			switch c {
			case '▀', '▄', '█', ' ':
			default:
				t.Errorf("非法字符 %q in %q", c, l)
				break
			}
		}
	}
	// 应至少有一个实心块（二维码不可能全空）
	if !strings.Contains(out, "█") && !strings.Contains(out, "▀") && !strings.Contains(out, "▄") {
		t.Error("二维码全空，疑似渲染失败")
	}
	// 宽度应在 80 列以内
	if w0 > 80 {
		t.Errorf("二维码宽度 %d 超过 80 列", w0)
	}
}

func TestRenderQRASCIIDifferentURLs(t *testing.T) {
	a, _ := renderQRASCII("https://a.com/cli-auth.html?s=cas_a")
	b, _ := renderQRASCII("https://b.com/cli-auth.html?s=cas_b")
	if a == b {
		t.Error("不同 URL 渲染出相同二维码")
	}
}
