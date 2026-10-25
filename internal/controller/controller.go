package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	v1 "github.com/xiaobaowan1988/okdev/internal/api/v1"
	"github.com/xiaobaowan1988/okdev/internal/deployer"
)

// Deployer 是控制器用到的那部分 deployer 客户端,便于测试替换。
type Deployer interface {
	Healthy(ctx context.Context) error
	GetBuilder(ctx context.Context, name string) (*deployer.Builder, error)
	CreateRun(ctx context.Context, pipeline, builder, env, ref string, replicas *int, actor string) (*deployer.Run, error)
	GetRun(ctx context.Context, id int64) (*deployer.Run, error)
}

type Config struct {
	DefaultPipeline string        // spec.pipeline 留空时用的流水线
	PollInterval    time.Duration // 轮询 deployer 运行状态的间隔
	Actor           string        // 记在 deployer 历史里的操作人
}

// Controller 监听 ReleaseTask,向 deployer 提交一次流水线运行,并把运行状态同步回 status。
// 构建、部署、并发控制、历史都在 deployer;这里不重复实现。
type Controller struct {
	cfg      Config
	dyn      dynamic.Interface
	dep      Deployer
	recorder record.EventRecorder
	queue    workqueue.TypedRateLimitingInterface[string]
	tasks    cache.SharedIndexInformer
	start    func(<-chan struct{})
	synced   cache.InformerSynced
	now      func() metav1.Time
}

// New 创建控制器。namespace 为空表示监听所有命名空间。
func New(cfg Config, dyn dynamic.Interface, dep Deployer, rec record.EventRecorder, namespace string, resync time.Duration) *Controller {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Second
	}
	if cfg.Actor == "" {
		cfg.Actor = "okdev-controller"
	}
	c := &Controller{
		cfg: cfg, dyn: dyn, dep: dep, recorder: rec,
		queue: workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		now:   metav1.Now,
	}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, resync, namespace, nil)
	c.tasks = factory.ForResource(v1.GVR).Informer()
	c.tasks.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { c.enqueue(o) },
		UpdateFunc: func(_, o any) { c.enqueue(o) },
	})
	c.start, c.synced = factory.Start, c.tasks.HasSynced
	return c
}

func (c *Controller) enqueue(o any) {
	if key, err := cache.MetaNamespaceKeyFunc(o); err == nil {
		c.queue.Add(key)
	}
}

// Run 启动 informer 和 worker,阻塞到 ctx 结束。
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer c.queue.ShutDown()
	c.start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), c.synced) {
		return fmt.Errorf("cache sync failed")
	}
	slog.Info("controller started", "workers", workers)
	for i := 0; i < workers; i++ {
		go func() {
			for c.processNext(ctx) {
			}
		}()
	}
	<-ctx.Done()
	return nil
}

func (c *Controller) processNext(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)
	if err := c.Reconcile(ctx, key); err != nil {
		slog.Error("reconcile failed", "task", key, "err", err)
		c.queue.AddRateLimited(key)
		return true
	}
	c.queue.Forget(key)
	return true
}

// Reconcile 推进一个任务:没有提交过就提交,提交过就同步 deployer 的运行状态。
func (c *Controller) Reconcile(ctx context.Context, key string) error {
	obj, exists, err := c.tasks.GetIndexer().GetByKey(key)
	if err != nil || !exists {
		return err
	}
	raw := obj.(*unstructured.Unstructured)
	if raw.GetDeletionTimestamp() != nil {
		return nil
	}
	task, err := v1.FromUnstructured(raw)
	if err != nil {
		return fmt.Errorf("decode %s: %w", key, err)
	}
	if task.Status.Phase.Terminal() {
		return nil
	}
	if msg := task.Spec.Validate(); msg != "" {
		c.recorder.Event(raw, corev1.EventTypeWarning, "InvalidSpec", msg)
		return c.fail(ctx, raw, msg)
	}
	if task.Status.RunID == 0 {
		return c.submit(ctx, key, raw, task)
	}
	return c.poll(ctx, key, raw, task)
}

