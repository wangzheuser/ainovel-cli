package flow

// Route 状态空间穷举测试。
//
// expectedInstruction 是决策表的独立镜像（可执行规格，对应 architecture.md 铁律二
// 的 11 分支优先级），故意不复用实现的任何代码：实现重构后行为若有偏移，这里立刻
// 红灯；要改变行为必须同时改动规格并留下 diff。router_test.go 的单分支用例负责
// 可读的意图文档，本文件负责全组合空间下的优先级与守恒性质。

import (
	"reflect"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

// expectKind 是规格层面的裁定结果：路由到谁、做什么类别的事。
type expectKind int

const (
	expectNil expectKind = iota
	expectRewrite
	expectArcReview
	expectArcSummary
	expectVolumeSummary
	expectExpandArc
	expectNewVolume
	expectNextChapter
)

// expectedInstruction 按架构规格计算某 State 应得的裁定。
// 优先级（自上而下第一个命中）：
//  1. Progress 缺失 / Phase 终态或非写作期 → LLM 裁定（nil）
//  2. 重写/打磨队列非空 → writer 按队列头（绝对优先，压过一切弧末事务）
//  3. Flow=Reviewing / Steering → LLM 裁定（nil）
//  4. 分层模式弧末 → 评审 → 弧摘要 → (卷末)卷摘要 → 展开下一弧 → 追加新卷
//  5. 其余 → writer 续写下一章
func expectedInstruction(s State) expectKind {
	p := s.Progress
	if p == nil || p.Phase != domain.PhaseWriting {
		return expectNil
	}
	if len(p.PendingRewrites) > 0 {
		return expectRewrite
	}
	if p.Flow == domain.FlowReviewing || p.Flow == domain.FlowSteering {
		return expectNil
	}
	if p.Layered && s.ArcBoundary != nil && s.ArcBoundary.IsArcEnd {
		b := s.ArcBoundary
		switch {
		case !s.HasArcReview:
			return expectArcReview
		case !s.HasArcSummary:
			return expectArcSummary
		case b.IsVolumeEnd && !s.HasVolumeSummary:
			return expectVolumeSummary
		case b.NeedsExpansion && b.NextArc > 0:
			return expectExpandArc
		case b.NeedsNewVolume:
			return expectNewVolume
		}
	}
	return expectNextChapter
}

// classify 把实现返回的 Instruction 归到规格类别；不认识的组合直接失败。
func classify(t *testing.T, inst *Instruction) expectKind {
	t.Helper()
	if inst == nil {
		return expectNil
	}
	switch inst.Agent {
	case "writer":
		switch {
		case contains(inst.Task, "重写") || contains(inst.Task, "打磨"):
			return expectRewrite
		case contains(inst.Task, "写第"):
			return expectNextChapter
		}
	case "editor":
		switch {
		case contains(inst.Task, "弧级评审"):
			return expectArcReview
		case contains(inst.Task, "save_arc_summary"):
			return expectArcSummary
		case contains(inst.Task, "save_volume_summary"):
			return expectVolumeSummary
		}
	case "architect_long":
		switch {
		case contains(inst.Task, "expand_arc"):
			return expectExpandArc
		case contains(inst.Task, "append_volume"):
			return expectNewVolume
		}
	}
	t.Fatalf("无法归类的指令：agent=%q task=%q", inst.Agent, inst.Task)
	return expectNil
}

// boundaryCase 是弧边界维度的一个枚举点：边界形态 + 三个摘要事实。
type boundaryCase struct {
	name             string
	boundary         *storepkg.ArcBoundary
	hasArcReview     bool
	hasArcSummary    bool
	hasVolumeSummary bool
}

func enumerateBoundaryCases() []boundaryCase {
	cases := []boundaryCase{
		{name: "no-boundary"},
		{name: "mid-arc", boundary: &storepkg.ArcBoundary{Volume: 1, Arc: 1}},
	}
	type volCase struct {
		name       string
		volumeEnd  bool
		volSummary bool
	}
	type followCase struct {
		name      string
		expansion bool
		nextArc   int
		newVolume bool
	}
	volCases := []volCase{
		{name: "vol-mid", volumeEnd: false},
		{name: "vol-end-nosum", volumeEnd: true, volSummary: false},
		{name: "vol-end-sum", volumeEnd: true, volSummary: true},
	}
	followCases := []followCase{
		{name: "settled"},
		{name: "expand", expansion: true, nextArc: 4},
		{name: "expand-no-nextarc", expansion: true, nextArc: 0}, // 展开位缺失 → 不可展开
		{name: "new-volume", newVolume: true},
	}
	for _, review := range []bool{false, true} {
		for _, summary := range []bool{false, true} {
			for _, vc := range volCases {
				for _, fc := range followCases {
					cases = append(cases, boundaryCase{
						name: fmtBool("rev", review) + fmtBool("+sum", summary) + "+" + vc.name + "+" + fc.name,
						boundary: &storepkg.ArcBoundary{
							IsArcEnd:       true,
							IsVolumeEnd:    vc.volumeEnd,
							Volume:         2,
							Arc:            3,
							NextVolume:     2,
							NextArc:        fc.nextArc,
							NeedsExpansion: fc.expansion,
							NeedsNewVolume: fc.newVolume,
						},
						hasArcReview:     review,
						hasArcSummary:    summary,
						hasVolumeSummary: vc.volSummary,
					})
				}
			}
		}
	}
	return cases
}

func fmtBool(label string, v bool) string {
	if v {
		return label
	}
	return label + "!"
}

func TestRoute_ExhaustiveAgainstSpec(t *testing.T) {
	phases := []domain.Phase{domain.PhaseInit, domain.PhasePremise, domain.PhaseOutline, domain.PhaseWriting, domain.PhaseComplete}
	flows := []domain.FlowState{domain.FlowWriting, domain.FlowReviewing, domain.FlowRewriting, domain.FlowPolishing, domain.FlowSteering}
	queues := [][]int{nil, {7, 9}}
	completedSets := [][]int{nil, {1, 2, 3}}

	total := 0
	for _, phase := range phases {
		for _, fl := range flows {
			for _, queue := range queues {
				for _, layered := range []bool{false, true} {
					for _, completed := range completedSets {
						for _, bc := range enumerateBoundaryCases() {
							total++
							p := &domain.Progress{
								Phase:             phase,
								Flow:              fl,
								Layered:           layered,
								CompletedChapters: append([]int(nil), completed...),
								PendingRewrites:   append([]int(nil), queue...),
							}
							last := 0
							if n := len(completed); n > 0 {
								last = completed[n-1]
							}
							s := State{
								Progress:         p,
								LastCompleted:    last,
								ArcBoundary:      bc.boundary,
								HasArcReview:     bc.hasArcReview,
								HasArcSummary:    bc.hasArcSummary,
								HasVolumeSummary: bc.hasVolumeSummary,
							}

							before := snapshotState(s)
							inst := Route(s)
							want := expectedInstruction(s)
							got := classify(t, inst)
							if got != want {
								t.Fatalf("phase=%s flow=%s queue=%v layered=%v completed=%v boundary=%s:\n规格期望 %d，实现返回 %d（inst=%+v）",
									phase, fl, queue, layered, completed, bc.name, want, got, inst)
							}
							assertConservation(t, s, inst)
							if !reflect.DeepEqual(before, snapshotState(s)) {
								t.Fatalf("Route 必须是纯函数，不得改写输入 State（boundary=%s）", bc.name)
							}
							if again := Route(s); !reflect.DeepEqual(inst, again) {
								t.Fatalf("Route 必须确定：两次调用结果不同（boundary=%s）", bc.name)
							}
						}
					}
				}
			}
		}
	}
	if total < 5000 {
		t.Fatalf("枚举空间意外缩水（%d 组合），检查维度枚举", total)
	}
}

// assertConservation 与具体分支无关的守恒性质。
func assertConservation(t *testing.T, s State, inst *Instruction) {
	t.Helper()
	if inst == nil {
		return
	}
	p := s.Progress
	if p == nil || p.Phase != domain.PhaseWriting {
		t.Fatalf("非写作期不得产生指令：%+v", inst)
	}
	switch inst.Agent {
	case "writer":
		if inst.Chapter <= 0 {
			t.Fatalf("writer 指令必须带章节号：%+v", inst)
		}
		if len(p.PendingRewrites) > 0 {
			if inst.Chapter != p.PendingRewrites[0] {
				t.Fatalf("重写队列非空时必须派队列头 %d，got %d", p.PendingRewrites[0], inst.Chapter)
			}
			wantVerb := "重写"
			if p.Flow == domain.FlowPolishing {
				wantVerb = "打磨"
			}
			if !contains(inst.Task, wantVerb) {
				t.Fatalf("队列任务动词应为 %q：%q", wantVerb, inst.Task)
			}
		} else if inst.Chapter != p.NextChapter() {
			t.Fatalf("续写指令章节号应为 NextChapter=%d，got %d", p.NextChapter(), inst.Chapter)
		}
	case "editor", "architect_long":
		if inst.Chapter != 0 {
			t.Fatalf("%s 指令不应带章节号：%+v", inst.Agent, inst)
		}
	default:
		t.Fatalf("未知路由目标 %q", inst.Agent)
	}
	if inst.Task == "" || inst.Reason == "" {
		t.Fatalf("指令的 Task 与 Reason 都不得为空：%+v", inst)
	}
}

// snapshotState 深拷贝 State 用于纯函数断言。
func snapshotState(s State) State {
	cp := s
	if s.Progress != nil {
		p := *s.Progress
		p.CompletedChapters = append([]int(nil), s.Progress.CompletedChapters...)
		p.PendingRewrites = append([]int(nil), s.Progress.PendingRewrites...)
		cp.Progress = &p
	}
	if s.ArcBoundary != nil {
		b := *s.ArcBoundary
		cp.ArcBoundary = &b
	}
	return cp
}
