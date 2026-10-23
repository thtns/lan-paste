package syncer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 直接驱动 pollOnce：模拟「引擎刚收到推送写入文本 → 剪贴板读回行尾被改写」
// （真实案例：网页上传 7 字节 "nihao\n\n"，Windows 客户端读回 8 字节 CRLF 版本），
// 文本守卫必须拦住上传（计数服务端零请求）；真正不同的新内容必须放行。
func TestPollTextRoundTripGuardDirect(t *testing.T) {
	uploads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	clip := &fakeClipboard{}
	e := New(Options{ServerURL: srv.URL, Token: "t", Device: "d", PollInterval: 10 * 1e6, Logger: quietLogger()}, clip)

	// 模拟远端推送写入成功后的状态（handleRemote 文本路径）
	e.mu.Lock()
	e.lastWriteText = "nihao\n\n"
	e.mu.Unlock()

	// 剪贴板读回的行尾变异：纯 CRLF 转换（\r 剥离归一化后与原文一致）
	for _, variant := range []string{"nihao\n\n", "nihao\r\n\r\n", "nihao\r\n\r\n"} {
		clip.SetText(variant)
		e.pollOnce(context.Background())
	}
	if uploads != 0 {
		t.Fatalf("文本守卫未拦截：发生了 %d 次上传", uploads)
	}

	// 真正不同的新内容必须放行
	clip.SetText("hello world")
	e.pollOnce(context.Background())
	if uploads != 1 {
		t.Fatalf("新内容应上传 1 次，实际 %d 次", uploads)
	}
}
