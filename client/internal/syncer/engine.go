// Package syncer 实现与平台无关的剪贴板同步引擎：
// 轮询本地剪贴板上传（REST），接收服务端 WebSocket 推送写入本地。
// v0.2 起支持图片：文本通道优先；文本为空时探测图片通道（平台实现 ImageClipboard）。
// 决策逻辑（LoopGuard）与 IO（剪贴板、网络）严格分离，便于单测。
package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg" // 注册 jpeg 解码器（收到 jpeg 图片时转 PNG）
	"image/png"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lanpaste/client/internal/clipboard"
)

// Options 是引擎配置。
type Options struct {
	ServerURL    string        // http://host:port
	Token        string        // Bearer token
	Device       string        // 设备标识（默认 hostname，由 main 解析）
	PollInterval time.Duration // 本地剪贴板轮询间隔，<=0 时取 500ms
	Logger       *log.Logger   // 为 nil 时输出到 stderr
	// OnStatus 连接状态变化回调（托盘界面用）：connected=false 表示断线重连中。
	OnStatus func(connected bool, deviceCount int)
}

// Clipboard 是平台剪贴板实现的统一接口（internal/clipboard 提供各平台实现）。
type Clipboard interface {
	ReadText() (string, error)
	WriteText(string) error
}

// pngMagic PNG 文件签名。
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// Engine 平台无关的同步引擎。
// 所有剪贴板读写、LoopGuard 状态、prevPoll/prevImg 的访问都在 e.mu 临界区内，
// 避免轮询与远端写入交错导致的“读到旧内容却当成新变化”竞态；
// 网络调用在临界区外进行，其状态回填可容忍并发写入（详见 pollOnce 注释）。
type Engine struct {
	opts    Options
	clip    Clipboard
	imgClip clipboard.ImageClipboard // 平台支持图片时非 nil
	api     *restClient
	log     *log.Logger

	mu       sync.Mutex
	guard    LoopGuard
	prevPoll string      // 上一次轮询到的本地文本 hash；上传失败时不推进，以驱动下一轮重试
	prevImg  string      // 上一次轮询到的本地图片 hash（图片通道独立跟踪）
	paused   atomic.Bool // 托盘「暂停同步」：双向暂停（不上传也不写入）

	// 最近一次「我们写入系统剪贴板」的图片的像素指纹与尺寸。
	// 系统剪贴板对图片普遍会重编码（macOS osascript 的 PNGf 往返会改变 PNG 字节，
	// Windows DIB 回退也会），字节 hash 对不上时用「像素相同」兜底判定 self-write，
	// 否则收到→写入→读回→再上传会形成无限回环风暴。
	lastWritePix string
	lastWriteW   int
	lastWriteH   int

	// 最近一次「我们写入系统剪贴板」的文本原文。
	// 部分平台的剪贴板往返会改变行尾（\n ↔ \r\n），字节 hash 对不上时
	// 用「忽略 \r 归一化后一致」兜底判定 self-write，原理同上。
	lastWriteText string

	// 重复错误限流：同一错误 30 秒内只记一次日志，避免服务端掉线时刷屏
	readErrLast   string
	readErrAt     time.Time
	uploadErrLast string
	uploadErrAt   time.Time
}

// New 创建引擎。平台实现若同时实现 clipboard.ImageClipboard，图片同步自动启用。
func New(opts Options, clip Clipboard) *Engine {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 500 * time.Millisecond
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	e := &Engine{
		opts: opts,
		clip: clip,
		api:  newRESTClient(opts.ServerURL, opts.Token, opts.Device),
		log:  logger,
	}
	if ic, ok := clip.(clipboard.ImageClipboard); ok {
		e.imgClip = ic
	}
	return e
}

// SetPaused 暂停/恢复双向同步（托盘菜单用）。暂停期间不上传、不写入。
func (e *Engine) SetPaused(p bool) {
	e.paused.Store(p)
	if p {
		e.log.Printf("同步已暂停")
	} else {
		e.log.Printf("同步已恢复")
	}
}

// Paused 返回当前是否处于暂停状态。
func (e *Engine) Paused() bool { return e.paused.Load() }

