package agents

// agentcore 契约测试：把本项目依赖的框架行为钉成可执行断言。
// 每条测试标注依赖方；bump agentcore 前必须全绿——注释会过时，测试不会。
//
// 已钉死的契约：
//  1. StopAfterTools/StopAfterToolResult 终态退出会经过 StopGuard（StopTriggerAfterTool），
//     guard 否决（InjectMessage）能把 run 拉回继续 —— guard/subagent_guards.go 的任务感知
//     EditorStopGuard 依赖此行为兜住"被派生成摘要却只做了复核"的提前退出。
//  2. StopReasonError / StopReasonAborted 直接终止 run，不触达 StopGuard ——
//     guard/subagent_guards.go 的 hardStopReasons 因此只需列 safety/content_filter。
//  3. provider 拒答（safety 等非 error 停机）会以 end_turn 路径触达 StopGuard，
//     且 info.Message.StopReason 保留原值 —— hardStopReasons 的立即升级依赖此路径。
//  4. StopGuard 返回 InjectMessage 后模型获得新一轮；返回 Escalate 立即终止 ——
//     guard/stop_guard.go 的"物理不可停机"与超限升级依赖此语义。
//  5. WithMaxToolErrors(0) 关闭熔断：工具连续报错不会被禁用 ——
//     build.go 的 Coordinator 依赖"错误显式返回而非静默禁用 subagent 通道"。
//  6. ToolGate 拒绝时 Reason 作为 IsError 工具结果回给模型，run 继续 ——
//     build.go 的 completePhaseGate / writerExpandedChapterGate 依赖"拦截即教学"。

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/subagent"
)

// contractModel 按调用序号返回预设响应的 mock 模型。
type contractModel struct {
	fn  func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error)
	idx int64
}

func (m *contractModel) take(msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
	i := int(atomic.AddInt64(&m.idx, 1) - 1)
	return m.fn(i, msgs)
}

func (m *contractModel) calls() int { return int(atomic.LoadInt64(&m.idx)) }

func (m *contractModel) Generate(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	return m.take(msgs)
}

func (m *contractModel) GenerateStream(_ context.Context, msgs []agentcore.Message, _ []agentcore.ToolSpec, _ ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	resp, err := m.take(msgs)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentcore.StreamEvent, 1)
	ch <- agentcore.StreamEvent{Type: agentcore.StreamEventDone, Message: resp.Message, StopReason: resp.Message.StopReason}
	close(ch)
	return ch, nil
}

func (m *contractModel) SupportsTools() bool { return true }

func assistantText(text string, stop agentcore.StopReason) agentcore.Message {
	return agentcore.Message{
		Role:       agentcore.RoleAssistant,
		Content:    []agentcore.ContentBlock{agentcore.TextBlock(text)},
		StopReason: stop,
	}
}

func assistantToolCall(name string, args string) agentcore.Message {
	return agentcore.Message{
		Role: agentcore.RoleAssistant,
		Content: []agentcore.ContentBlock{agentcore.ToolCallBlock(agentcore.ToolCall{
			ID: "tc-" + name, Name: name, Args: json.RawMessage(args),
		})},
		StopReason: agentcore.StopReasonToolUse,
	}
}

