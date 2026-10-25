import { ApiProxy, Router } from '@kinvolk/headlamp-plugin/lib';
import { Link } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import { getCluster } from '@kinvolk/headlamp-plugin/lib/Utils';
import {
  Alert,
  Autocomplete,
  Box,
  Button,
  Checkbox,
  Collapse,
  IconButton,
  Paper,
  TextField,
  Typography,
} from '@mui/material';
import React from 'react';
import { useHistory } from 'react-router-dom';
import { EnvironmentSelect } from './EnvironmentSelect';
import {
  ANNOTATION_GIT_REF,
  ANNOTATION_GIT_REPO,
  ANNOTATION_GITOPS_BRANCH,
  ANNOTATION_GITOPS_PATH,
  ANNOTATION_GITOPS_REPO,
  ReleaseTask,
} from './ReleaseTask';
import { ServiceItem, useServices } from './services';

// 每个被选中的服务一行,各自的仓库、分支(以及可选的 GitOps 设置)互相独立。
interface Row {
  repo: string;
  ref: string;
  gitopsRepo: string;
  gitopsBranch: string;
  gitopsPath: string;
}

function rowFor(s: ServiceItem): Row {
  const a = s.obj.metadata.annotations || {};
  return {
    repo: a[ANNOTATION_GIT_REPO] || '',
    ref: a[ANNOTATION_GIT_REF] || 'main',
    gitopsRepo: a[ANNOTATION_GITOPS_REPO] || '',
    gitopsBranch: a[ANNOTATION_GITOPS_BRANCH] || '',
    gitopsPath: a[ANNOTATION_GITOPS_PATH] || '',
  };
}

function buildTask(env: string, s: ServiceItem, r: Row) {
  const spec: any = {
    service: { name: s.name, kind: s.kind },
    environment: env,
    git: { repo: r.repo.trim(), ref: r.ref.trim() },
  };
  if (r.gitopsRepo.trim()) {
    spec.gitops = {
      repo: r.gitopsRepo.trim(),
      ...(r.gitopsBranch.trim() && { branch: r.gitopsBranch.trim() }),
      ...(r.gitopsPath.trim() && { path: r.gitopsPath.trim() }),
    };
  }
  return {
    apiVersion: 'ci.okdev.io/v1',
    kind: 'ReleaseTask',
    metadata: { generateName: `${s.name}-`.toLowerCase(), namespace: env },
    spec,
  };
}

