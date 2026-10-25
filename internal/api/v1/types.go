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

// StageSubmitting 是控制器向 deployer 提交运行期间写入 status.stage 的标记:
// 提交后、记下 runID 前如果控制器崩溃,重启后看到这个标记就知道"提交结果不明",不会重复提交造成重复部署。
const StageSubmitting = "submitting"

// ReleaseTask 描述一次"把某服务的某个分支/Tag 构建并发布到某环境"。
// 构建和部署由 deployer(myk8s)执行,这里只是它的 K8s 原生入口:RBAC、kubectl 可查、Headlamp 统一界面。
type ReleaseTask struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ReleaseTaskSpec   `json:"spec"`
	Status            ReleaseTaskStatus `json:"status,omitempty"`
}

type ReleaseTaskSpec struct {
	// Service 是 deployer 里登记的服务名。
	Service string `json:"service"`
	// Builder 是 deployer 里关联该服务的构建器。
	Builder string `json:"builder"`
	// Environment 是 deployer 里的环境名(不是 K8s 命名空间)。
	Environment string `json:"environment"`
	// Ref 是要构建的分支或 Tag;留空用构建器的默认值。
	Ref string `json:"ref,omitempty"`
	// Pipeline 是要运行的流水线;留空用控制器的默认值。
	Pipeline string `json:"pipeline,omitempty"`
	// Replicas 仅在该环境还没有这个服务的部署时使用。
	Replicas *int `json:"replicas,omitempty"`
}

type ReleaseTaskStatus struct {
	Phase Phase `json:"phase,omitempty"`
	// Stage 是 deployer 运行的原始状态:pending / building / deploying / succeeded / failed / cancelled。
	Stage string `json:"stage,omitempty"`
	// RunID 是 deployer 里的运行编号,日志、取消、审计都以它为准。
	RunID int64 `json:"runID,omitempty"`
	// Image 是本次构建出的镜像。
	Image          string       `json:"image,omitempty"`
	Message        string       `json:"message,omitempty"`
	StartTime      *metav1.Time `json:"startTime,omitempty"`
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// Validate 检查 spec 是否完整。
func (s ReleaseTaskSpec) Validate() string {
	switch {
	case s.Service == "":
		return "spec.service is required"
	case s.Builder == "":
		return "spec.builder is required"
	case s.Environment == "":
		return "spec.environment is required"
	case strings.HasPrefix(s.Ref, "-"):
		return "spec.ref must not start with '-'"
	case s.Replicas != nil && (*s.Replicas < 0 || *s.Replicas > 50):
		return "spec.replicas must be 0..50"
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
