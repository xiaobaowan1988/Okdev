package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/xiaobaowan1988/okdev/internal/api/v1"
	"github.com/xiaobaowan1988/okdev/internal/deployer"
)

// PhaseOf 把 deployer 运行的状态映射成任务阶段。
func PhaseOf(stage string) v1.Phase {
	switch stage {
	case deployer.StatusPending:
		return v1.PhasePending
	case deployer.StatusSucceeded:
		return v1.PhaseSuccess
	case deployer.StatusFailed, deployer.StatusCancelled:
		return v1.PhaseFailed
	default: // building、deploying,以及将来可能新增的中间状态
		return v1.PhaseRunning
	}
}

// StatusFromRun 根据 deployer 运行算出任务的新状态,是纯函数。终态不会被改写。
func StatusFromRun(cur v1.ReleaseTaskStatus, run *deployer.Run, now metav1.Time) v1.ReleaseTaskStatus {
	if cur.Phase.Terminal() {
		return cur
	}
	st := cur
	st.RunID = run.ID
	st.Stage = run.Status
	st.Phase = PhaseOf(run.Status)
	st.Image = run.Image
	st.Message = run.Message
	if run.StartedAt != nil {
		t := metav1.NewTime(*run.StartedAt)
		st.StartTime = &t
	}
	if st.Phase.Terminal() {
		done := now
		if run.FinishedAt != nil {
			done = metav1.NewTime(*run.FinishedAt)
		}
		st.CompletionTime = &done
		if st.Phase == v1.PhaseFailed && st.Message == "" {
			st.Message = "run " + run.Status
		}
	}
	return st
}
