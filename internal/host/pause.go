package host

import (
	"log/slog"

	"github.com/voocel/ainovel-cli/internal/flow"
	"github.com/voocel/ainovel-cli/internal/store"
)

// PausePointSentinel 在流程边界执行用户预约的停靠点（RunMeta.PausePoint）。
//
// 合宪定位与 BudgetSentinel 相同（architecture.md §8.4/§10.15）：不评估模型行为——
// 停靠点是 Coordinator 依据用户干预意图预先签署的暂停指令，Host 只在条件满足的
// 边界代为执行。它影响控制流，因此是与 Dispatcher 平级的 Host 政策组件；
// Route/工具层不感知。停靠点一次性：命中即消费，Continue 恢复后不再触发。
type PausePointSentinel struct {
	store  *store.Store
	abort  func(reason string)         // Host 停机包装（带原因事件）
	report func(level, summary string) // 事件出口（emitEvent + notify，由 Host 注入）
}

// NewPausePointSentinel 创建停靠点哨兵；所有方法 nil 安全。
func NewPausePointSentinel(s *store.Store, abort func(reason string), report func(level, summary string)) *PausePointSentinel {
	return &PausePointSentinel{store: s, abort: abort, report: report}
}

// HandleBoundary 在子代理边界裁定停靠点，命中则消费并停机，返回是否已停机
// （true 表示调用方应短路本轮派发）。先清后停：清除失败只告警仍继续停机，
// 安全侧最坏情况是恢复后同一停靠点再暂停一次，绝不会该停不停。
func (s *PausePointSentinel) HandleBoundary() bool {
	res, reason := s.resolve()
	switch res {
	case flow.PauseConsume:
		s.clear()
		s.report("info", withReason("验收停靠点已随完本收尾解除", reason))
		return false
	case flow.PauseConsumeAndStop:
		s.clear()
		s.abort(withReason("返工队列已排空，已自动暂停等待验收；在输入框输入指令即可继续", reason))
		return true
	default:
		return false
	}
}

// ReconcileOnResume 恢复路径对账：停机窗口里停靠点条件已满足但未及消费时
// （崩溃恰在排空后、或预算/Esc 停机抢在边界消费前），用户显式恢复即视为放行——
// 只消费并报告（事件+通知成对），不再补一次暂停。
// 已知窗口：设点后 editor 未及入队即停机，磁盘状态与"真排空"无法区分，对账会
// 连同解除；报告带诉求摘要，用户据此重新下达即可，不为极端窗口引入跨层状态。
func (s *PausePointSentinel) ReconcileOnResume() {
	res, reason := s.resolve()
	if res == flow.PauseKeep {
		return
	}
	s.clear()
	s.report("info", withReason("上次的验收停靠点已完成，恢复时自动解除", reason))
}

func (s *PausePointSentinel) resolve() (flow.PauseResolution, string) {
	if s == nil || s.store == nil {
		return flow.PauseKeep, ""
	}
	meta, err := s.store.RunMeta.Load()
	if err != nil || meta == nil || meta.PausePoint == nil {
		return flow.PauseKeep, ""
	}
	progress, err := s.store.Progress.Load()
	if err != nil {
		return flow.PauseKeep, ""
	}
	return flow.ResolvePausePoint(meta.PausePoint, progress), meta.PausePoint.Reason
}

func (s *PausePointSentinel) clear() {
	if err := s.store.RunMeta.ClearPausePoint(); err != nil {
		slog.Warn("清除停靠点失败", "module", "host", "err", err)
	}
}

// withReason 给消息附上用户诉求摘要；reason 为空时原样返回。
func withReason(msg, reason string) string {
	if reason == "" {
		return msg
	}
	return msg + "（诉求：" + reason + "）"
}
