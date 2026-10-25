package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"

	"github.com/xiaobaowan1988/okdev/internal/controller"
)

func main() {
	var (
		kubeconfig  = flag.String("kubeconfig", "", "kubeconfig 路径,留空则使用集群内配置")
		namespace   = flag.String("namespace", "", "只监听该命名空间,留空监听全部")
		workers     = flag.Int("workers", 2, "并发调谐数")
		leaderElect = flag.Bool("leader-elect", false, "多副本时开启 Lease 选主")
		leaseNS     = flag.String("leader-election-namespace", "okdev-system", "选主 Lease 所在命名空间")
		cfg         controller.Config
	)
	flag.StringVar(&cfg.ExecutorImage, "executor-image", "", "默认执行器镜像(必填)")
	flag.StringVar(&cfg.KanikoImage, "kaniko-image", "gcr.io/kaniko-project/executor:v1.23.2-debug", "构建镜像用的 kaniko(需带 shell 的 debug 版)")
	flag.StringVar(&cfg.CAConfigMap, "ca-configmap", "", "含 build-ca.crt 的 ConfigMap(可选)")
	flag.StringVar(&cfg.GitOpsSecret, "gitops-secret", "", "含 token 的 Secret,用于推送 GitOps 仓库(可选)")
	flag.Func("executor-env", "传给执行器的环境变量 KEY=VALUE,可重复,如 REGISTRY_PUSH=...", func(v string) error {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return fmt.Errorf("want KEY=VALUE, got %q", v)
		}
		cfg.ExtraEnv = append(cfg.ExtraEnv, corev1.EnvVar{Name: k, Value: val})
		return nil
	})
	flag.DurationVar(&cfg.DefaultTimeout, "default-timeout", 30*time.Minute, "任务默认超时")
	flag.Parse()

	if cfg.ExecutorImage == "" {
		slog.Error("--executor-image is required")
		os.Exit(2)
	}
	rc, err := restConfig(*kubeconfig)
	if err != nil {
		slog.Error("load kube config", "err", err)
		os.Exit(1)
	}
	kube := kubernetes.NewForConfigOrDie(rc)
	dyn := dynamic.NewForConfigOrDie(rc)

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcore.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	defer broadcaster.Shutdown()
	rec := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "okdev-controller"})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	run := func(ctx context.Context) {
		c := controller.New(cfg, dyn, kube, rec, *namespace, 10*time.Minute)
		if err := c.Run(ctx, *workers); err != nil {
			slog.Error("controller stopped", "err", err)
			os.Exit(1)
		}
	}
	if !*leaderElect {
		run(ctx)
		return
	}

	id, _ := os.Hostname()
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: "okdev-controller", Namespace: *leaseNS},
		Client:     kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: id},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock: lock, ReleaseOnCancel: true,
		LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() { slog.Info("lost leadership"); os.Exit(0) },
		},
	})
}

func restConfig(path string) (*rest.Config, error) {
	if path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	return rest.InClusterConfig()
}
