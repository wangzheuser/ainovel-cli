package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/flow"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

func TestDispatcher_TrackRepeat(t *testing.T) {
	// 不需要真实 coordinator / store；trackRepeat 只读自己的缓存。
	d := &Dispatcher{}
	inst := &flow.Instruction{Agent: "writer", Task: "写第 5 章", Reason: "续写"}
	if got := d.trackRepeat(inst); got != 1 {
		t.Fatalf("首次下达应计 1，got %d", got)
	}
	if got := d.trackRepeat(inst); got != 2 {
		t.Fatalf("同 Agent+Task 重复下达应计 2，got %d", got)
	}
	// Reason 不同、Agent+Task 相同时视为同一指令继续累计
	sameTaskDiffReason := &flow.Instruction{Agent: "writer", Task: "写第 5 章", Reason: "弧末后继续"}
	if got := d.trackRepeat(sameTaskDiffReason); got != 3 {
		t.Fatalf("仅 Reason 不同应视为重复累计到 3，got %d", got)
	}
	other := &flow.Instruction{Agent: "writer", Task: "写第 6 章", Reason: "续写"}
	if got := d.trackRepeat(other); got != 1 {
		t.Fatalf("Task 变更后应重置为 1，got %d", got)
	}
	d.ResetRepeat()
	if got := d.trackRepeat(other); got != 1 {
		t.Fatalf("ResetRepeat 后首次应计 1，got %d", got)
	}
}

func TestFormatDispatchMessage_RepeatNotice(t *testing.T) {
	inst := &flow.Instruction{Agent: "writer", Task: "写第 5 章", Reason: "续写"}
	first := formatDispatchMessage(inst, 1)
	if first != flow.FormatMessage(inst) {
		t.Fatalf("首次下达不应附加重复注记: %s", first)
	}
	third := formatDispatchMessage(inst, 3)
	for _, want := range []string{"第 3 次下达", "路由事实未变化", "novel_context", "改派"} {
		if !strings.Contains(third, want) {
			t.Errorf("重复注记缺少 %q: %s", want, third)
		}
	}
}

func TestDispatcher_OnRepeatFiresOnceAtThreshold(t *testing.T) {
	d := &Dispatcher{}
	var fired []string
	d.SetOnRepeat(func(agent, task string, n int) {
		fired = append(fired, fmt.Sprintf("%s|%s|%d", agent, task, n))
	})

	inst := &flow.Instruction{Agent: "writer", Task: "写第 5 章"}
	for range 6 {
		d.trackRepeat(inst) // n=1..6：只在 n==3 时回调一次
	}
	if len(fired) != 1 || fired[0] != fmt.Sprintf("writer|写第 5 章|%d", repeatNotifyAt) {
		t.Fatalf("应恰好在第 %d 次触发一次，got %v", repeatNotifyAt, fired)
	}

	// 键变更后重新武装：换任务再连续 3 次 → 再触发一次
	other := &flow.Instruction{Agent: "writer", Task: "写第 6 章"}
	for range 3 {
		d.trackRepeat(other)
	}
	if len(fired) != 2 {
		t.Fatalf("键变更后应重新武装，got %v", fired)
	}
}

