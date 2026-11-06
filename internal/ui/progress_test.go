package ui

import "testing"

func finishedCount(p *Progress) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, l := range p.layers {
		if l.Completed {
			n++
		}
	}
	return n
}

// TestSpeedExcludesCacheBytes 验证缓存复用与记账调整不计入下载速度。
func TestSpeedExcludesCacheBytes(t *testing.T) {
	p := NewProgress(New(true, false))
	idx := p.AddLayer("abc", 1000)

	// 缓存中已有的 300 字节与回退调整都不应计入网络下载量。
	p.SetLayerPlan(idx, 2, 1, 300)
	p.Add(idx, -50)
	p.AddDownloaded(idx, 200)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.net != 200 {
		t.Fatalf("网络下载量 = %d，期望 200（缓存字节不应计入）", p.net)
	}
	if p.done != 450 {
		t.Fatalf("完成量 = %d，期望 450", p.done)
	}
}

// TestLayerCompletionAccuracy 验证层完成计数随分片完成推进，重置后回退。
func TestLayerCompletionAccuracy(t *testing.T) {
	p := NewProgress(New(true, false))

	// 分片全部来自缓存的层应立即计为完成。
	cached := p.AddLayer("cached", 500)
	p.SetLayerPlan(cached, 2, 2, 500)

	// 正常下载的层随分片完成逐步计为完成。
	downloading := p.AddLayer("downloading", 800)
	p.SetLayerPlan(downloading, 2, 0, 0)
	if got := finishedCount(p); got != 1 {
		t.Fatalf("完成层数 = %d，期望 1", got)
	}

	p.PartDone(downloading)
	if got := finishedCount(p); got != 1 {
		t.Fatalf("分片未全部完成时完成层数 = %d，期望 1", got)
	}
	p.PartDone(downloading)
	if got := finishedCount(p); got != 2 {
		t.Fatalf("分片全部完成后完成层数 = %d，期望 2", got)
	}

	// 校验失败重下时应回退完成计数。
	p.ResetLayer(downloading)
	if got := finishedCount(p); got != 1 {
		t.Fatalf("重置后完成层数 = %d，期望 1", got)
	}
}
