import {
  registerRoute,
  registerSidebarEntry,
  registerSidebarEntryFilter,
} from '@kinvolk/headlamp-plugin/lib';
import React from 'react';
import { ServicesView } from './ServicesView';
import { TaskDetail } from './TaskDetail';
import { WorkflowsView } from './WorkflowsView';

// 顶层侧边栏只保留:环境(排障流:环境 -> 服务 -> Pod)、工作流(发布流)。
// 原生菜单只是从侧边栏隐藏,路由还在,从环境页点进去仍能用原生的详情、日志和终端。
registerSidebarEntry({
  parent: null,
  name: 'okdev-services',
  label: '环境',
  url: '/services',
  icon: 'mdi:apps',
});
registerSidebarEntry({
  parent: null,
  name: 'okdev-workflows',
  label: '工作流',
  url: '/workflows',
  icon: 'mdi:rocket-launch',
});

registerRoute({
  path: '/services',
  sidebar: 'okdev-services',
  name: 'okdev-services',
  exact: true,
  component: () => <ServicesView />,
});

registerRoute({
  path: '/workflows',
  sidebar: 'okdev-workflows',
  name: 'okdev-workflows',
  exact: true,
  component: () => <WorkflowsView />,
});

// 任务详情。CRD 是命名空间级资源,路径里需要 namespace。
registerRoute({
  path: '/workflows/:namespace/:name',
  sidebar: 'okdev-workflows',
  name: 'okdev-workflow-detail',
  exact: true,
  component: () => <TaskDetail />,
});

registerSidebarEntryFilter(entry => (entry.name.startsWith('okdev-') ? entry : null));
