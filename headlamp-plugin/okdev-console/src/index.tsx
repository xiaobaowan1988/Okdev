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
  label: 'Environment',
  url: '/services',
  icon: 'mdi:apps',
});
registerSidebarEntry({
  parent: null,
  name: 'okdev-workflows',
  label: 'Workflow',
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

// 侧边栏底部的 "Create"(新建任意资源的 YAML 编辑器)没有官方开关可关,
// 这里用 MutationObserver 把侧边栏抽屉里的该按钮隐藏。只隐藏抽屉内的,
// 原生详情页里的编辑/创建按钮不受影响。
function hideSidebarCreateButton() {
  const hide = () =>
    document.querySelectorAll<HTMLButtonElement>('.MuiDrawer-root button').forEach(btn => {
      // 展开时是带文字的 "Create" 按钮;侧边栏收起(窄屏)时变成只有图标、aria-label 为 "Create / Apply" 的按钮
      if (btn.textContent?.trim() === 'Create' && btn.parentElement) {
        btn.parentElement.style.display = 'none';
      } else if (btn.getAttribute('aria-label')?.startsWith('Create')) {
        btn.style.display = 'none';
      }
    });
  new MutationObserver(hide).observe(document.body, { childList: true, subtree: true });
  hide();
}
hideSidebarCreateButton();