func (c *Controller) submit(ctx context.Context, key string, raw *unstructured.Unstructured, task *v1.ReleaseTask) error {
	if task.Status.Stage == v1.StageSubmitting {
		// 缓存可能落后:我们自己刚写的"提交中"标记已经被后面的 runID 覆盖,但缓存里还是旧的。先读最新的再判断。
		live, err := c.dyn.Resource(v1.GVR).Namespace(raw.GetNamespace()).Get(ctx, raw.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if lt, err := v1.FromUnstructured(live); err != nil {
			return err
		} else if lt.Status.RunID != 0 || lt.Status.Phase.Terminal() {
			c.queue.AddAfter(key, c.cfg.PollInterval)
			return nil
		}
		// 已经写过"提交中"标记却没有 runID:控制器在提交过程中重启过,deployer 那边有没有创建运行不确定。
		// 重新提交可能造成重复部署,所以不重试,让人确认。
		msg := fmt.Sprintf("submission state unknown (controller restarted while submitting); check recent runs of builder %q in env %q in deployer, then create a new task if none exists", task.Spec.Builder, task.Spec.Environment)
		c.recorder.Event(raw, corev1.EventTypeWarning, "SubmissionUnknown", msg)
		return c.fail(ctx, raw, msg)
	}
	// deployer 不可达时先别写标记,等它恢复(返回错误走退避重试)。
	if err := c.dep.Healthy(ctx); err != nil {
		return fmt.Errorf("deployer not reachable: %w", err)
	}
	b, err := c.dep.GetBuilder(ctx, task.Spec.Builder)
	switch {
	case deployer.IsNotFound(err):
		return c.fail(ctx, raw, fmt.Sprintf("builder %q does not exist in deployer", task.Spec.Builder))
	case err != nil && deployer.ClientError(err):
		return c.fail(ctx, raw, "deployer rejected the builder lookup: "+err.Error())
	case err != nil:
		return err
	case b.Service != task.Spec.Service:
		return c.fail(ctx, raw, fmt.Sprintf("builder %q belongs to service %q, not %q", b.Name, b.Service, task.Spec.Service))
	}

	// 先写"提交中"标记,再提交。写之前读一次最新的任务,避免缓存落后时重复提交。
	marked, err := c.updateStatus(ctx, raw.GetNamespace(), raw.GetName(), func(cur *v1.ReleaseTaskStatus) bool {
		if cur.RunID != 0 || cur.Stage != "" || cur.Phase.Terminal() {
			return false // 别处已经处理过
		}
		cur.Phase, cur.Stage = v1.PhasePending, v1.StageSubmitting
		return true
	})
	if err != nil || !marked {
		return err
	}

	pipeline := task.Spec.Pipeline
	if pipeline == "" {
		pipeline = c.cfg.DefaultPipeline
	}
	run, err := c.dep.CreateRun(ctx, pipeline, task.Spec.Builder, task.Spec.Environment, task.Spec.Ref, task.Spec.Replicas, c.cfg.Actor)
	if err != nil {
		var msg string
		if deployer.ClientError(err) {
			msg = "deployer rejected the run: " + err.Error()
		} else {
			// 网络错误等:请求可能已经到达。同样不自动重试,避免重复部署。
			msg = fmt.Sprintf("submission failed with unknown result (%v); check recent runs of builder %q in env %q in deployer before retrying", err, task.Spec.Builder, task.Spec.Environment)
		}
		c.recorder.Event(raw, corev1.EventTypeWarning, "SubmitFailed", msg)
		return c.fail(ctx, raw, msg)
	}
	c.recorder.Eventf(raw, corev1.EventTypeNormal, "Submitted", "submitted deployer run %d (pipeline %s, builder %s, env %s)", run.ID, pipeline, task.Spec.Builder, task.Spec.Environment)
	if _, err := c.updateStatus(ctx, raw.GetNamespace(), raw.GetName(), func(cur *v1.ReleaseTaskStatus) bool {
		*cur = StatusFromRun(*cur, run, c.now())
		return true
	}); err != nil {
		return err
	}
	c.queue.AddAfter(key, c.cfg.PollInterval)
	return nil
}

func (c *Controller) poll(ctx context.Context, key string, raw *unstructured.Unstructured, task *v1.ReleaseTask) error {
	run, err := c.dep.GetRun(ctx, task.Status.RunID)
	switch {
	case deployer.IsNotFound(err):
		return c.fail(ctx, raw, fmt.Sprintf("run %d no longer exists in deployer", task.Status.RunID))
	case err != nil:
		return err
	}
	var changedPhase bool
	var after v1.ReleaseTaskStatus
	if _, err := c.updateStatus(ctx, raw.GetNamespace(), raw.GetName(), func(cur *v1.ReleaseTaskStatus) bool {
		next := StatusFromRun(*cur, run, c.now())
		if equality.Semantic.DeepEqual(*cur, next) {
			return false
		}
		changedPhase = next.Phase != cur.Phase
		*cur, after = next, next
		return true
	}); err != nil {
		return err
	}
	if changedPhase {
		c.emitPhaseEvent(raw, after)
	}
	if !PhaseOf(run.Status).Terminal() {
		c.queue.AddAfter(key, c.cfg.PollInterval)
	}
	return nil
}

func (c *Controller) emitPhaseEvent(raw *unstructured.Unstructured, st v1.ReleaseTaskStatus) {
	switch st.Phase {
	case v1.PhaseRunning:
		c.recorder.Eventf(raw, corev1.EventTypeNormal, "Running", "run %d is %s", st.RunID, st.Stage)
	case v1.PhaseSuccess:
		c.recorder.Eventf(raw, corev1.EventTypeNormal, "Succeeded", "run %d succeeded, image %s", st.RunID, st.Image)
	case v1.PhaseFailed:
		c.recorder.Eventf(raw, corev1.EventTypeWarning, "Failed", "run %d %s: %s", st.RunID, st.Stage, st.Message)
	}
}

// fail 把任务置为失败(终态)。
func (c *Controller) fail(ctx context.Context, raw *unstructured.Unstructured, msg string) error {
	_, err := c.updateStatus(ctx, raw.GetNamespace(), raw.GetName(), func(cur *v1.ReleaseTaskStatus) bool {
		if cur.Phase.Terminal() {
			return false
		}
		cur.Phase, cur.Message, cur.CompletionTime = v1.PhaseFailed, msg, ptr(c.now())
		return true
	})
	return err
}

// updateStatus 读最新的任务、应用 mutate、写回 status;冲突时重试。mutate 返回 false 表示无需写入。
// 返回值表示是否真的写入了。
func (c *Controller) updateStatus(ctx context.Context, ns, name string, mutate func(*v1.ReleaseTaskStatus) bool) (bool, error) {
	res := c.dyn.Resource(v1.GVR).Namespace(ns)
	for attempt := 0; attempt < 5; attempt++ {
		live, err := res.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		task, err := v1.FromUnstructured(live)
		if err != nil {
			return false, err
		}
		st := task.Status
		if !mutate(&st) {
			return false, nil
		}
		m, err := v1.StatusToMap(st)
		if err != nil {
			return false, err
		}
		out := live.DeepCopy()
		if err := unstructured.SetNestedMap(out.Object, runtime.DeepCopyJSON(m), "status"); err != nil {
			return false, err
		}
		_, err = res.UpdateStatus(ctx, out, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	return false, fmt.Errorf("update status of %s/%s: too many conflicts", ns, name)
}

func ptr[T any](v T) *T { return &v }
