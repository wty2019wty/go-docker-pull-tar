package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type progressLayer struct {
	Name       string
	Total      int64
	Done       int64
	TotalParts int
	DoneParts  int
	Completed  bool
}

// Progress 汇总并渲染所有层的下载进度。
type Progress struct {
	ui *UI

	mu      sync.Mutex
	layers  []*progressLayer
	total   int64
	done    int64
	started time.Time

	lastDone int64
	lastTime time.Time
	speed    float64

	stop        chan struct{}
	stopOnce    sync.Once
	doneCh      chan struct{}
	loopStarted bool
}

// NewProgress 创建进度管理器。
func NewProgress(ui *UI) *Progress {
	return &Progress{
		ui:     ui,
		stop:   make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// AddLayer 注册一个下载层，返回索引。
func (p *Progress) AddLayer(name string, total int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.layers = append(p.layers, &progressLayer{Name: name, Total: total})
	p.total += total
	return len(p.layers) - 1
}

// SetLayerTotal 更新某个层的总大小。
func (p *Progress) SetLayerTotal(idx int, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= len(p.layers) {
		return
	}
	l := p.layers[idx]
	p.total += total - l.Total
	l.Total = total
	if l.Total < 0 {
		l.Total = 0
	}
}

// SetLayerPlan 设置某个层的分片计划。
func (p *Progress) SetLayerPlan(idx, totalParts, doneParts int, doneBytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= len(p.layers) {
		return
	}
	l := p.layers[idx]
	l.TotalParts = totalParts
	l.DoneParts = doneParts
	l.Done = doneBytes
	p.recalcLocked()
}

// Add 增加已下载字节数。
func (p *Progress) Add(idx int, n int64) {
	if n == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= len(p.layers) {
		return
	}
	p.layers[idx].Done += n
	p.done += n
}

// PartDone 标记一个分片完成。
func (p *Progress) PartDone(idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= len(p.layers) {
		return
	}
	p.layers[idx].DoneParts++
}

// ResetLayer 重置某个层的下载进度（用于校验失败后重试）。
func (p *Progress) ResetLayer(idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= len(p.layers) {
		return
	}
	l := p.layers[idx]
	p.done -= l.Done
	if p.done < 0 {
		p.done = 0
	}
	l.Done = 0
	l.DoneParts = 0
	l.Completed = false
}

// MarkLayerDone 将层标记为完成。
func (p *Progress) MarkLayerDone(idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= len(p.layers) {
		return
	}
	l := p.layers[idx]
	if l.Total > 0 && l.Done < l.Total {
		p.done += l.Total - l.Done
		l.Done = l.Total
	}
	l.Completed = true
}

func (p *Progress) recalcLocked() {
	var done int64
	for _, l := range p.layers {
		done += l.Done
	}
	p.done = done
}

// Start 启动渲染循环。
func (p *Progress) Start() {
	p.mu.Lock()
	p.started = time.Now()
	p.lastTime = p.started
	p.loopStarted = true
	p.mu.Unlock()
	go p.loop()
}

// Stop 停止渲染并清除进度行。
func (p *Progress) Stop() {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		running := p.loopStarted
		p.mu.Unlock()
		close(p.stop)
		if running {
			<-p.doneCh
		}
	})
	// 最后再渲染一次，让用户看到 100% 的最终状态。
	p.render(time.Now())
	p.ui.ClearProgress()
}

func (p *Progress) loop() {
	defer close(p.doneCh)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case now := <-ticker.C:
			p.render(now)
		}
	}
}

func (p *Progress) render(now time.Time) {
	p.mu.Lock()
	done := p.done
	total := p.total
	lastDone := p.lastDone
	lastTime := p.lastTime
	speed := p.speed
	started := p.started
	layers := len(p.layers)
	finished := 0
	for _, l := range p.layers {
		if l.Completed {
			finished++
		}
	}
	p.mu.Unlock()

	dt := now.Sub(lastTime).Seconds()
	if dt > 0.2 && done >= lastDone {
		inst := float64(done-lastDone) / dt
		if speed == 0 {
			speed = inst
		} else {
			speed = speed*0.7 + inst*0.3
		}
	}
	if speed <= 0 && done > 0 && !started.IsZero() {
		if elapsed := now.Sub(started).Seconds(); elapsed > 1 {
			speed = float64(done) / elapsed
		}
	}
	p.mu.Lock()
	p.lastDone = done
	p.lastTime = now
	p.speed = speed
	p.mu.Unlock()

	const width = 26
	ratio := 0.0
	if total > 0 {
		ratio = float64(done) / float64(total)
		if ratio > 1 {
			ratio = 1
		}
	}
	filled := int(ratio * width)
	bar := strings.Repeat("=", filled)
	if filled < width {
		bar += ">"
		bar += strings.Repeat(" ", width-filled-1)
	}

	var b strings.Builder
	b.WriteString("下载 ")
	b.WriteString("[")
	b.WriteString(bar)
	b.WriteString("] ")
	if total > 0 {
		fmt.Fprintf(&b, "%5.1f%%  %s / %s  %s/s  层 %d/%d",
			ratio*100, FormatBytes(done), FormatBytes(total), FormatBytes(int64(speed)), finished, layers)
	} else {
		fmt.Fprintf(&b, "%s  %s/s  层 %d/%d",
			FormatBytes(done), FormatBytes(int64(speed)), finished, layers)
	}
	p.ui.Progress(b.String())
}

// FormatBytes 将字节数格式化为人可读形式。
func FormatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), units[exp])
}
