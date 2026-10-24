package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var (
	appGVR    = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	deployGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

const (
	// 同一个 Application 两次硬刷新之间的最短间隔;连续不一致达到 mismatchAlert 次就打 ERROR 日志
	refreshBackoff = 90 * time.Second
	unknownGrace   = 60 * time.Second
	mismatchAlert  = 3

	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "deployer"
	finalizer      = "resources-finalizer.argocd.argoproj.io"
)

type Reconciler struct {
	store  *Store
	db     *sql.DB
	kube   dynamic.Interface
	cfg    config
	log    *slog.Logger
	kickCh chan struct{}

	// 仅对账 goroutine 访问,不需要加锁
	lastRefresh  map[string]time.Time // Application 名 → 上次硬刷新时间
	mismatchRuns map[string]int       // Application 名 → 连续"副本数不一致"的轮数
	unknownSince map[string]time.Time // Application 名 → 开始 Unknown 的时间
}

func NewReconciler(st *Store, db *sql.DB, kube dynamic.Interface, cfg config, log *slog.Logger) *Reconciler {
	return &Reconciler{store: st, db: db, kube: kube, cfg: cfg, log: log, kickCh: make(chan struct{}, 1),
		lastRefresh: map[string]time.Time{}, mismatchRuns: map[string]int{}, unknownSince: map[string]time.Time{}}
}

// Kick 让对账循环尽快跑一轮(API 写入后调用),不阻塞。
func (r *Reconciler) Kick() {
	select {
	case r.kickCh <- struct{}{}:
	default:
	}
}

func appName(x EnvService) string       { return x.Service + "-" + x.Env }
func destNamespace(x EnvService) string { return x.Env + "-env-" + x.Service }

// desiredApp 由数据库里的期望状态生成 Application。副本数/镜像通过 kustomize 参数覆盖传给 Argo CD。
func (r *Reconciler) desiredApp(x EnvService) *unstructured.Unstructured {
	kustomize := map[string]any{
		"replicas": []any{map[string]any{"name": x.Workload, "count": int64(x.Replicas)}},
	}
	if x.Image != "" && x.ImageName != "" {
		kustomize["images"] = []any{x.ImageName + "=" + x.Image}
	}
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      appName(x),
			"namespace": r.cfg.appNamespace,
			"labels": map[string]any{
				labelManagedBy:     managedByValue,
				"deployer/env":     x.Env,
				"deployer/service": x.Service,
			},
			"finalizers": []any{finalizer},
		},
		"spec": map[string]any{
			"project": r.cfg.project,
			"source": map[string]any{
				"repoURL":        x.Repo,
				"path":           x.Path,
				"targetRevision": x.Revision,
				"kustomize":      kustomize,
			},
			"destination": map[string]any{"server": r.cfg.destServer, "namespace": destNamespace(x)},
			"syncPolicy": map[string]any{
				"automated":   map[string]any{"prune": true, "selfHeal": true},
				"syncOptions": []any{"CreateNamespace=true"},
			},
		},
	}}
	return app
}

// subsetEqual: want 里出现的字段必须在 have 里相等;have 里多出来的(服务端默认值、status 等)忽略。
// 用来检测"有人手动改了 Application.spec"这种漂移。
func subsetEqual(want, have any) bool {
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !subsetEqual(v, h[k]) {
				return false
			}
		}
		return true
	case []any:
		h, ok := have.([]any)
		if !ok || len(h) != len(w) {
			return false
		}
		for i := range w {
			if !subsetEqual(w[i], h[i]) {
				return false
			}
		}
		return true
	default:
		return fmt.Sprint(want) == fmt.Sprint(have)
	}
}

func (r *Reconciler) apps() dynamic.ResourceInterface {
	return r.kube.Resource(appGVR).Namespace(r.cfg.appNamespace)
}

