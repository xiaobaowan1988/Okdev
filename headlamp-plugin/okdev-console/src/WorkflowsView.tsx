import { ApiProxy, Router } from '@kinvolk/headlamp-plugin/lib';
import { Link, SectionBox, Table } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import { getCluster } from '@kinvolk/headlamp-plugin/lib/Utils';
import {
  Alert,
  Autocomplete,
  Box,
  Button,
  Collapse,
  Paper,
  TextField,
  Typography,
} from '@mui/material';
import React from 'react';
import { useHistory } from 'react-router-dom';
import { EnvironmentSelect, useEnvironment } from './EnvironmentSelect';
import { PhaseChip } from './PhaseChip';
import {
  ANNOTATION_GIT_REF,
  ANNOTATION_GIT_REPO,
  ANNOTATION_GITOPS_BRANCH,
  ANNOTATION_GITOPS_PATH,
  ANNOTATION_GITOPS_REPO,
  ReleaseTask,
} from './ReleaseTask';
import { ServiceItem, useServices } from './services';

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
      <SectionBox title="运行工作流">
        <RunForm env={env} onEnvChange={setEnv} />
      </SectionBox>
      <SectionBox title={`最近的发布任务(${env})`}>
        <Table<ReleaseTask>
          data={tasks === null ? null : sorted}
          columns={[
            {
              header: '任务',
              accessorFn: (t: ReleaseTask) => t.metadata.name,
              Cell: ({ row }: any) => (
                <Link kubeObject={row.original}>{row.original.metadata.name}</Link>
              ),
            },
            { header: '服务', accessorFn: (t: ReleaseTask) => t.spec?.service?.name },
            { header: 'Kind', accessorFn: (t: ReleaseTask) => t.spec?.service?.kind },
            { header: 'Git Ref', accessorFn: (t: ReleaseTask) => t.spec?.git?.ref },
            {
              header: '状态',
              accessorFn: (t: ReleaseTask) => t.phase,
              Cell: ({ row }: any) => <PhaseChip phase={row.original.phase} />,
            },
            { header: '创建时间', accessorFn: (t: ReleaseTask) => t.getAge() },
          ]}
          emptyMessage="还没有发布任务"
        />
      </SectionBox>
    </>
  );
}

