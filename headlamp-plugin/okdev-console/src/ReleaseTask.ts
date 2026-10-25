import { KubeObject, KubeObjectInterface } from '@kinvolk/headlamp-plugin/lib/k8s/cluster';

export type Phase = 'Pending' | 'Running' | 'Success' | 'Failed';

// 发布任务只在这个命名空间里生效(控制器只监听它,研发也只在这里有创建权限)
export const RELEASE_NAMESPACE = 'okdev-releases';

export interface ReleaseTaskInterface extends KubeObjectInterface {
  spec: {
    service: string;
    builder: string;
    environment: string;
    ref?: string;
    pipeline?: string;
    replicas?: number;
  };
  status?: {
    phase?: Phase;
    stage?: string;
    runID?: number;
    image?: string;
    message?: string;
    startTime?: string;
    completionTime?: string;
  };
}

// 控制器(Okdev)注册的 CRD:ci.okdev.io/v1 ReleaseTask。构建和部署由 deployer 执行。
export class ReleaseTask extends KubeObject<ReleaseTaskInterface> {
  static apiVersion = 'ci.okdev.io/v1';
  static kind = 'ReleaseTask';
  static apiName = 'releasetasks';
  static isNamespaced = true;

  static get detailsRoute() {
    return '/workflows/:namespace/:name';
  }

  get spec() {
    return this.jsonData.spec;
  }

  get status() {
    return this.jsonData.status || {};
  }

  get phase(): Phase {
    return this.status.phase || 'Pending';
  }
}