func (r *Reconciler) Run(ctx context.Context) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		r.log.Error("reconciler conn", "err", err)
		return
	}
	defer conn.Close()
	t := time.NewTicker(r.cfg.reconcileEvery)
	defer t.Stop()
	for {
		// GET_LOCK 是会话级锁:多个副本里只有一个能拿到,拿不到就这一轮不干活;连接断开锁自动释放。
		var got int
		if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK('deployer-reconciler', 0)`).Scan(&got); err != nil {
			r.log.Error("leader lock", "err", err)
		} else if got == 1 {
			r.reconcileAll(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.kickCh:
		}
	}
}

func (r *Reconciler) reconcileAll(ctx context.Context) {
	list, err := r.store.ListEnvServices(ctx, "")
	if err != nil {
		r.log.Error("list", "err", err)
		return
	}
	for _, x := range list {
		if err := r.reconcileOne(ctx, x); err != nil {
			r.log.Error("reconcile", "env", x.Env, "service", x.Service, "err", err)
			_ = r.store.SetObservedOnly(ctx, x, x.ObservedSync, x.ObservedHealth, "reconcile error: "+err.Error())
		}
	}
}

func (r *Reconciler) reconcileOne(ctx context.Context, x EnvService) error {
	live, err := r.apps().Get(ctx, appName(x), metav1.GetOptions{})
	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return err
	}
	if !missing && live.GetLabels()[labelManagedBy] != managedByValue {
		return fmt.Errorf("conflict: Application %s exists but is not managed by deployer", appName(x))
	}
	deleting := !missing && live.GetDeletionTimestamp() != nil

	if x.DesiredState == "deleted" {
		switch {
		case missing:
			r.log.Info("deployment removed", "env", x.Env, "service", x.Service)
			delete(r.lastRefresh, appName(x))
			delete(r.mismatchRuns, appName(x))
			delete(r.unknownSince, appName(x))
			return r.store.DeleteRow(ctx, x)
		case deleting:
			return r.store.SetObservedOnly(ctx, x, "", "Deleting", "waiting for Argo CD to delete resources")
		default:
			r.log.Info("deleting application", "app", appName(x))
			return r.apps().Delete(ctx, appName(x), metav1.DeleteOptions{})
		}
	}

	want := r.desiredApp(x)
	switch {
	case deleting:
		return r.store.SetObservedOnly(ctx, x, "", "Deleting", "previous Application is still being deleted")
	case missing:
		r.log.Info("creating application", "app", appName(x))
		if live, err = r.apps().Create(ctx, want, metav1.CreateOptions{}); err != nil {
			return err
		}
	case !subsetEqual(want.Object["spec"], live.Object["spec"]):
		r.log.Info("drift detected, restoring application", "app", appName(x))
		live.Object["spec"] = want.Object["spec"]
		if live, err = r.apps().Update(ctx, live, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}

	sync, _, _ := unstructured.NestedString(live.Object, "status", "sync", "status")
	health, _, _ := unstructured.NestedString(live.Object, "status", "health", "status")
	msg := ""
	if conds, ok, _ := unstructured.NestedSlice(live.Object, "status", "conditions"); ok && len(conds) > 0 {
		if m, ok := conds[0].(map[string]any); ok {
			msg, _ = m["message"].(string)
		}
	}
	health, msg = r.verify(ctx, x, sync, health, msg)
	return r.store.SetObserved(ctx, x, sync, health, msg)
}

// replicaVerdict 比较期望副本数和实际工作负载。返回空串表示一致。纯函数,便于单测。
func replicaVerdict(want int, actual int64, found bool, workload, ns string) string {
	if !found {
		return fmt.Sprintf("workload %s not found in namespace %s although Application is Synced", workload, ns)
	}
	if int64(want) != actual {
		return fmt.Sprintf("replica mismatch: desired %d, actual %d", want, actual)
	}
	return ""
}

// verify 在 Argo CD 说"已同步"的前提下,直接读工作负载的实际副本数核对,补上 Application 状态看不到的盲区
// (实测过:Argo CD 渲染出错误的副本数却报 Synced/Healthy)。不一致就标 ReplicaMismatch 并触发硬刷新。
// 另外 Application 长时间 Unknown(如渲染报错)也触发硬刷新,清掉缓存的错误结果。
func (r *Reconciler) verify(ctx context.Context, x EnvService, sync, health, msg string) (string, string) {
	name := appName(x)
	if sync == "" || sync == "Unknown" {
		since, ok := r.unknownSince[name]
		if !ok {
			r.unknownSince[name] = time.Now()
		} else if time.Since(since) >= unknownGrace {
			r.hardRefresh(ctx, name, "sync status stuck at Unknown")
		}
		return health, msg
	}
	delete(r.unknownSince, name)

	// 只在稳定时核对:已 Synced,且这一版期望之前的轮次已经应用过(避免刚改完就误报)
	if sync != "Synced" || x.AppliedVersion != x.Version {
		return health, msg
	}
	ns := destNamespace(x)
	var actual int64
	found := false
	d, err := r.kube.Resource(deployGVR).Namespace(ns).Get(ctx, x.Workload, metav1.GetOptions{})
	switch {
	case err == nil:
		found = true
		if v, ok, _ := unstructured.NestedInt64(d.Object, "spec", "replicas"); ok {
			actual = v
		} else {
			actual = 1 // Deployment 未写 replicas 时的默认值
		}
	case apierrors.IsNotFound(err):
	default:
		r.log.Warn("verify: get deployment", "ns", ns, "name", x.Workload, "err", err)
		return health, msg
	}

	if v := replicaVerdict(x.Replicas, actual, found, x.Workload, ns); v != "" {
		r.mismatchRuns[name]++
		if r.mismatchRuns[name] >= mismatchAlert {
			r.log.Error("replica mismatch persists", "app", name, "detail", v, "runs", r.mismatchRuns[name])
		}
		r.hardRefresh(ctx, name, v)
		return "ReplicaMismatch", v
	}
	delete(r.mismatchRuns, name)
	return health, msg
}

func (r *Reconciler) hardRefresh(ctx context.Context, name, reason string) {
	if t, ok := r.lastRefresh[name]; ok && time.Since(t) < refreshBackoff {
		return
	}
	r.lastRefresh[name] = time.Now()
	r.log.Warn("hard refresh", "app", name, "reason", reason)
	patch := []byte(`{"metadata":{"annotations":{"argocd.argoproj.io/refresh":"hard"}}}`)
	if _, err := r.apps().Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		r.log.Error("hard refresh failed", "app", name, "err", err)
	}
}
