package controller

import (
	"fmt"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/xiaobaowan1988/okdev/internal/api/v1"
)

// ComputeStatus 根据 Job 和它的 Pod 算出任务的新状态,是纯函数。
// job 为 nil 表示 Job 还没创建。终态不会被改写。
func ComputeStatus(cur v1.ReleaseTaskStatus, job *batchv1.Job, pods []*corev1.Pod, now metav1.Time) v1.ReleaseTaskStatus {
	if cur.Phase.Terminal() {
		return cur
	}
	st := cur
	if job == nil {
		if st.Phase == "" {
			st.Phase = v1.PhasePending
		}
		return st
	}
	st.JobName = job.Name
	pod := latestPod(pods)
	if pod != nil {
		st.ExecutorPodName = pod.Name
	}
	if job.Status.StartTime != nil {
		st.StartTime = job.Status.StartTime
	}

	switch {
	case jobCondition(job, batchv1.JobComplete) != nil:
		st.Phase = v1.PhaseSuccess
		st.Message = ""
		st.CompletionTime = completionTime(job, now)
	case jobCondition(job, batchv1.JobFailed) != nil:
		c := jobCondition(job, batchv1.JobFailed)
		st.Phase = v1.PhaseFailed
		st.Message = failureMessage(c, pod)
		st.CompletionTime = completionTime(job, now)
	case pod != nil && pod.Status.Phase == corev1.PodPending:
		st.Phase = v1.PhasePending
	case pod != nil || job.Status.Active > 0:
		st.Phase = v1.PhaseRunning
	default:
		st.Phase = v1.PhasePending
	}
	return st
}

func jobCondition(job *batchv1.Job, t batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range job.Status.Conditions {
		c := &job.Status.Conditions[i]
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

func completionTime(job *batchv1.Job, now metav1.Time) *metav1.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime
	}
	return &now
}

func latestPod(pods []*corev1.Pod) *corev1.Pod {
	if len(pods) == 0 {
		return nil
	}
	sorted := append([]*corev1.Pod(nil), pods...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[j].CreationTimestamp.Before(&sorted[i].CreationTimestamp)
	})
	return sorted[0]
}

// failureMessage 优先报告容器的退出信息,没有就用 Job 条件。
func failureMessage(c *batchv1.JobCondition, pod *corev1.Pod) string {
	if pod != nil {
		statuses := append(append([]corev1.ContainerStatus(nil), pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
				return fmt.Sprintf("container %s exited with code %d: %s", cs.Name, t.ExitCode, firstNonEmpty(lastLine(t.Message), t.Reason))
			}
			if w := cs.State.Waiting; w != nil && (w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull") {
				return fmt.Sprintf("container %s: %s: %s", cs.Name, w.Reason, w.Message)
			}
		}
	}
	return firstNonEmpty(c.Message, c.Reason, "job failed")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// lastLine 取终止消息(日志末尾)里最后一个非空行。
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