// Run 阻塞运行直到 ctx 取消。
// 启动时把当前剪贴板内容视为「已知」（写入 lastSeen），
// 避免每次重启都把剪贴板里的旧内容重传一遍。
func (e *Engine) Run(ctx context.Context) {
	if text, err := e.clip.ReadText(); err != nil {
		e.warnReadErr(err)
	} else {
		h := HashText(text)
		e.mu.Lock()
		e.prevPoll = h
		if h != "" {
			e.guard.MarkSeen(h)
		}
		e.mu.Unlock()
	}
	// 图片同理（若有）：标记为已知，避免重启重传当前剪贴板里的旧图
	if e.imgClip != nil {
		if img, err := e.imgClip.ReadImage(); err == nil && img != nil {
			h := HashBytes(img.Data)
			e.mu.Lock()
			e.prevImg = h
			e.guard.MarkSeen(h)
			e.mu.Unlock()
		}
	}

	go e.pollLoop(ctx)
	e.wsLoop(ctx)
}

// pollLoop 按固定间隔轮询本地剪贴板。
func (e *Engine) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(e.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.pollOnce(ctx)
		}
	}
}

// pollOnce 轮询一次本地剪贴板：文本优先，文本为空时探测图片。
//
// 上传期间不持锁；若上传期间远端推送改写了剪贴板（prevPoll/prevImg 被推进到别的
// hash），上传成功后的回填可能暂时覆盖，但下一轮轮询会命中 lastWrittenHash 的
// self-write 分支自动收敛，不会造成回环。
func (e *Engine) pollOnce(ctx context.Context) {
	if e.paused.Load() {
		return
	}
	e.mu.Lock()
	text, err := e.clip.ReadText()
	if err != nil {
		e.mu.Unlock()
		e.warnReadErr(err)
		return
	}

	if text != "" {
		if fixed := clipboard.FixEncoding(text); fixed != text {
			text = fixed
			e.log.Printf("检测到非 UTF-8 剪贴板文本（疑似 GBK），已自动转码")
		}
		e.pollText(ctx, text) // 内部负责解锁
		return
	}

	// 文本为空：探测图片通道（不支持图片的平台 imgClip 为 nil）
	if e.imgClip == nil {
		e.mu.Unlock()
		return
	}
	img, err := e.imgClip.ReadImage()
	if err != nil {
		e.mu.Unlock()
		e.warnReadErr(err)
		return
	}
	if img == nil || len(img.Data) == 0 {
		if e.prevImg != "" {
			e.prevImg = "" // 剪贴板已清空，复位图片跟踪
		}
		e.mu.Unlock()
		return
	}
	e.pollImage(ctx, img) // 内部负责解锁
}

// pollText 文本通道（与 v0.1 行为一致）。调用时持锁，返回时不持锁。
func (e *Engine) pollText(ctx context.Context, text string) {
	h := HashText(text)
	if h == e.prevPoll {
		e.mu.Unlock()
		return
	}
	// 字节变了 ≠ 内容变了：剪贴板往返可能改变行尾（\n ↔ \r\n）。
	// 与「上次我们写入的文本」忽略 \r 归一化后一致 → self-write，不重传。
	if e.lastWriteText != "" && normalizeText(text) == normalizeText(e.lastWriteText) {
		e.guard.MarkSeen(h)
		e.prevPoll = h
		e.mu.Unlock()
		e.log.Printf("忽略上传文本（self-write 归一化一致）sha256:%s", shortHash(h))
		return
	}
	d := e.guard.DecideLocal(h)
	if !d.Upload {
		if d.UpdateSeen {
			e.guard.MarkSeen(h)
		}
		e.prevPoll = h
		e.mu.Unlock()
		if d.Reason == ReasonSelfWrite || d.Reason == ReasonDuplicate {
			e.log.Printf("忽略上传（%s）sha256:%s", d.Reason, shortHash(h))
		}
		return // empty 分支静默：清空剪贴板是高频操作
	}
	e.mu.Unlock()

	clip, dedup, err := e.api.UploadClip(ctx, text)
	if err != nil {
		// prevPoll 未推进，下一轮轮询会再次尝试上传
		e.warnUploadErr(err)
		return
	}

	e.mu.Lock()
	e.prevPoll = h
	e.guard.MarkUploaded(h)
	e.mu.Unlock()
	if dedup {
		e.log.Printf("上传文本 %dB sha256:%s（服务端最新一条相同，已去重未广播）", len(text), shortHash(h))
	} else {
		e.log.Printf("上传文本 %dB sha256:%s → 服务端记录 #%d", len(text), shortHash(h), clip.ID)
	}
}