func okTool(name string) agentcore.Tool {
	return agentcore.NewFuncTool(name, "contract test tool", map[string]any{"type": "object"},
		func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"ok"`), nil
		})
}

// runSubagent 用给定配置跑一次 subagent 单派发。返回执行错误——StopGuard 升级
// 终止会以 error 形式浮出（这本身也是契约），期望正常结束的用例自行断言 nil。
func runSubagent(t *testing.T, cfg subagent.Config) error {
	t.Helper()
	tool := subagent.New(cfg)
	_, err := tool.Execute(context.Background(),
		json.RawMessage(`{"agent":"`+cfg.Name+`","task":"contract"}`))
	return err
}

// 契约 1：终态工具退出经过 StopGuard；guard 否决（InjectMessage）后 run 继续。
// 依赖方：EditorStopGuard —— save_review 等终态工具命中后，任务感知 guard 必须
// 有机会把"产物未落盘"的提前退出拉回来。
func TestContract_TerminalToolExitConsultsStopGuard(t *testing.T) {
	var guardCalls atomic.Int32
	var trigger atomic.Value

	model := &contractModel{fn: func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		switch i {
		case 0:
			return &agentcore.LLMResponse{Message: assistantToolCall("finish", `{}`)}, nil
		default:
			// guard 否决终态退出后模型必须获得新一轮；这轮正常结束。
			return &agentcore.LLMResponse{Message: assistantText("done", agentcore.StopReasonStop)}, nil
		}
	}}

	if err := runSubagent(t, subagent.Config{
		Name:           "editorish",
		Description:    "contract",
		Model:          model,
		SystemPrompt:   "test",
		Tools:          []agentcore.Tool{okTool("finish")},
		MaxTurns:       5,
		StopAfterTools: []string{"finish"},
		StopGuardFactory: func(_, _ string) agentcore.StopGuard {
			return func(_ context.Context, info agentcore.StopInfo) agentcore.StopDecision {
				n := guardCalls.Add(1)
				if n == 1 {
					trigger.Store(info.Trigger)
					return agentcore.StopDecision{Allow: false, InjectMessage: "还没落盘，继续"}
				}
				return agentcore.StopDecision{Allow: true}
			}
		},
	}); err != nil {
		t.Fatalf("subagent execute: %v", err)
	}

	if guardCalls.Load() < 2 {
		t.Fatalf("终态工具退出必须触达 StopGuard 且否决后继续（期望 ≥2 次咨询），got %d", guardCalls.Load())
	}
	if got := trigger.Load(); got != agentcore.StopTriggerAfterTool {
		t.Fatalf("终态退出的 Trigger 应为 StopTriggerAfterTool，got %v", got)
	}
	if model.calls() < 2 {
		t.Fatalf("guard 否决后模型应获得新一轮，got %d calls", model.calls())
	}
}

// 契约 2：StopReasonError / StopReasonAborted 直接终止，不触达 StopGuard。
// 依赖方：hardStopReasons 注释——只需处理会真正走到 guard 的拒答语义。
func TestContract_ErrorAndAbortedStopSkipStopGuard(t *testing.T) {
	for _, stop := range []agentcore.StopReason{agentcore.StopReasonError, agentcore.StopReasonAborted} {
		t.Run(string(stop), func(t *testing.T) {
			var guardCalls atomic.Int32
			model := &contractModel{fn: func(int, []agentcore.Message) (*agentcore.LLMResponse, error) {
				return &agentcore.LLMResponse{Message: assistantText("dead", stop)}, nil
			}}
			_ = runSubagent(t, subagent.Config{
				Name: "dying", Description: "contract", Model: model,
				SystemPrompt: "test", MaxTurns: 5,
				StopGuardFactory: func(_, _ string) agentcore.StopGuard {
					return func(context.Context, agentcore.StopInfo) agentcore.StopDecision {
						guardCalls.Add(1)
						return agentcore.StopDecision{Allow: true}
					}
				},
			}) // error/aborted 停机的 error 语义由 subagent 层定义，这里只关心 guard 是否被触达
			if guardCalls.Load() != 0 {
				t.Fatalf("%s 停机不应触达 StopGuard，got %d 次咨询", stop, guardCalls.Load())
			}
		})
	}
}

// 契约 3：provider 拒答（safety 等）走 end_turn 路径触达 StopGuard，
// 且 info.Message.StopReason 保留原值。依赖方：hardStopReasons 的立即升级。
func TestContract_SafetyStopReachesStopGuardWithReason(t *testing.T) {
	var seen atomic.Value
	model := &contractModel{fn: func(int, []agentcore.Message) (*agentcore.LLMResponse, error) {
		return &agentcore.LLMResponse{Message: assistantText("refused", agentcore.StopReason("safety"))}, nil
	}}
	err := runSubagent(t, subagent.Config{
		Name: "refused", Description: "contract", Model: model,
		SystemPrompt: "test", MaxTurns: 5,
		StopGuardFactory: func(_, _ string) agentcore.StopGuard {
			return func(_ context.Context, info agentcore.StopInfo) agentcore.StopDecision {
				seen.Store(info.Message.StopReason)
				return agentcore.StopDecision{Allow: false, Escalate: true}
			}
		},
	})
	if got := seen.Load(); got != agentcore.StopReason("safety") {
		t.Fatalf("StopGuard 应看到原始 stop reason safety，got %v", got)
	}
	if err == nil {
		t.Fatal("Escalate 应以 error 形式浮出到 subagent 调用方")
	}
}

// 契约 4：end_turn 时 InjectMessage 让模型获得新一轮且注入内容在场；
// Escalate 立即终止，模型不再被调用。依赖方：Coordinator StopGuard 的
// "物理不可停机 + 连续超限升级"。
func TestContract_StopGuardInjectContinuesEscalateTerminates(t *testing.T) {
	var sawInject atomic.Bool
	model := &contractModel{fn: func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		if i > 0 {
			for _, m := range msgs {
				if strings.Contains(m.TextContent(), "禁止结束-契约") {
					sawInject.Store(true)
				}
			}
		}
		return &agentcore.LLMResponse{Message: assistantText("try stop", agentcore.StopReasonStop)}, nil
	}}

	var guardCalls atomic.Int32
	err := runSubagent(t, subagent.Config{
		Name: "stubborn", Description: "contract", Model: model,
		SystemPrompt: "test", MaxTurns: 10,
		StopGuardFactory: func(_, _ string) agentcore.StopGuard {
			return func(context.Context, agentcore.StopInfo) agentcore.StopDecision {
				switch guardCalls.Add(1) {
				case 1:
					return agentcore.StopDecision{Allow: false, InjectMessage: "禁止结束-契约"}
				default:
					return agentcore.StopDecision{Allow: false, Escalate: true}
				}
			}
		},
	})
	if err == nil {
		t.Fatal("Escalate 应以 error 形式浮出到 subagent 调用方")
	}

	if !sawInject.Load() {
		t.Fatal("InjectMessage 后模型的下一轮请求里应包含注入消息")
	}
	if guardCalls.Load() != 2 {
		t.Fatalf("期望 guard 恰被咨询 2 次（1 注入 + 1 升级），got %d", guardCalls.Load())
	}
	if model.calls() != 2 {
		t.Fatalf("Escalate 后模型不应再被调用，期望恰 2 次，got %d", model.calls())
	}
}

// 契约 5：WithMaxToolErrors(0) 关闭熔断——工具连续报错不会被禁用，
// 每次调用都真实执行。依赖方：Coordinator 的 subagent 主通道。
func TestContract_MaxToolErrorsZeroNeverDisablesTool(t *testing.T) {
	const attempts = 5
	var execs atomic.Int32
	failing := agentcore.NewFuncTool("flaky", "always fails", map[string]any{"type": "object"},
		func(context.Context, json.RawMessage) (json.RawMessage, error) {
			execs.Add(1)
			return nil, context.DeadlineExceeded // 任意非 retryable 业务错误
		})

	model := &contractModel{fn: func(i int, _ []agentcore.Message) (*agentcore.LLMResponse, error) {
		if i < attempts {
			return &agentcore.LLMResponse{Message: assistantToolCall("flaky", `{}`)}, nil
		}
		return &agentcore.LLMResponse{Message: assistantText("give up", agentcore.StopReasonStop)}, nil
	}}

	agent := agentcore.NewAgent(
		agentcore.WithModel(model),
		agentcore.WithTools(failing),
		agentcore.WithMaxTurns(attempts+2),
		agentcore.WithMaxToolErrors(0),
	)
	if err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	agent.WaitForIdle()

	if got := execs.Load(); got != attempts {
		t.Fatalf("MaxToolErrors(0) 下工具应每次都真实执行（期望 %d 次），got %d——若被熔断说明契约失效", attempts, got)
	}
}

// 契约 6：ToolGate 拒绝时 Reason 作为工具错误结果回给模型，run 继续。
// 依赖方：completePhaseGate / writerExpandedChapterGate 的"拦截即教学"。
func TestContract_ToolGateDenialSurfacesReasonToModel(t *testing.T) {
	const reason = "契约：先展开下一弧再派 writer"
	var sawReason atomic.Bool
	var toolExecuted atomic.Bool

	gated := agentcore.NewFuncTool("gated", "never runs", map[string]any{"type": "object"},
		func(context.Context, json.RawMessage) (json.RawMessage, error) {
			toolExecuted.Store(true)
			return json.RawMessage(`"ran"`), nil
		})

	model := &contractModel{fn: func(i int, msgs []agentcore.Message) (*agentcore.LLMResponse, error) {
		if i == 0 {
			return &agentcore.LLMResponse{Message: assistantToolCall("gated", `{}`)}, nil
		}
		for _, m := range msgs {
			if m.Role == agentcore.RoleTool && strings.Contains(m.TextContent(), reason) {
				sawReason.Store(true)
			}
		}
		return &agentcore.LLMResponse{Message: assistantText("understood", agentcore.StopReasonStop)}, nil
	}}

	agent := agentcore.NewAgent(
		agentcore.WithModel(model),
		agentcore.WithTools(gated),
		agentcore.WithMaxTurns(5),
		agentcore.WithToolGate(func(context.Context, agentcore.GateRequest) (*agentcore.GateDecision, error) {
			return &agentcore.GateDecision{Allowed: false, Reason: reason}, nil
		}),
	)
	if err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	agent.WaitForIdle()

	if toolExecuted.Load() {
		t.Fatal("gate 拒绝后工具不应真实执行")
	}
	if !sawReason.Load() {
		t.Fatal("gate 的 Reason 应作为工具结果回给模型（拦截即教学）")
	}
	if model.calls() < 2 {
		t.Fatalf("gate 拒绝不应终止 run，模型应获得下一轮，got %d calls", model.calls())
	}
}
