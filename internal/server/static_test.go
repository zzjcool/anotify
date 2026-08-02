package server

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		want cacheClass
	}{
		{"/assets/app.e95f1f68.js", cacheImmutable},
		{"/design/tokens.0350ec40.css", cacheImmutable},
		{"/icon.3f3b283e.png", cacheImmutable},
		{"/fonts/caveat.71b9e49b.woff2", cacheImmutable},
		{"/index.html", cacheEntry},
		{"/login.html", cacheEntry},
		{"/sw.js", cacheEntry},
		{"/manifest.json", cacheEntry},
		{"/app.js", cacheEntry}, // 无指纹 → 短缓存
		{"/assets/app.js", cacheEntry},
	}
	for _, c := range cases {
		if got := classify(c.name); got != c.want {
			t.Errorf("classify(%q)=%v want %v", c.name, got, c.want)
		}
	}
}

func TestIsHex(t *testing.T) {
	if !isHex("e95f1f68") {
		t.Error("e95f1f68 应是 hex")
	}
	if isHex("e95f1f6g") {
		t.Error("含 g 不应是 hex")
	}
}
