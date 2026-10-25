import { Router } from '@kinvolk/headlamp-plugin/lib';
import { SectionBox, Table } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import { Alert, Box, Link as MuiLink, Typography } from '@mui/material';
import React from 'react';
import { Link as RouterLink } from 'react-router-dom';
import { EnvironmentSelect, useEnvironment } from './EnvironmentSelect';
import { EnvService, useGateway } from './gateway';

// 排障流:环境 -> 服务;点击服务名跳到 Headlamp 原生的工作负载页(Pod 列表、日志、终端都在那里)。
// 数据来自 deployer:每个 (环境, 服务) 的期望状态和 Argo CD 回写的观测状态。
export function ServicesView() {
  const [env, setEnv] = useEnvironment();
  const { data, error, loading } = useGateway<EnvService[]>(
    env ? `/envs/${encodeURIComponent(env)}/services` : '',
    5000
  );

  return (
    <SectionBox
      title="Environment"
      headerProps={{ actions: [<EnvironmentSelect key="env" value={env} onChange={setEnv} />] }}
    >
      {error && (
        <Alert severity="error" sx={{ mb: 1 }}>
          Failed to load services: {error}
        </Alert>
      )}
      <Table<EnvService>
        data={loading || !data ? null : data}
        columns={[
          {
            header: 'Service',
            accessorKey: 'service',
            // 每个 (环境, 服务) 在自己的命名空间里,工作负载是 Deployment
            Cell: ({ row }: any) => (
              <MuiLink
                component={RouterLink}
                to={Router.createRouteURL('deployment', {
                  namespace: row.original.namespace,
                  name: row.original.workload,
                })}
              >
                {row.original.service}
              </MuiLink>
            ),
          },
          { header: 'Namespace', accessorKey: 'namespace' },
          { header: 'Image', accessorKey: 'image' },
          {
            header: 'Replicas',
            accessorFn: (s: EnvService) =>
              s.desired_state === 'absent' ? 'removing' : String(s.replicas),
          },
          { header: 'Sync', accessorKey: 'observed_sync' },
          { header: 'Health', accessorKey: 'observed_health' },
          { header: 'Message', accessorKey: 'observed_message' },
        ]}
        emptyMessage={env ? `No services in environment ${env}` : 'No environments'}
      />
      <Box sx={{ mt: 1 }}>
        <Typography variant="caption" color="text.secondary">
          Each service of an environment runs in its own namespace
          (&lt;environment&gt;-env-&lt;service&gt;). Refreshes every 5 seconds.
        </Typography>
      </Box>
    </SectionBox>
  );
}
