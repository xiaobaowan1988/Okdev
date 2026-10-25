// Package v1 定义 ReleaseTask 自定义资源。
//
// 控制器用 dynamic client 读写,这里的结构体只负责与 unstructured 互转,
// 不依赖代码生成。
package v1

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Group   = "ci.okdev.io"
	Version = "v1"
	Kind    = "ReleaseTask"
)

var GVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: "releasetasks"}

// Phase 是任务阶段:Pending -> Running -> Success / Failed。
type Phase string

const (
	PhasePending Phase = "Pending"
	PhaseRunning Phase = "Running"
	PhaseSuccess Phase = "Success"
	PhaseFailed  Phase = "Failed"
)

func (p Phase) Terminal() bool { return p == PhaseSuccess || p == PhaseFailed }

// ReleaseTask 描述一次"把某服务的某个 Git 版本发布到某环境"。
type ReleaseTask struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ReleaseTaskSpec   `json:"spec"`
	Status            ReleaseTaskStatus `json:"status,omitempty"`
}

type ReleaseTaskSpec struct {
	// Service 是被发布的工作负载。
	Service ServiceRef `json:"service"`
	// Environment 是目标环境,即目标 Namespace。
	Environment string `json:"environment"`
	// Git 是要构建的源码版本。
	Git GitSpec `json:"git"`
	// GitOps 是部署配置所在仓库;留空则执行器只构建不改配置。
	GitOps *GitOpsSpec `json:"gitops,omitempty"`
	// Executor 覆盖控制器的默认执行器镜像。
	Executor *ExecutorSpec `json:"executor,omitempty"`
	// TimeoutSeconds 是整个任务的超时,0 表示用控制器默认值。
	TimeoutSeconds int64 `json:"timeoutSeconds,omitempty"`
}

type ServiceRef struct {
	Name string `json:"name"`
	// Kind 是 Deployment / StatefulSet / CronJob / Job 之一。
	Kind string `json:"kind"`
}

type GitSpec struct {
	Repo string `json:"repo"`
	// Ref 可以是分支、Tag 或 commit SHA。
	Ref string `json:"ref"`
}

type GitOpsSpec struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
	// Path 是仓库内该服务在该环境的配置目录。
	Path string `json:"path,omitempty"`
}

type ExecutorSpec struct {
	Image   string   `json:"image,omitempty"`
	Command []string `json:"command,omitempty"`
}

type ReleaseTaskStatus struct {
	Phase Phase `json:"phase,omitempty"`
	// JobName 是控制器为该任务创建的 Job。
	JobName string `json:"jobName,omitempty"`
	// ExecutorPodName 是正在(或曾经)执行任务的 Pod,前端据此读取日志。
	ExecutorPodName string       `json:"executorPodName,omitempty"`
	Message         string       `json:"message,omitempty"`
	StartTime       *metav1.Time `json:"startTime,omitempty"`
	CompletionTime  *metav1.Time `json:"completionTime,omitempty"`
}

var validKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "CronJob": true, "Job": true}

// Validate 检查 spec 是否完整。
func (s ReleaseTaskSpec) Validate() string {
	switch {
	case s.Service.Name == "":
		return "spec.service.name is required"
	case !validKinds[s.Service.Kind]:
		return "spec.service.kind must be one of Deployment, StatefulSet, CronJob, Job"
	case s.Environment == "":
		return "spec.environment is required"
	case s.Git.Repo == "":
		return "spec.git.repo is required"
	case s.Git.Ref == "":
		return "spec.git.ref is required"
	case strings.HasPrefix(s.Git.Ref, "-") || strings.HasPrefix(s.Git.Repo, "-"):
		return "spec.git.repo and spec.git.ref must not start with '-'"
	}
	return ""
}

// FromUnstructured 把 unstructured 转成 ReleaseTask。
func FromUnstructured(u *unstructured.Unstructured) (*ReleaseTask, error) {
	t := &ReleaseTask{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, t); err != nil {
		return nil, err
	}
	return t, nil
}

// StatusToMap 把 status 转成可写回 unstructured 的 map。
func StatusToMap(s ReleaseTaskStatus) (map[string]any, error) {
	return runtime.DefaultUnstructuredConverter.ToUnstructured(&s)
}
