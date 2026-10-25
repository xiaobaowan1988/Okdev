package controller

import (
	"crypto/sha1"
	"encoding/hex"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/xiaobaowan1988/okdev/internal/api/v1"
)

const (
	LabelManagedBy = "ci.okdev.io/managed-by"
	ManagedByValue = "okdev"
	AnnotationTask = "ci.okdev.io/task-name"
	workspaceDir   = "/workspace"
	sourceDir      = "/workspace/src"
)

// Config 是控制器级别的默认值。
type Config struct {
	ExecutorImage  string        // 默认执行器镜像,由它完成编译、打包、更新 GitOps 仓库
	GitImage       string        // 拉源码用,需要有 git 和 sh
	ServiceAccount string        // 执行 Pod 使用的 ServiceAccount,可空
	DefaultTimeout time.Duration // 任务超时的默认值
}

// JobName 返回任务对应的 Job 名。Pod 名由 Job 名派生,所以名字过长时截断并加哈希。
func JobName(taskName string) string {
	n := "rt-" + taskName
	if len(n) <= 52 {
		return n
	}
	sum := sha1.Sum([]byte(taskName))
	return n[:43] + "-" + hex.EncodeToString(sum[:])[:8]
}

// BuildJob 生成执行任务的 Job:init 容器克隆代码,主容器跑执行器。
// 用户输入一律通过环境变量传给容器,不拼进 shell 命令。
func BuildJob(t *v1.ReleaseTask, cfg Config) *batchv1.Job {
	image, command := cfg.ExecutorImage, []string(nil)
	if e := t.Spec.Executor; e != nil {
		if e.Image != "" {
			image = e.Image
		}
		command = e.Command
	}
	timeout := cfg.DefaultTimeout
	if t.Spec.TimeoutSeconds > 0 {
		timeout = time.Duration(t.Spec.TimeoutSeconds) * time.Second
	}
	deadline := int64(timeout.Seconds())
	backoff := int32(0)
	isController := true

	env := []corev1.EnvVar{
		{Name: "TASK_NAME", Value: t.Name},
		{Name: "SERVICE_NAME", Value: t.Spec.Service.Name},
		{Name: "SERVICE_KIND", Value: t.Spec.Service.Kind},
		{Name: "ENVIRONMENT", Value: t.Spec.Environment},
		{Name: "GIT_REPO", Value: t.Spec.Git.Repo},
		{Name: "GIT_REF", Value: t.Spec.Git.Ref},
	}
	if g := t.Spec.GitOps; g != nil {
		env = append(env,
			corev1.EnvVar{Name: "GITOPS_REPO", Value: g.Repo},
			corev1.EnvVar{Name: "GITOPS_BRANCH", Value: g.Branch},
			corev1.EnvVar{Name: "GITOPS_PATH", Value: g.Path},
		)
	}
	mounts := []corev1.VolumeMount{{Name: "workspace", MountPath: workspaceDir}}
	labels := map[string]string{LabelManagedBy: ManagedByValue}
	annotations := map[string]string{AnnotationTask: t.Name}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        JobName(t.Name),
			Namespace:   t.Namespace,
			Labels:      labels,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1.Group + "/" + v1.Version,
				Kind:       v1.Kind,
				Name:       t.Name,
				UID:        t.UID,
				Controller: &isController,
			}},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cfg.ServiceAccount,
					Volumes: []corev1.Volume{{
						Name:         "workspace",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					}},
					InitContainers: []corev1.Container{{
						Name:         "git-clone",
						Image:        cfg.GitImage,
						Command:      []string{"sh", "-c", `set -e; git clone -- "$GIT_REPO" ` + sourceDir + `; cd ` + sourceDir + `; git checkout "$GIT_REF" --`},
						Env:          env,
						VolumeMounts: mounts,
					}},
					Containers: []corev1.Container{{
						Name:         "executor",
						Image:        image,
						Command:      command,
						WorkingDir:   sourceDir,
						Env:          env,
						VolumeMounts: mounts,
					}},
				},
			},
		},
	}
}