// pollImage 图片通道。调用时持锁，返回时不持锁。
func (e *Engine) pollImage(ctx context.Context, img *clipboard.Image) {
	h := HashBytes(img.Data)
	if h == e.prevImg {
		e.mu.Unlock()
		return
	}
	// 字节变了 ≠ 内容变了：剪贴板写回读普遍重编码图片。
	// 与「上次我们写入的图片」尺寸且像素一致 → self-write，不重传。
	if img.Width == e.lastWriteW && img.Height == e.lastWriteH && e.lastWritePix != "" {
		if ph := pixelHash(img); ph != "" && ph == e.lastWritePix {
			e.guard.MarkSeen(h)
			e.prevImg = h
			e.mu.Unlock()
			e.log.Printf("忽略上传图片（self-write 像素一致）%dx%d sha256:%s", img.Width, img.Height, shortHash(h))
			return
		}
	}
	d := e.guard.DecideLocal(h)
	if !d.Upload {
		if d.UpdateSeen {
			e.guard.MarkSeen(h)
		}
		e.prevImg = h
		e.mu.Unlock()
		if d.Reason == ReasonSelfWrite || d.Reason == ReasonDuplicate {
			e.log.Printf("忽略上传图片（%s）sha256:%s", d.Reason, shortHash(h))
		}
		return
	}
	e.mu.Unlock()

	clip, dedup, err := e.api.UploadImage(ctx, img)
	if err != nil {
		e.warnUploadErr(err) // prevImg 未推进，下一轮重试
		return
	}

	e.mu.Lock()
	e.prevImg = h
	e.guard.MarkUploaded(h)
	e.mu.Unlock()
	if dedup {
		e.log.Printf("上传图片 %dB %dx%d sha256:%s（服务端去重未广播）", len(img.Data), img.Width, img.Height, shortHash(h))
	} else {
		e.log.Printf("上传图片 %dB %dx%d → 服务端记录 #%d", len(img.Data), img.Width, img.Height, clip.ID)
	}
}

// handleRemote 处理一条服务端 new 事件（防回环规则 1-3）。
func (e *Engine) handleRemote(ctx context.Context, clip *Clip) {
	if e.paused.Load() {
		e.log.Printf("已暂停：忽略来自 %s 的推送（类型 %s）", clip.SourceDevice, clip.Type)
		return
	}
	if clip.Type == "image" {
		e.handleRemoteImage(ctx, clip)
		return
	}
	if clip.Type != "" && clip.Type != "text" {
		e.log.Printf("忽略接收：类型 %q 暂不支持", clip.Type)
		return
	}

	e.mu.Lock()
	local, rerr := e.clip.ReadText()
	if rerr != nil {
		local = "" // 读不到本地内容时仍按规则尝试写入远端内容
	}
	localHash := HashText(local)

	d := e.guard.DecideRemote(clip.Hash, localHash)
	if !d.Write {
		if d.UpdateSeen {
			e.guard.MarkSeen(clip.Hash)
		}
		e.mu.Unlock()
		e.log.Printf("忽略接收（%s）：本地剪贴板已是该内容 sha256:%s", d.Reason, shortHash(clip.Hash))
		return
	}
	werr := e.clip.WriteText(clip.Text)
	if werr == nil {
		e.guard.MarkWritten(clip.Hash)
		e.prevPoll = clip.Hash      // 让轮询侧不再把这次写入当作本地变化
		e.prevImg = ""              // 剪贴板已被文本占据
		e.lastWriteText = clip.Text // 记录写入原文，供轮询侧归一化比对
	}
	e.mu.Unlock()

	if rerr != nil {
		e.warnReadErr(rerr)
	}
	if werr != nil {
		e.log.Printf("错误：写入本地剪贴板失败：%v", werr)
		return
	}
	e.log.Printf("接收文本 %dB sha256:%s 来自 %s（服务端记录 #%d）", len(clip.Text), shortHash(clip.Hash), clip.SourceDevice, clip.ID)
}

