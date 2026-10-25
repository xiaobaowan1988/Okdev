import { Link, SectionBox, Table } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import React from 'react';
import { useEnvironment } from './EnvironmentSelect';
import { PhaseChip } from './PhaseChip';
import { ReleaseTask } from './ReleaseTask';
import { RunForm } from './RunForm';

// 工作流:选服务 / 环境 / Git 分支或 Tag,点击运行 -> 创建 ReleaseTask -> 跳转详情页。
export function WorkflowsView() {
  const [env, setEnv] = useEnvironment();
  const [tasks] = ReleaseTask.useList({ namespace: env });
  const sorted = (tasks || [])
    .slice()
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
            { header: 'Service', accessorFn: (t: ReleaseTask) => t.spec?.service?.name },
            { header: 'Kind', accessorFn: (t: ReleaseTask) => t.spec?.service?.kind },
            { header: 'Git Ref', accessorFn: (t: ReleaseTask) => t.spec?.git?.ref },
            {
              header: 'Status',
              accessorFn: (t: ReleaseTask) => t.phase,
              Cell: ({ row }: any) => <PhaseChip phase={row.original.phase} />,
            },
            { header: 'Age', accessorFn: (t: ReleaseTask) => t.getAge() },
          ]}
          emptyMessage="No release tasks yet"
        />
      </SectionBox>
    </>
  );
}
