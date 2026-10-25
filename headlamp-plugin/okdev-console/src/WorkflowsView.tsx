import { Link, SectionBox, Table } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import React from 'react';
import { useEnvironment } from './EnvironmentSelect';
import { PhaseChip } from './PhaseChip';
import { RELEASE_NAMESPACE, ReleaseTask } from './ReleaseTask';
import { RunForm } from './RunForm';

// 工作流:选环境 / 服务 / 分支或 Tag,点击运行 -> 创建 ReleaseTask -> 由 deployer 构建并部署。
export function WorkflowsView() {
  const [env, setEnv] = useEnvironment();
  const [tasks] = ReleaseTask.useList({ namespace: RELEASE_NAMESPACE });
  const sorted = (tasks || [])
    .filter(t => !env || t.spec?.environment === env)
    .sort((a, b) =>
      String(b.metadata.creationTimestamp).localeCompare(String(a.metadata.creationTimestamp))
    );

  return (
    <>
      <SectionBox title="Run workflow">
        <RunForm env={env} onEnvChange={setEnv} />
      </SectionBox>
      <SectionBox title={`Recent release tasks (${env})`}>
        <Table<ReleaseTask>
          data={tasks === null ? null : sorted}
          columns={[
            {
              header: 'Task',
              accessorFn: (t: ReleaseTask) => t.metadata.name,
              Cell: ({ row }: any) => (
                <Link kubeObject={row.original}>{row.original.metadata.name}</Link>
              ),
            },
            { header: 'Service', accessorFn: (t: ReleaseTask) => t.spec?.service },
            { header: 'Git Ref', accessorFn: (t: ReleaseTask) => t.spec?.ref || '(default)' },
            {
              header: 'Status',
              accessorFn: (t: ReleaseTask) => t.phase,
              Cell: ({ row }: any) => <PhaseChip phase={row.original.phase} />,
            },
            { header: 'Stage', accessorFn: (t: ReleaseTask) => t.status.stage || '' },
            { header: 'Age', accessorFn: (t: ReleaseTask) => t.getAge() },
          ]}
          emptyMessage="No release tasks yet"
        />
      </SectionBox>
    </>
  );
}
