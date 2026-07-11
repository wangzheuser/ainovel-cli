package flow

import (
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func TestResolvePausePoint(t *testing.T) {
	pp := &domain.PausePoint{After: domain.PauseAfterRewritesDrained, Reason: "重写第3章"}
	cases := []struct {
		name string
		pp   *domain.PausePoint
		p    *domain.Progress
		want PauseResolution
	}{
		{"无停靠点", nil, &domain.Progress{Phase: domain.PhaseWriting}, PauseKeep},
		{"未知触发条件保守保留", &domain.PausePoint{After: "chapter_committed"}, &domain.Progress{Phase: domain.PhaseWriting}, PauseKeep},
		{"progress 缺失保守保留", pp, nil, PauseKeep},
		{"队列未排空", pp, &domain.Progress{Phase: domain.PhaseWriting, PendingRewrites: []int{3}}, PauseKeep},
		{"排空且写作中→消费并停机", pp, &domain.Progress{Phase: domain.PhaseWriting}, PauseConsumeAndStop},
		{"排空但已完本→只消费", pp, &domain.Progress{Phase: domain.PhaseComplete}, PauseConsume},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolvePausePoint(tc.pp, tc.p); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}
