import { ApiProxy } from '@kinvolk/headlamp-plugin/lib';
import { getCluster } from '@kinvolk/headlamp-plugin/lib/Utils';
import React from 'react';

// 控制器的只读接口(环境、构建器、分支/Tag、运行日志),经 API Server 的服务代理访问,
// 所以沿用当前用户的身份和 RBAC;控制器再用自己的令牌去读 deployer。
const BASE = '/api/v1/namespaces/okdev-system/services/okdev-controller:http/proxy';

export interface Builder {
  name: string;
  service: string;
  repo_url: string;
  ref: string;
}

export interface Refs {
  branches: string[];
  tags: string[];
  default?: string;
  builder_ref?: string;
}

export interface EnvService {
  env: string;
  service: string;
  desired_state: string;
  replicas: number;
  image: string;
  observed_sync: string;
  observed_health: string;
  observed_message: string;
  namespace: string;
  workload: string;
}

const opts = () => ({ cluster: getCluster() || undefined });

export function getJSON<T>(path: string): Promise<T> {
  return ApiProxy.request(BASE + path, opts(), false, true);
}

export async function getText(path: string): Promise<string> {
  const res = await ApiProxy.request(BASE + path, { ...opts(), isJSON: false }, false, true);
  return res.text();
}

interface State<T> {
  data: T | null;
  error: string;
  loading: boolean;
}

// 读一个接口;path 为空表示暂时不读;intervalMs > 0 时定时刷新(刷新时不闪烁加载状态)。
export function useGateway<T>(path: string, intervalMs = 0): State<T> {
  const [state, setState] = React.useState<State<T>>({ data: null, error: '', loading: !!path });
  React.useEffect(() => {
    if (!path) {
      setState({ data: null, error: '', loading: false });
      return;
    }
    let live = true;
    setState({ data: null, error: '', loading: true });
    const load = () =>
      getJSON<T>(path)
        .then(data => live && setState({ data, error: '', loading: false }))
        .catch(
          (e: any) =>
            live &&
            setState(s => ({ data: s.data, error: e?.message || String(e), loading: false }))
        );
    load();
    const t = intervalMs > 0 ? setInterval(load, intervalMs) : undefined;
    return () => {
      live = false;
      if (t) clearInterval(t);
    };
  }, [path, intervalMs]);
  return state;
}