// 运行工作流:一次选多个服务,每个服务各选分支;每个服务创建一个 ReleaseTask,互不影响。
export function RunForm(props: {
  env: string;
  onEnvChange: (ns: string) => void;
  tasks: ReleaseTask[];
}) {
  const { env, onEnvChange, tasks } = props;
  const history = useHistory();
  const { items } = useServices(env);
  const [selected, setSelected] = React.useState<ServiceItem[]>([]);
  const [rows, setRows] = React.useState<Record<string, Row>>({});
  const [open, setOpen] = React.useState<Record<string, boolean>>({});
  const [submitting, setSubmitting] = React.useState(false);
  const [errors, setErrors] = React.useState<Record<string, string>>({});
  const [created, setCreated] = React.useState<{ name: string; service: string }[]>([]);

  // 换环境后,之前选的服务不再属于该环境
  React.useEffect(() => {
    setSelected([]);
    setRows({});
    setErrors({});
    setCreated([]);
  }, [env]);

  // 每个服务以前用过的分支/Tag,作为分支输入框的候选
  const refOptions = (service: string) => {
    const seen = new Set<string>(['main', 'master']);
    tasks.forEach(
      t => t.spec?.service?.name === service && t.spec.git?.ref && seen.add(t.spec.git.ref)
    );
    return [...seen];
  };

  const choose = (list: ServiceItem[]) => {
    setSelected(list);
    setRows(prev => {
      const next: Record<string, Row> = {};
      list.forEach(s => (next[s.key] = prev[s.key] || rowFor(s)));
      return next;
    });
  };
  const patch = (key: string, p: Partial<Row>) =>
    setRows(prev => ({ ...prev, [key]: { ...prev[key], ...p } }));

  const rowValid = (s: ServiceItem) => !!rows[s.key]?.repo.trim() && !!rows[s.key]?.ref.trim();
  const valid = selected.length > 0 && selected.every(rowValid);

  const submit = async () => {
    setSubmitting(true);
    setErrors({});
    const ok: { name: string; service: string; namespace: string }[] = [];
    const failed: Record<string, string> = {};
    // 并发创建;某个服务失败不影响其他服务
    await Promise.all(
      selected.map(async s => {
        try {
          const res: any = await ApiProxy.post(
            `/apis/ci.okdev.io/v1/namespaces/${env}/releasetasks`,
            buildTask(env, s, rows[s.key]),
            true,
            { cluster: getCluster() || undefined }
          );
          ok.push({ name: res.metadata.name, namespace: res.metadata.namespace, service: s.name });
        } catch (e: any) {
          failed[s.key] = e?.message || String(e);
        }
      })
    );
    setSubmitting(false);

    if (ok.length === 1 && Object.keys(failed).length === 0) {
      // 只发布一个服务:直接进入详情页
      history.push(
        Router.createRouteURL('okdev-workflow-detail', {
          namespace: ok[0].namespace,
          name: ok[0].name,
        })
      );
      return;
    }
    // 多个服务:成功的从表单移走,失败的留下重试;任务列表会自动出现新任务
    setCreated(ok);
    setErrors(failed);
    setSelected(prev => prev.filter(s => failed[s.key]));
  };

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Box sx={{ display: 'grid', gap: 2, gridTemplateColumns: { xs: '1fr', md: '1fr 2fr' } }}>
        <EnvironmentSelect value={env} onChange={onEnvChange} label="目标环境" />
        <Autocomplete
          multiple
          disableCloseOnSelect
          size="small"
          options={items}
          value={selected}
          onChange={(_, v) => choose(v)}
          getOptionLabel={s => `${s.name} (${s.kind})`}
          isOptionEqualToValue={(a, b) => a.key === b.key}
          renderOption={(p, s, { selected: sel }) => (
            <li {...p} key={s.key}>
              <Checkbox size="small" checked={sel} sx={{ mr: 1 }} />
              {s.name} ({s.kind})
            </li>
          )}
          renderInput={params => (
            <TextField {...params} label="服务(可多选)" data-testid="service-input" />
          )}
        />
      </Box>

      {selected.map(s => {
        const r = rows[s.key] || rowFor(s);
        const hasGitops = !!(r.gitopsRepo || open[s.key]);
        return (
          <Box
            key={s.key}
            data-testid={`row-${s.name}`}
            sx={{ mt: 2, p: 1.5, border: 1, borderColor: 'divider', borderRadius: 1 }}
          >
            <Typography variant="subtitle2" sx={{ mb: 1 }}>
              {s.name}{' '}
              <Typography component="span" variant="caption" color="text.secondary">
                {s.kind}
              </Typography>
            </Typography>
            <Box
              sx={{
                display: 'grid',
                gap: 2,
                gridTemplateColumns: { xs: '1fr', md: '2fr 1fr auto' },
                alignItems: 'center',
              }}
            >
              <TextField
                size="small"
                label="Git 仓库"
                value={r.repo}
                onChange={e => patch(s.key, { repo: e.target.value })}
                placeholder="https://github.com/org/app.git"
                inputProps={{ 'data-testid': `repo-${s.name}` }}
              />
              <Autocomplete
                freeSolo
                size="small"
                options={refOptions(s.name)}
                inputValue={r.ref}
                onInputChange={(_, v) => patch(s.key, { ref: v })}
                renderInput={params => (
                  <TextField
                    {...params}
                    label="分支 / Tag / Commit"
                    inputProps={{ ...params.inputProps, 'data-testid': `ref-${s.name}` }}
                  />
                )}
              />
              <IconButton
                size="small"
                onClick={() => setOpen({ ...open, [s.key]: !hasGitops })}
                title="GitOps 设置"
              >
                <Typography variant="caption">{hasGitops ? '收起' : 'GitOps'}</Typography>
              </IconButton>
            </Box>
            <Collapse in={hasGitops}>
              <Box
                sx={{
                  display: 'grid',
                  gap: 2,
                  mt: 1.5,
                  gridTemplateColumns: { xs: '1fr', md: '2fr 1fr 2fr' },
                }}
              >
                <TextField
                  size="small"
                  label="GitOps 仓库"
                  value={r.gitopsRepo}
                  onChange={e => patch(s.key, { gitopsRepo: e.target.value })}
                  inputProps={{ 'data-testid': `gitops-repo-${s.name}` }}
                />
                <TextField
                  size="small"
                  label="分支"
                  value={r.gitopsBranch}
                  onChange={e => patch(s.key, { gitopsBranch: e.target.value })}
                  placeholder="main"
                />
                <TextField
                  size="small"
                  label="配置目录"
                  value={r.gitopsPath}
                  onChange={e => patch(s.key, { gitopsPath: e.target.value })}
                  placeholder="envs/dev/app"
                  inputProps={{ 'data-testid': `gitops-path-${s.name}` }}
                />
              </Box>
            </Collapse>
            {errors[s.key] && (
              <Alert severity="error" sx={{ mt: 1.5 }}>
                创建失败:{errors[s.key]}
              </Alert>
            )}
          </Box>
        );
      })}

      {created.length > 0 && (
        <Alert severity="success" sx={{ mt: 2 }} data-testid="created-tasks">
          已创建 {created.length} 个任务:
          {created.map(c => (
            <span key={c.name} style={{ marginLeft: 8 }}>
              <Link routeName="okdev-workflow-detail" params={{ namespace: env, name: c.name }}>
                {c.service} ({c.name})
              </Link>
            </span>
          ))}
        </Alert>
      )}

      <Box sx={{ mt: 2 }}>
        <Button
          variant="contained"
          disabled={!valid || submitting}
          onClick={submit}
          data-testid="run-button"
        >
          {submitting ? '创建中…' : selected.length > 1 ? `运行 ${selected.length} 个服务` : '运行'}
        </Button>
      </Box>
    </Paper>
  );
}
