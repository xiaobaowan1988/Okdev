package controller

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/xiaobaowan1988/okdev/internal/api/v1"
)

var now = metav1.NewTime(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))

func pod(name string, age int, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(now.Add(time.Duration(age) * time.Second))},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func jobWith(cond batchv1.JobConditionType, msg string) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "rt-x"}}
	if cond != "" {
		j.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, Message: msg}}
	}
	return j
}

func TestComputeStatus(t *testing.T) {
	cases := []struct {
		name      string
		cur       v1.ReleaseTaskStatus
		job       *batchv1.Job
		pods      []*corev1.Pod
		wantPhase v1.Phase
		wantPod   string
		wantMsg   string
	}{
		{"no job yet", v1.ReleaseTaskStatus{}, nil, nil, v1.PhasePending, "", ""},
		{"job without pod", v1.ReleaseTaskStatus{}, jobWith("", ""), nil, v1.PhasePending, "", ""},
		{"pod pending", v1.ReleaseTaskStatus{}, jobWith("", ""), []*corev1.Pod{pod("p1", 0, corev1.PodPending)}, v1.PhasePending, "p1", ""},
		{"pod running", v1.ReleaseTaskStatus{}, jobWith("", ""), []*corev1.Pod{pod("p1", 0, corev1.PodRunning)}, v1.PhaseRunning, "p1", ""},
		{"success", v1.ReleaseTaskStatus{Phase: v1.PhaseRunning}, jobWith(batchv1.JobComplete, ""), []*corev1.Pod{pod("p1", 0, corev1.PodSucceeded)}, v1.PhaseSuccess, "p1", ""},
		{"failure uses job message", v1.ReleaseTaskStatus{}, jobWith(batchv1.JobFailed, "DeadlineExceeded"), []*corev1.Pod{pod("p1", 0, corev1.PodFailed)}, v1.PhaseFailed, "p1", "DeadlineExceeded"},
		{"latest pod wins", v1.ReleaseTaskStatus{}, jobWith("", ""), []*corev1.Pod{pod("old", 0, corev1.PodFailed), pod("new", 5, corev1.PodRunning)}, v1.PhaseRunning, "new", ""},
		{"terminal is sticky", v1.ReleaseTaskStatus{Phase: v1.PhaseFailed, Message: "x", CompletionTime: &now}, jobWith(batchv1.JobComplete, ""), nil, v1.PhaseFailed, "", "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ComputeStatus(c.cur, c.job, c.pods, now)
			if got.Phase != c.wantPhase || got.ExecutorPodName != c.wantPod || got.Message != c.wantMsg {
				t.Fatalf("got %+v, want phase=%s pod=%q msg=%q", got, c.wantPhase, c.wantPod, c.wantMsg)
			}
			if got.Phase.Terminal() && got.CompletionTime == nil {
				t.Fatal("terminal status must carry completionTime")
			}
		})
	}
}

func TestFailureMessagePrefersContainerExit(t *testing.T) {
	p := pod("p1", 0, corev1.PodFailed)
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:  "git-clone",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128, Reason: "Error"}},
	}}
	got := ComputeStatus(v1.ReleaseTaskStatus{}, jobWith(batchv1.JobFailed, "BackoffLimitExceeded"), []*corev1.Pod{p}, now)
	if got.Message != "container git-clone exited with code 128: Error" {
		t.Fatalf("unexpected message %q", got.Message)
	}
}

func TestJobNameLength(t *testing.T) {
	if got := JobName("short"); got != "rt-short" {
		t.Fatalf("got %q", got)
	}
	long := JobName("a-very-long-release-task-name-that-would-overflow-the-pod-label-limit-0123456789")
	if len(long) > 52 {
		t.Fatalf("job name too long: %d", len(long))
	}
	if long == JobName("a-very-long-release-task-name-that-would-overflow-the-pod-label-limit-0123456780") {
		t.Fatal("distinct tasks must get distinct job names")
	}
}