// handleRemoteImage 处理图片推送：下载 blob → 统一转 PNG → 写入系统剪贴板。
func (e *Engine) handleRemoteImage(ctx context.Context, clip *Clip) {
	if e.imgClip == nil {
		e.log.Printf("收到来自 %s 的图片（%d 字节）：本平台客户端暂不支持图片，请在网页端查看",
			clip.SourceDevice, clip.Size)
		return
	}
	if clip.BlobURL == "" {
		e.log.Printf("忽略图片事件：记录 #%d 缺少 blob_url（服务端版本过旧？）", clip.ID)
		return
	}

	e.mu.Lock()
	localImg, rerr := e.imgClip.ReadImage()
	localHash := ""
	if rerr == nil && localImg != nil {
		localHash = HashBytes(localImg.Data)
	}
	d := e.guard.DecideRemote(clip.Hash, localHash)
	if !d.Write {
		if d.UpdateSeen {
			e.guard.MarkSeen(clip.Hash)
		}
		e.mu.Unlock()
		e.log.Printf("忽略接收图片（%s）sha256:%s", d.Reason, shortHash(clip.Hash))
		return
	}
	e.mu.Unlock()

	data, err := e.api.DownloadBlob(ctx, clip.ID)
	if err != nil {
		e.log.Printf("错误：下载图片 #%d 失败：%v", clip.ID, err)
		return
	}
	img, err := ensurePNG(data)
	if err != nil {
		e.log.Printf("错误：图片 #%d 转换 PNG 失败：%v", clip.ID, err)
		return
	}

	e.mu.Lock()
	werr := e.imgClip.WriteImage(img)
	if werr == nil {
		e.guard.MarkWritten(clip.Hash)
		e.prevImg = clip.Hash // 图片通道：这次写入不再当作本地新变化
		e.prevPoll = ""       // 剪贴板已被图片占据
		e.lastWritePix = pixelHash(img)
		e.lastWriteW, e.lastWriteH = img.Width, img.Height
	}
	e.mu.Unlock()

	if rerr != nil {
		e.warnReadErr(rerr)
	}
	if werr != nil {
		e.log.Printf("错误：写入本地剪贴板图片失败：%v", werr)
		return
	}
	e.log.Printf("接收图片 %dB %dx%d 来自 %s（服务端记录 #%d）",
		len(img.Data), img.Width, img.Height, clip.SourceDevice, clip.ID)
}

// ensurePNG 把服务端下发的图片字节统一转成 PNG（darwin/linux 写入路径只认 PNG）。
// PNG 原样返回；jpeg/gif 等可解码格式重编码为 PNG（无损公约数）；
// webp 等标准库不支持的格式返回错误（记录 #id 仍可在网页端使用）。
func ensurePNG(data []byte) (*clipboard.Image, error) {
	if bytes.HasPrefix(data, pngMagic) {
		img := &clipboard.Image{Data: data}
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			img.Width, img.Height = cfg.Width, cfg.Height
		}
		return img, nil
	}
	// image.Decode 依赖已注册的解码器：本文件 blank import 了 gif/png/jpeg
	m, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("不支持的图片格式（%w）", err)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, m); err != nil {
		return nil, err
	}
	return &clipboard.Image{Data: out.Bytes(), Width: m.Bounds().Dx(), Height: m.Bounds().Dy()}, nil
}

// normalizeText 忽略 \r 的文本归一化：剪贴板往返可能把 \n 变成 \r\n（或多余 \r），
// 字节 hash 会失配，但可见内容一致。
func normalizeText(s string) string {
	return strings.ReplaceAll(s, "\r", "")
}

// pixelHash 计算图片解码后像素的 sha256（跨重编码稳定的内容指纹）。
// 剪贴板往返会改变 PNG 字节但通常不改变像素；解码失败返回空串（调用方退回字节 hash 判断）。
func pixelHash(img *clipboard.Image) string {
	m, _, err := image.Decode(bytes.NewReader(img.Data))
	if err != nil {
		return ""
	}
	b := m.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), m, b.Min, draw.Src)
	sum := sha256.Sum256(rgba.Pix)
	return hex.EncodeToString(sum[:])
}

func (e *Engine) warnReadErr(err error) {
	e.mu.Lock()
	now := time.Now()
	throttled := err.Error() == e.readErrLast && now.Sub(e.readErrAt) < 30*time.Second
	e.readErrLast, e.readErrAt = err.Error(), now
	e.mu.Unlock()
	if throttled {
		return
	}
	if errors.Is(err, clipboard.ErrUnavailable) {
		e.log.Printf("警告：%v；请安装 wl-clipboard 或 xsel/xclip，客户端继续等待", err)
		return
	}
	e.log.Printf("警告：读取本地剪贴板失败：%v", err)
}

func (e *Engine) warnUploadErr(err error) {
	e.mu.Lock()
	now := time.Now()
	throttled := err.Error() == e.uploadErrLast && now.Sub(e.uploadErrAt) < 30*time.Second
	e.uploadErrLast, e.uploadErrAt = err.Error(), now
	e.mu.Unlock()
	if !throttled {
		e.log.Printf("错误：上传失败（内容未丢，稍后自动重试）：%v", err)
	}
}
