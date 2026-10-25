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
	ExecutorImage  string          // 执行器镜像(executor/ 目录),负责 checkout / prepare / release 三步
	KanikoImage    string          // 构建镜像的 kaniko,必须是带 shell 的 debug 版
	ExtraEnv       []corev1.EnvVar // 传给执行器的额外环境变量,如 REGISTRY_PUSH、REGISTRY_PULL
	CAConfigMap    string          // 可选:含 build-ca.crt 的 ConfigMap,出口 HTTPS 被中间人时用
	GitOpsSecret   string          // 可选:含 token 的 Secret,用于推送 GitOps 仓库
	DefaultTimeout time.Duration   // 任务超时的默认值
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
	env = append(env, cfg.ExtraEnv...)
	optional, noToken := true, false
	ws := corev1.VolumeMount{Name: "workspace", MountPath: workspaceDir}
	mounts := []corev1.VolumeMount{ws}
	volumes := []corev1.Volume{{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	kanikoMounts := []corev1.VolumeMount{ws}
	if cfg.CAConfigMap != "" {
		volumes = append(volumes, corev1.Volume{Name: "ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: cfg.CAConfigMap}, Optional: &optional}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "ca", MountPath: "/etc/build-ca", ReadOnly: true})
		kanikoMounts = append(kanikoMounts, corev1.VolumeMount{Name: "ca", MountPath: "/kaniko/ssl/certs/build-ca.crt", SubPath: "build-ca.crt", ReadOnly: true})
	}
	if cfg.GitOpsSecret != "" {
		volumes = append(volumes, corev1.Volume{Name: "gitops", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: cfg.GitOpsSecret, Optional: &optional}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "gitops", MountPath: "/etc/gitops", ReadOnly: true})
	}
	step := func(name string) corev1.Container {
		// 失败时把日志末尾作为终止消息,控制器据此在 status.message 里给出具体原因
		return corev1.Container{Name: name, Image: cfg.ExecutorImage, Args: []string{name}, Env: env, VolumeMounts: mounts,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError}
	}
	release := step("release")
	release.Image = image
	if command != nil {
		release.Command, release.Args = command, nil
	}
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
					RestartPolicy: corev1.RestartPolicyNever,
					// 构建的是任意用户代码,不给它集群访问令牌
					AutomountServiceAccountToken: &noToken,
					Volumes:                      volumes,
					InitContainers: []corev1.Container{
						step("checkout"),
						step("prepare"),
						// kaniko 会改写自己容器的根文件系统,所以单独一步;参数由 prepare 写入
						{
							Name: "build", Image: cfg.KanikoImage, VolumeMounts: kanikoMounts, TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
							Command: []string{"/busybox/sh", "-c", "exec /kaniko/executor $(cat " + workspaceDir + "/kaniko-args)"},
						},
					},
					Containers: []corev1.Container{release},
				},
			},
		},
	}
}