func TestDispatcher_SteersAfterSuccessfulBoundaryToolBeforeNextModelCall(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	if err := st.Progress.Init("test", 3); err != nil {
		t.Fatalf("init progress: %v", err)
	}

	var secondReq *agentcore.LLMRequest
	var dispatcher *Dispatcher
	coordinator := agentcore.NewAgent(
		agentcore.WithModel(sequentialDispatchTestModel(func(i int, req *agentcore.LLMRequest) (*agentcore.LLMResponse, error) {
			if i == 0 {
				return &agentcore.LLMResponse{Message: dispatchTestToolCallMsg(agentcore.ToolCall{
					ID:   "tc-subagent",
					Name: "subagent",
					Args: json.RawMessage(`{"agent":"architect_long","task":"plan"}`),
				})}, nil
			}
			secondReq = req
			return &agentcore.LLMResponse{Message: dispatchTestAssistantMsg("done", agentcore.StopReasonStop)}, nil
		})),
		agentcore.WithTools(agentcore.NewFuncTool("subagent", "fake subagent", map[string]any{
			"type": "object",
		}, func(context.Context, json.RawMessage) (json.RawMessage, error) {
			if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
				return nil, err
			}
			return json.RawMessage(`"foundation_ready=true"`), nil
		})),
		agentcore.WithMiddlewares(func(ctx context.Context, call agentcore.ToolCall, next agentcore.ToolExecuteFunc) (json.RawMessage, error) {
			out, err := next(ctx, call.Args)
			if err == nil && call.Name == "subagent" {
				dispatcher.Dispatch()
			}
			return out, err
		}),
	)

	dispatcher = NewDispatcher(coordinator, st)
	dispatcher.Enable()

	if err := coordinator.Prompt(context.Background(), "start"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	coordinator.WaitForIdle()

	if secondReq == nil {
		t.Fatal("expected second model request")
	}
	if len(secondReq.Messages) < 4 {
		t.Fatalf("expected tool result and Host instruction in second request, got %d messages", len(secondReq.Messages))
	}
	if result := secondReq.Messages[len(secondReq.Messages)-2]; result.Role != agentcore.RoleTool {
		t.Fatalf("expected tool result immediately before Host instruction, got %q", result.Role)
	}
	got := secondReq.Messages[len(secondReq.Messages)-1].TextContent()
	for _, want := range []string{"[Host 下达指令]", "subagent(writer", "写第 1 章"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Host instruction missing %q: %s", want, got)
		}
	}
}

type dispatchTestSequentialModel struct {
	fn  func(i int, req *agentcore.LLMRequest) (*agentcore.LLMResponse, error)
	idx int64
}

func sequentialDispatchTestModel(fn func(i int, req *agentcore.LLMRequest) (*agentcore.LLMResponse, error)) *dispatchTestSequentialModel {
	return &dispatchTestSequentialModel{fn: fn}
}

func (m *dispatchTestSequentialModel) take(msgs []agentcore.Message, tools []agentcore.ToolSpec) (*agentcore.LLMResponse, error) {
	i := int(atomic.AddInt64(&m.idx, 1) - 1)
	return m.fn(i, &agentcore.LLMRequest{Messages: msgs, Tools: tools})
}

func (m *dispatchTestSequentialModel) Generate(_ context.Context, msgs []agentcore.Message, tools []agentcore.ToolSpec, _ ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	return m.take(msgs, tools)
}

func (m *dispatchTestSequentialModel) GenerateStream(_ context.Context, msgs []agentcore.Message, tools []agentcore.ToolSpec, _ ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	resp, err := m.take(msgs, tools)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentcore.StreamEvent, 1)
	ch <- agentcore.StreamEvent{Type: agentcore.StreamEventDone, Message: resp.Message, StopReason: resp.Message.StopReason}
	close(ch)
	return ch, nil
}

func (m *dispatchTestSequentialModel) SupportsTools() bool { return true }

func dispatchTestAssistantMsg(text string, stop agentcore.StopReason) agentcore.Message {
	return agentcore.Message{
		Role:       agentcore.RoleAssistant,
		Content:    []agentcore.ContentBlock{agentcore.TextBlock(text)},
		StopReason: stop,
	}
}

func dispatchTestToolCallMsg(calls ...agentcore.ToolCall) agentcore.Message {
	blocks := make([]agentcore.ContentBlock, len(calls))
	for i, call := range calls {
		blocks[i] = agentcore.ToolCallBlock(call)
	}
	return agentcore.Message{
		Role:       agentcore.RoleAssistant,
		Content:    blocks,
		StopReason: agentcore.StopReasonToolUse,
	}
}
