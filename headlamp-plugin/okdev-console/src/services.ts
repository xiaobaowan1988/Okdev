import { K8s } from '@kinvolk/headlamp-plugin/lib';
import { KubeObject } from '@kinvolk/headlamp-plugin/lib/k8s/cluster';

export type ServiceKind = 'Deployment' | 'StatefulSet' | 'CronJob' | 'Job';

export interface ServiceItem {
  key: string;
  name: string;
  kind: ServiceKind;
  namespace: string;
  obj: KubeObject;
}

// 同时订阅四类工作负载(四个 hook 并发拉取),合并成统一的服务列表。
// 由 CronJob 或其他控制器创建的 Job(如 ReleaseTask 的执行 Job)不算"服务",
// 否则每次发布都会在列表里多出一行。
export function useServices(namespace: string): {
  items: ServiceItem[];
  loading: boolean;
  errors: string[];
} {
  const opts = { namespace };
  const [deployments, e1] = K8s.ResourceClasses.Deployment.useList(opts);
  const [statefulSets, e2] = K8s.ResourceClasses.StatefulSet.useList(opts);
  const [cronJobs, e3] = K8s.ResourceClasses.CronJob.useList(opts);
  const [jobs, e4] = K8s.ResourceClasses.Job.useList(opts);

  const items: ServiceItem[] = [];
  const add = (list: KubeObject[] | null, kind: ServiceKind, skip?: (o: KubeObject) => boolean) =>
    (list || []).forEach(o => {
      if (skip && skip(o)) return;
      items.push({
        key: `${kind}/${o.metadata.namespace}/${o.metadata.name}`,
        name: o.metadata.name,
        kind,
        namespace: o.metadata.namespace!,
        obj: o,
      });
    });
  add(deployments, 'Deployment');
  add(statefulSets, 'StatefulSet');
  add(cronJobs, 'CronJob');
  add(jobs, 'Job', o => (o.metadata.ownerReferences || []).length > 0);
  items.sort((a, b) => a.name.localeCompare(b.name) || a.kind.localeCompare(b.kind));

  const errors = [e1, e2, e3, e4].filter(Boolean).map(e => `${(e as any).message || e}`);
  const loading =
    [deployments, statefulSets, cronJobs, jobs].some(l => l === null) && errors.length === 0;
  return { items, loading, errors };
}
