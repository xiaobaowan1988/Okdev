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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersbatch "k8s.io/client-go/listers/batch/v1"
	listerscore "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	v1 "github.com/xiaobaowan1988/okdev/internal/api/v1"
)

// Controller 监听 ReleaseTask,为每个任务创建一个 Job,并把阶段和执行 Pod 名写回 status。
type Controller struct {
	cfg      Config
	dyn      dynamic.Interface
	kube     kubernetes.Interface
	recorder record.EventRecorder
	queue    workqueue.TypedRateLimitingInterface[string]

	tasks    cache.SharedIndexInformer
	jobs     listersbatch.JobLister
	pods     listerscore.PodLister
	hasSyncs []cache.InformerSynced
	startFns []func(<-chan struct{})
	now      func() metav1.Time
}

// New 创建控制器。namespace 为空表示监听所有命名空间。
func New(cfg Config, dyn dynamic.Interface, kube kubernetes.Interface, rec record.EventRecorder, namespace string, resync time.Duration) *Controller {
	c := &Controller{
		cfg: cfg, dyn: dyn, kube: kube, recorder: rec,
		queue: workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		now:   metav1.Now,
	}

	taskFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, resync, namespace, nil)
	c.tasks = taskFactory.ForResource(v1.GVR).Informer()
	c.tasks.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { c.enqueueKey(o) },
		UpdateFunc: func(_, o any) { c.enqueueKey(o) },
	})

	kubeFactory := informers.NewSharedInformerFactoryWithOptions(kube, resync,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = LabelManagedBy + "=" + ManagedByValue }))
	jobInf := kubeFactory.Batch().V1().Jobs()
	podInf := kubeFactory.Core().V1().Pods()
	c.jobs, c.pods = jobInf.Lister(), podInf.Lister()
	child := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { c.enqueueOwner(o) },
		UpdateFunc: func(_, o any) { c.enqueueOwner(o) },
		DeleteFunc: func(o any) { c.enqueueOwner(o) },
	}
	jobInformer, podInformer := jobInf.Informer(), podInf.Informer()
	jobInformer.AddEventHandler(child)
	podInformer.AddEventHandler(child)

	c.hasSyncs = []cache.InformerSynced{c.tasks.HasSynced, jobInformer.HasSynced, podInformer.HasSynced}
	c.startFns = []func(<-chan struct{}){taskFactory.Start, kubeFactory.Start}
	return c
}

func (c *Controller) enqueueKey(o any) {
	if key, err := cache.MetaNamespaceKeyFunc(o); err == nil {
		c.queue.Add(key)
	}
}

// enqueueOwner 通过注解找到 Job/Pod 所属的任务。
func (c *Controller) enqueueOwner(o any) {
	if tomb, ok := o.(cache.DeletedFinalStateUnknown); ok {
		o = tomb.Obj
	}
	m, ok := o.(metav1.Object)
	if !ok {
		return
	}
	if name := m.GetAnnotations()[AnnotationTask]; name != "" {
		c.queue.Add(m.GetNamespace() + "/" + name)
	}
}

// Run 启动 informer 和 worker,阻塞到 ctx 结束。
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer c.queue.ShutDown()
	for _, s := range c.startFns {
		s(ctx.Done())
	}
	if !cache.WaitForCacheSync(ctx.Done(), c.hasSyncs...) {
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

// Reconcile 让一个任务的 Job 和 status 与期望一致。
func (c *Controller) Reconcile(ctx context.Context, key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil
	}
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
		return c.writeStatus(ctx, raw, task.Status, v1.ReleaseTaskStatus{
			Phase: v1.PhaseFailed, Message: msg, CompletionTime: ptr(c.now()),
		})
	}

	job, err := c.jobs.Jobs(ns).Get(JobName(name))
	switch {
	case apierrors.IsNotFound(err):
		job = nil
	case err != nil:
		return err
	}

	if job == nil && task.Status.Phase != "" && task.Status.JobName != "" {
		// 已经创建过 Job 但现在找不到(被人删了),不重建以免重复发布。
		c.recorder.Event(raw, corev1.EventTypeWarning, "JobLost", "executor job disappeared")
		st := task.Status
		st.Phase, st.Message, st.CompletionTime = v1.PhaseFailed, "executor job was deleted", ptr(c.now())
		return c.writeStatus(ctx, raw, task.Status, st)
	}

	if job == nil {
		created, err := c.kube.BatchV1().Jobs(ns).Create(ctx, BuildJob(task, c.cfg), metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return nil // informer 还没看到,等 Job 事件触发下一轮
		}
		if err != nil {
			return fmt.Errorf("create job: %w", err)
		}
		c.recorder.Eventf(raw, corev1.EventTypeNormal, "JobCreated", "created executor job %s", created.Name)
		job = created
	}

	pods, err := c.pods.Pods(ns).List(labels.SelectorFromSet(labels.Set{"job-name": job.Name}))
	if err != nil {
		return err
	}
	next := ComputeStatus(task.Status, job, pods, c.now())
	if next.Phase != task.Status.Phase {
		c.emitPhaseEvent(raw, next)
	}
	return c.writeStatus(ctx, raw, task.Status, next)
}

func (c *Controller) emitPhaseEvent(raw *unstructured.Unstructured, st v1.ReleaseTaskStatus) {
	switch st.Phase {
	case v1.PhaseRunning:
		c.recorder.Eventf(raw, corev1.EventTypeNormal, "Running", "executor pod %s is running", st.ExecutorPodName)
	case v1.PhaseSuccess:
		c.recorder.Event(raw, corev1.EventTypeNormal, "Succeeded", "release task succeeded")
	case v1.PhaseFailed:
		c.recorder.Eventf(raw, corev1.EventTypeWarning, "Failed", "release task failed: %s", st.Message)
	}
}

func (c *Controller) writeStatus(ctx context.Context, raw *unstructured.Unstructured, cur, next v1.ReleaseTaskStatus) error {
	if equality.Semantic.DeepEqual(cur, next) {
		return nil
	}
	m, err := v1.StatusToMap(next)
	if err != nil {
		return err
	}
	out := raw.DeepCopy()
	if err := unstructured.SetNestedMap(out.Object, runtime.DeepCopyJSON(m), "status"); err != nil {
		return err
	}
	_, err = c.dyn.Resource(v1.GVR).Namespace(raw.GetNamespace()).UpdateStatus(ctx, out, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func ptr[T any](v T) *T { return &v }
