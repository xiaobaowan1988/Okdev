import { SectionBox, Table } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import { Box, Link as MuiLink, Typography } from '@mui/material';
import React from 'react';
import { Link as RouterLink } from 'react-router-dom';
import { EnvironmentSelect, useEnvironment } from './EnvironmentSelect';
import { ServiceItem, useServices } from './services';

// 排障流:环境 -> 服务;点击服务名跳到原生详情页(Pod 列表、日志、终端都在那里)。
export function ServicesView() {
  const [env, setEnv] = useEnvironment();
  const { items, loading, errors } = useServices(env);

  return (
    <SectionBox
      title="Environment"
      headerProps={{ actions: [<EnvironmentSelect key="env" value={env} onChange={setEnv} />] }}
    >
      {errors.length > 0 && (
        <Typography color="error" sx={{ mb: 1 }}>
          Some resources failed to load: {errors.join('; ')}
        </Typography>
      )}
      <Table<ServiceItem>
        data={loading ? null : items}
        columns={[
          {
            header: 'Service',
            accessorKey: 'name',
            // getDetailsLink() 按资源类型给出原生路由:deployments / statefulsets / cronjobs / jobs。
            // 不用 Headlamp 的 <Link>:新版默认在侧边抽屉里打开详情,这里要整页跳转。
            Cell: ({ row }: any) => (
              <MuiLink component={RouterLink} to={row.original.obj.getDetailsLink()}>
                {row.original.name}
              </MuiLink>
            ),
          },
          { header: 'Kind', accessorKey: 'kind' },
          { header: 'Environment', accessorKey: 'namespace' },
          {
            header: 'Status',
            accessorFn: (s: ServiceItem) => statusText(s),
          },
          {
            header: 'Age',
            accessorFn: (s: ServiceItem) => s.obj.metadata.creationTimestamp,
            Cell: ({ row }: any) => row.original.obj.getAge?.() ?? '',
          },
        ]}
        emptyMessage={`No services in environment ${env}`}
      />
      <Box sx={{ mt: 1 }}>
        <Typography variant="caption" color="text.secondary">
          Jobs created by a CronJob or another controller are not listed separately.
        </Typography>
      </Box>
    </SectionBox>
  );
}

function statusText(s: ServiceItem): string {
  const j: any = s.obj.jsonData;
  switch (s.kind) {
    case 'Deployment':
    case 'StatefulSet':
      return `${j.status?.readyReplicas ?? 0}/${j.spec?.replicas ?? 0} ready`;
    case 'CronJob':
      return j.spec?.suspend ? 'Suspended' : j.spec?.schedule || '';
    default:
      return j.status?.succeeded ? 'Complete' : j.status?.failed ? 'Failed' : 'Running';
  }
}
