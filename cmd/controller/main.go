package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
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
	"github.com/xiaobaowan1988/okdev/internal/deployer"
	"github.com/xiaobaowan1988/okdev/internal/gateway"
	"github.com/xiaobaowan1988/okdev/internal/gitrefs"
)

func main() {
	var (
		kubeconfig  = flag.String("kubeconfig", "", "kubeconfig 路径,留空则使用集群内配置")
		namespace   = flag.String("namespace", "", "只监听该命名空间,留空监听全部")
		workers     = flag.Int("workers", 2, "并发调谐数")
		leaderElect = flag.Bool("leader-elect", false, "多副本时开启 Lease 选主")
		leaseNS     = flag.String("leader-election-namespace", "okdev-system", "选主 Lease 所在命名空间")
		cfg         controller.Config

		httpAddr      string
		deployerURL   string
		deployerToken string
		gitTokenFile  string
	)
	flag.StringVar(&deployerURL, "deployer-url", "http://deployer.deployer.svc:8080", "deployer(platform/deployer)的地址")
	flag.StringVar(&deployerToken, "deployer-token-file", "", "deployer 的 API_TOKEN 文件(deployer 开启鉴权时需要)")
	flag.StringVar(&cfg.DefaultPipeline, "default-pipeline", "build-and-deploy", "spec.pipeline 留空时运行的流水线")
	flag.DurationVar(&cfg.PollInterval, "poll-interval", 3*time.Second, "轮询 deployer 运行状态的间隔")
	flag.StringVar(&cfg.Actor, "actor", "okdev-controller", "记在 deployer 历史里的操作人")
	flag.StringVar(&httpAddr, "http-addr", ":8080", "只读 HTTP 接口地址(给 Headlamp 插件:构建器、环境、分支/Tag、运行日志),留空关闭")
	flag.StringVar(&gitTokenFile, "git-token-file", "", "访问私有 http(s) 仓库列分支用的 token 文件(可选)")
	flag.Parse()

	rc, err := restConfig(*kubeconfig)
	if err != nil {
		slog.Error("load kube config", "err", err)
		os.Exit(1)
	}
	kube := kubernetes.NewForConfigOrDie(rc)
	dyn := dynamic.NewForConfigOrDie(rc)

	token := readTrim(deployerToken)
	dep := deployer.New(deployerURL, token)

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcore.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	defer broadcaster.Shutdown()
	rec := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "okdev-controller"})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if httpAddr != "" {
		gw := &gateway.Gateway{Dep: dep, Refs: gitrefs.NewLister(readTrim(gitTokenFile))}
		srv := &http.Server{Addr: httpAddr, Handler: gw.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("http server", "err", err)
				os.Exit(1)
			}
		}()
		go func() { <-ctx.Done(); _ = srv.Close() }()
	}

	run := func(ctx context.Context) {
		c := controller.New(cfg, dyn, dep, rec, *namespace, 10*time.Minute)
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

// readTrim 读文件内容并去掉首尾空白;路径为空或读取失败时退出(配置了就必须可读)。
func readTrim(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		slog.Error("read file", "path", path, "err", err)
		os.Exit(1)
	}
	return strings.TrimSpace(string(b))
}
