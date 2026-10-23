package syncer

// LoopGuard 逐条实现 PROTOCOL.md「防回环规则」的纯状态机（无 IO）：
//
//  1. 收到 new 事件：取事件内 hash
//  2. 与本地剪贴板当前内容 hash 相同 → 忽略（仅顺手更新 lastSeen）
//  3. 否则写入本地剪贴板，写入成功后记录 lastWrittenHash = hash
//  4. 本地轮询到剪贴板变化：先比 lastWrittenHash（相同 → 仅更新 lastSeen 不上传，
//     这正是自己刚收推送写入的）；再比 lastSeenHash（相同 → 不上传）
//  5. 上传成功后更新 lastSeenHash
//
// 使用方（Engine）负责真正的 IO，并把 IO 结果回填给 Guard。
// 决策与状态更新刻意拆开（Decide* 不改状态，Mark* 才改），
// 便于在写入失败时不污染状态。
type LoopGuard struct {
	lastWrittenHash string // 最近一次因收到推送而写入本地剪贴板的 hash（规则 3）
	lastSeenHash    string // 最近一次确认服务端已知的 hash（规则 4/5）
}

// 决策原因，会出现在日志里。
const (
	ReasonSelfWrite = "self-write" // 自己刚写入的，不上传
	ReasonDuplicate = "duplicate"  // 与已知内容重复
	ReasonNew       = "new"
	ReasonEmpty     = "empty" // 空剪贴板，不上传
)

// LocalDecision 是本地剪贴板发生变化时的决策结果。
type LocalDecision struct {
	Upload     bool   // 是否需要 POST /api/clips
	UpdateSeen bool   // self-write 分支：仅更新 lastSeenHash（规则 4）
	Reason     string // 日志用原因
}

// DecideLocal 对应规则 4/5：轮询到本地 hash 变为 h 时，决定是否上传。
func (g *LoopGuard) DecideLocal(h string) LocalDecision {
	if h == "" {
		return LocalDecision{Reason: ReasonEmpty}
	}
	if h == g.lastWrittenHash {
		return LocalDecision{UpdateSeen: true, Reason: ReasonSelfWrite}
	}
	if h == g.lastSeenHash {
		return LocalDecision{Reason: ReasonDuplicate}
	}
	return LocalDecision{Upload: true, Reason: ReasonNew}
}

// RemoteDecision 是收到 new 事件时的决策结果。
type RemoteDecision struct {
	Write      bool // 是否写入本地剪贴板
	UpdateSeen bool // duplicate 分支：内容服务端已知，顺手更新 lastSeenHash
	Reason     string
}

// DecideRemote 对应规则 1-3：remoteHash 为事件内 hash，
// localHash 为本地剪贴板当前内容的 hash。
func (g *LoopGuard) DecideRemote(remoteHash, localHash string) RemoteDecision {
	if remoteHash == "" || remoteHash == localHash {
		return RemoteDecision{UpdateSeen: remoteHash != "", Reason: ReasonDuplicate}
	}
	return RemoteDecision{Write: true, Reason: ReasonNew}
}

// MarkWritten 在写入本地剪贴板成功后调用（规则 3）。
// 该内容服务端显然已知，故同时更新 lastSeenHash。
func (g *LoopGuard) MarkWritten(h string) {
	g.lastWrittenHash = h
	g.lastSeenHash = h
}

// MarkSeen 记录「服务端已知该内容」（规则 4 的仅更新 lastSeen）。
func (g *LoopGuard) MarkSeen(h string) { g.lastSeenHash = h }

// MarkUploaded 在上传成功后调用（规则 5）。
func (g *LoopGuard) MarkUploaded(h string) { g.lastSeenHash = h }

// LastWrittenHash / LastSeenHash 仅供测试与调试观察。
func (g *LoopGuard) LastWrittenHash() string { return g.lastWrittenHash }
func (g *LoopGuard) LastSeenHash() string    { return g.lastSeenHash }