function RunForm(props: { env: string; onEnvChange: (ns: string) => void }) {
  const { env, onEnvChange } = props;
  const history = useHistory();
  const { items } = useServices(env);
  const [service, setService] = React.useState<ServiceItem | null>(null);
  const [repo, setRepo] = React.useState('');
  const [ref, setRef] = React.useState('');
  const [gitopsRepo, setGitopsRepo] = React.useState('');
  const [gitopsBranch, setGitopsBranch] = React.useState('');
  const [gitopsPath, setGitopsPath] = React.useState('');
  const [advanced, setAdvanced] = React.useState(false);
  const [submitting, setSubmitting] = React.useState(false);
  const [error, setError] = React.useState('');

  // 换环境后,之前选的服务不再属于该环境
  React.useEffect(() => setService(null), [env]);

  const pick = (s: ServiceItem | null) => {
    setService(s);
    const a = s?.obj.metadata.annotations || {};
    setRepo(a[ANNOTATION_GIT_REPO] || '');
    setRef(a[ANNOTATION_GIT_REF] || 'main');
    setGitopsRepo(a[ANNOTATION_GITOPS_REPO] || '');
    setGitopsBranch(a[ANNOTATION_GITOPS_BRANCH] || '');
    setGitopsPath(a[ANNOTATION_GITOPS_PATH] || '');
    if (a[ANNOTATION_GITOPS_REPO]) setAdvanced(true);
  };

  const valid = !!service && repo.trim() !== '' && ref.trim() !== '';

  const submit = async () => {
    if (!service) return;
    setSubmitting(true);
    setError('');
    const body: any = {
      apiVersion: 'ci.okdev.io/v1',
      kind: 'ReleaseTask',
      metadata: { generateName: `${service.name}-`.toLowerCase(), namespace: env },
      spec: {
        service: { name: service.name, kind: service.kind },
        environment: env,
        git: { repo: repo.trim(), ref: ref.trim() },
      },
    };
    if (gitopsRepo.trim()) {
      body.spec.gitops = {
        repo: gitopsRepo.trim(),
        ...(gitopsBranch.trim() && { branch: gitopsBranch.trim() }),
        ...(gitopsPath.trim() && { path: gitopsPath.trim() }),
      };
    }
    try {
      // 经 Headlamp 的 API 代理创建自定义资源
      const created: any = await ApiProxy.post(
        `/apis/ci.okdev.io/v1/namespaces/${env}/releasetasks`,
        body,
        true,
        { cluster: getCluster() || undefined }
      );
      history.push(
        Router.createRouteURL('okdev-workflow-detail', {
          namespace: created.metadata.namespace,
          name: created.metadata.name,
        })
      );
    } catch (e: any) {
      setError(e?.message || String(e));
      setSubmitting(false);
    }
  };

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Box sx={{ display: 'grid', gap: 2, gridTemplateColumns: { xs: '1fr', md: '1fr 1fr' } }}>
        <EnvironmentSelect value={env} onChange={onEnvChange} label="目标环境" />
        <Autocomplete
          size="small"
          options={items}
          value={service}
          onChange={(_, v) => pick(v)}
          getOptionLabel={s => `${s.name} (${s.kind})`}
          isOptionEqualToValue={(a, b) => a.key === b.key}
          renderInput={params => <TextField {...params} label="服务" data-testid="service-input" />}
        />
        <TextField
          size="small"
          label="Git 仓库"
          value={repo}
          onChange={e => setRepo(e.target.value)}
          placeholder="https://github.com/org/app.git"
          inputProps={{ 'data-testid': 'git-repo' }}
        />
        <TextField
          size="small"
          label="分支 / Tag / Commit"
          value={ref}
          onChange={e => setRef(e.target.value)}
          inputProps={{ 'data-testid': 'git-ref' }}
        />
      </Box>
      <Button size="small" sx={{ mt: 1 }} onClick={() => setAdvanced(!advanced)}>
        {advanced ? '收起 GitOps 设置' : 'GitOps 设置(可选)'}
      </Button>
      <Collapse in={advanced}>
        <Box
          sx={{
            display: 'grid',
            gap: 2,
            mt: 1,
            gridTemplateColumns: { xs: '1fr', md: '2fr 1fr 2fr' },
          }}
        >
          <TextField
            size="small"
            label="GitOps 仓库"
            value={gitopsRepo}
            onChange={e => setGitopsRepo(e.target.value)}
            inputProps={{ 'data-testid': 'gitops-repo' }}
          />
          <TextField
            size="small"
            label="分支"
            value={gitopsBranch}
            onChange={e => setGitopsBranch(e.target.value)}
            placeholder="main"
          />
          <TextField
            size="small"
            label="配置目录"
            value={gitopsPath}
            onChange={e => setGitopsPath(e.target.value)}
            placeholder="envs/dev/app"
            inputProps={{ 'data-testid': 'gitops-path' }}
          />
        </Box>
        <Typography variant="caption" color="text.secondary">
          留空则只构建镜像,不更新部署配置。仓库、分支、目录可通过工作负载注解 okdev.io/* 预填。
        </Typography>
      </Collapse>
      {error && (
        <Alert severity="error" sx={{ mt: 2 }}>
          创建失败:{error}
        </Alert>
      )}
      <Box sx={{ mt: 2 }}>
        <Button
          variant="contained"
          disabled={!valid || submitting}
          onClick={submit}
          data-testid="run-button"
        >
          {submitting ? '创建中…' : '运行'}
        </Button>
      </Box>
    </Paper>
  );
}
