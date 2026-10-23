package syncer

import "testing"

// 逐条覆盖 PROTOCOL.md「防回环规则」1-5。
func TestLoopGuardProtocolRules(t *testing.T) {
	t.Run("规则4-5：本地变化→上传，上传后 lastSeen 更新使重复内容不再上传", func(t *testing.T) {
		g := &LoopGuard{}
		h := HashText("hello")

		d := g.DecideLocal(h)
		if !d.Upload || d.Reason != ReasonNew {
			t.Fatalf("首次本地变化应上传，得到 %+v", d)
		}
		g.MarkUploaded(h)
		if g.LastSeenHash() != h {
			t.Fatalf("上传成功后 lastSeenHash 应更新为 %s，得到 %s", h, g.LastSeenHash())
		}

		// 同一内容再次出现在本地（例如轮询重复、或剪贴板切走又切回且未被 prevPoll 拦截）
		d = g.DecideLocal(h)
		if d.Upload || d.Reason != ReasonDuplicate {
			t.Fatalf("与 lastSeen 相同应忽略（duplicate），得到 %+v", d)
		}
	})

	t.Run("规则3-4：收到自己刚写入的→忽略 self-write 并仅更新 lastSeen", func(t *testing.T) {
		g := &LoopGuard{}
		localOld := HashText("old")
		g.MarkSeen(localOld)

		remote := HashText("from server")
		// 收到与本地不同的推送 → 应写入
		if d := g.DecideRemote(remote, localOld); !d.Write {
			t.Fatalf("与本地不同的推送应写入，得到 %+v", d)
		}
		g.MarkWritten(remote)
		if g.LastWrittenHash() != remote || g.LastSeenHash() != remote {
			t.Fatalf("写入后 lastWritten/lastSeen 应更新，得到 %s/%s", g.LastWrittenHash(), g.LastSeenHash())
		}

		// 轮询侧看到自己刚写的内容 → self-write 分支：不上传，仅更新 lastSeen
		d := g.DecideLocal(remote)
		if d.Upload || !d.UpdateSeen || d.Reason != ReasonSelfWrite {
			t.Fatalf("自己刚写入的应命中 self-write 分支，得到 %+v", d)
		}
		g.MarkSeen(remote)
	})

	t.Run("规则1-2：收到与本地剪贴板相同的 hash→忽略 duplicate 并更新 lastSeen", func(t *testing.T) {
		g := &LoopGuard{}
		local := HashText("same content")
		g.MarkSeen(HashText("older"))

		d := g.DecideRemote(local, local)
		if d.Write || !d.UpdateSeen || d.Reason != ReasonDuplicate {
			t.Fatalf("与本地相同的推送应忽略（duplicate），得到 %+v", d)
		}
		g.MarkSeen(local)
		if g.LastSeenHash() != local {
			t.Fatalf("duplicate 分支应顺手更新 lastSeen，得到 %s", g.LastSeenHash())
		}
	})

	t.Run("空内容不上传", func(t *testing.T) {
		g := &LoopGuard{}
		if d := g.DecideLocal(""); d.Upload || d.Reason != ReasonEmpty {
			t.Fatalf("空剪贴板不应上传，得到 %+v", d)
		}
	})

	t.Run("lastWritten 优先于 lastSeen 判定", func(t *testing.T) {
		g := &LoopGuard{}
		h := HashText("x")
		// 即使 lastSeen 也等于 h，self-write 语义（规则4的先后顺序）应先命中
		g.MarkWritten(h)
		d := g.DecideLocal(h)
		if d.Upload || d.Reason != ReasonSelfWrite {
			t.Fatalf("应先命中 self-write 分支，得到 %+v", d)
		}
	})
}

// 完整时序演练：A 的内容经写入/上传状态流转后，B 的对称视角。
func TestLoopGuardRoundTrip(t *testing.T) {
	ga := &LoopGuard{} // 上传方
	gb := &LoopGuard{} // 接收方

	text := "shared snippet"
	h := HashText(text)

	// A：本地变化 → 上传 → lastSeen=h
	if d := ga.DecideLocal(h); !d.Upload {
		t.Fatalf("A 应上传: %+v", d)
	}
	ga.MarkUploaded(h)

	// B：收到推送（本地是别的内容）→ 写入 → lastWritten=h
	if d := gb.DecideRemote(h, HashText("b 的旧内容")); !d.Write {
		t.Fatalf("B 应写入: %+v", d)
	}
	gb.MarkWritten(h)

	// B：轮询发现自己刚写入的内容 → self-write 忽略，不回传
	if d := gb.DecideLocal(h); d.Upload {
		t.Fatalf("B 不应把刚写入的内容再上传: %+v", d)
	}

	// A：若 A 也通过其他途径收到该 hash（本地已是该内容）→ duplicate 忽略
	if d := ga.DecideRemote(h, h); d.Write {
		t.Fatalf("A 本地已是该内容应忽略: %+v", d)
	}
}
