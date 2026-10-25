import { KubeObject, KubeObjectInterface } from '@kinvolk/headlamp-plugin/lib/k8s/cluster';

export type Phase = 'Pending' | 'Running' | 'Success' | 'Failed';

export interface ReleaseTaskInterface extends KubeObjectInterface {
  spec: {
    service: { name: string; kind: string };
    environment: string;
    git: { repo: string; ref: string };
    gitops?: { repo: string; branch?: string; path?: string };
  };
  status?: {
    phase?: Phase;
    jobName?: string;
    executorPodName?: string;
    message?: string;
    startTime?: string;
    completionTime?: string;
  };
}

// 控制器(Okdev)注册的 CRD:ci.okdev.io/v1 ReleaseTask
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

// 工作负载上的注解,用来给"运行工作流"表单预填默认值
export const ANNOTATION_GIT_REPO = 'okdev.io/git-repo';
export const ANNOTATION_GIT_REF = 'okdev.io/git-ref';
export const ANNOTATION_GITOPS_REPO = 'okdev.io/gitops-repo';
export const ANNOTATION_GITOPS_BRANCH = 'okdev.io/gitops-branch';
export const ANNOTATION_GITOPS_PATH = 'okdev.io/gitops-path';
