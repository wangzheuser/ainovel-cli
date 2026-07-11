package flow

import (
	"github.com/voocel/ainovel-cli/internal/domain"
)

// PauseResolution 停靠点在当前事实下的裁定结果。
type PauseResolution int

const (
	// PauseKeep 条件未满足，停靠点保留。
	PauseKeep PauseResolution = iota
	// PauseConsume 条件已满足但不停机（如完本收尾优先，run 自然结束即验收点），
	// 只消费停靠点防止残留误触发下一次 run。
	PauseConsume
	// PauseConsumeAndStop 条件已满足，消费停靠点并暂停运行。
	PauseConsumeAndStop
)

// ResolvePausePoint 按当前事实裁定停靠点去向。与 Route 同纪律：纯函数、零 IO，
// 事实不全时保守返回 Keep（宁可晚停一拍，不做误判消费）。
func ResolvePausePoint(pp *domain.PausePoint, p *domain.Progress) PauseResolution {
	if pp == nil || pp.After != domain.PauseAfterRewritesDrained {
		return PauseKeep
	}
	if p == nil {
		return PauseKeep
	}
	if len(p.PendingRewrites) > 0 {
		return PauseKeep
	}
	if p.Phase == domain.PhaseComplete {
		return PauseConsume
	}
	return PauseConsumeAndStop
}
