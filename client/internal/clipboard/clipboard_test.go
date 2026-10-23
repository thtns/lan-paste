package clipboard

import (
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// 模拟国内应用写入 GBK 字节的剪贴板:自动转码回正确中文。
func TestFixEncodingGBK(t *testing.T) {
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("测试一下你好世界"))
	if err != nil {
		t.Fatal(err)
	}
	if utf8.Valid(gbk) {
		t.Fatalf("GBK 字节不应是合法 UTF-8")
	}
	got := FixEncoding(string(gbk))
	if got != "测试一下你好世界" {
		t.Fatalf("GBK 转码结果不符: %q", got)
	}
}

// 合法 UTF-8 原样返回(不改动)。
func TestFixEncodingValidUTF8(t *testing.T) {
	const s = "各位 NAS 玩家们周四好！🎉 https://club.fnnas.com/"
	if got := FixEncoding(s); got != s {
		t.Fatalf("合法 UTF-8 不应被改动: %q", got)
	}
}

// 纯 ASCII 原样返回。
func TestFixEncodingASCII(t *testing.T) {
	const s = "hello world 123"
	if got := FixEncoding(s); got != s {
		t.Fatalf("ASCII 不应被改动: %q", got)
	}
}
