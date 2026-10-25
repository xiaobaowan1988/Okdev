import { ApiProxy } from '@kinvolk/headlamp-plugin/lib';
import { getCluster } from '@kinvolk/headlamp-plugin/lib/Utils';
import React from 'react';

export interface GitRefs {
  branches: string[];
  tags: string[];
  default?: string;
}

// 控制器的只读接口,经 API Server 服务代理访问,所以沿用当前用户的身份和 RBAC。
const REFS_PATH = '/api/v1/namespaces/okdev-system/services/okdev-controller:http/proxy/refs';

export function useGitRefs(repo: string): {
  refs: GitRefs | null;
  error: string;
  loading: boolean;
} {
  const [state, setState] = React.useState<{
    refs: GitRefs | null;
    error: string;
    loading: boolean;
  }>({
    refs: null,
    error: '',
    loading: !!repo,
  });
  React.useEffect(() => {
    if (!repo) {
      setState({ refs: null, error: '', loading: false });
      return;
    }
    let live = true;
    setState({ refs: null, error: '', loading: true });
    ApiProxy.request(
      `${REFS_PATH}?repo=${encodeURIComponent(repo)}`,
      { cluster: getCluster() || undefined },
      false,
      true
    )
      .then((refs: GitRefs) => live && setState({ refs, error: '', loading: false }))
      .catch(
        (e: any) => live && setState({ refs: null, error: e?.message || String(e), loading: false })
      );
    return () => {
      live = false;
    };
  }, [repo]);
  return state;
}
