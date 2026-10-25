import { ApiProxy, Router } from '@kinvolk/headlamp-plugin/lib';
import { Link } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import { getCluster } from '@kinvolk/headlamp-plugin/lib/Utils';
import {
  Alert,
  Autocomplete,
  Box,
  Button,
  Checkbox,
  CircularProgress,
  FormControl,
  FormHelperText,
  InputLabel,
  ListSubheader,
  MenuItem,
  Paper,
  Select,
  TextField,
  Typography,
} from '@mui/material';
import React from 'react';
import { useHistory } from 'react-router-dom';
import { EnvironmentSelect } from './EnvironmentSelect';
import { useGitRefs } from './gitRefs';
import {
  ANNOTATION_GIT_REF,
  ANNOTATION_GIT_REPO,
  ANNOTATION_GITOPS_BRANCH,
  ANNOTATION_GITOPS_PATH,
  ANNOTATION_GITOPS_REPO,
} from './ReleaseTask';
import { ServiceItem, useServices } from './services';

// 每个被选中的服务一行。仓库和 GitOps 位置由平台通过工作负载注解固定,研发只选分支/Tag。
interface Platform {
  repo: string;
  ref: string; // 注解里的默认分支,可空
  gitopsRepo: string;
  gitopsBranch: string;
  gitopsPath: string;
}

function platformFor(s: ServiceItem): Platform {
  const a = s.obj.metadata.annotations || {};
  return {
    repo: a[ANNOTATION_GIT_REPO] || '',
    ref: a[ANNOTATION_GIT_REF] || '',
    gitopsRepo: a[ANNOTATION_GITOPS_REPO] || '',
    gitopsBranch: a[ANNOTATION_GITOPS_BRANCH] || '',
    gitopsPath: a[ANNOTATION_GITOPS_PATH] || '',
  };
}

function buildTask(env: string, s: ServiceItem, ref: string) {
  const p = platformFor(s);
  const spec: any = {
    service: { name: s.name, kind: s.kind },
    environment: env,
    git: { repo: p.repo, ref },
  };
  if (p.gitopsRepo) {
    spec.gitops = {
      repo: p.gitopsRepo,
      ...(p.gitopsBranch && { branch: p.gitopsBranch }),
      ...(p.gitopsPath && { path: p.gitopsPath }),
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
export function RunForm(props: { env: string; onEnvChange: (ns: string) => void }) {
  const { env, onEnvChange } = props;
  const history = useHistory();
  const { items } = useServices(env);
  const [selected, setSelected] = React.useState<ServiceItem[]>([]);
  const [refs, setRefs] = React.useState<Record<string, string>>({}); // 服务 key -> 所选分支/Tag
  const [submitting, setSubmitting] = React.useState(false);
  const [errors, setErrors] = React.useState<Record<string, string>>({});
  const [created, setCreated] = React.useState<{ name: string; service: string }[]>([]);

  // 换环境后,之前选的服务不再属于该环境
  React.useEffect(() => {
    setSelected([]);
    setRefs({});
    setErrors({});
    setCreated([]);
  }, [env]);

  const choose = (list: ServiceItem[]) => {
    setSelected(list);
    setRefs(prev =>
      Object.fromEntries(Object.entries(prev).filter(([k]) => list.some(s => s.key === k)))
    );
  };

  const valid = selected.length > 0 && selected.every(s => !!platformFor(s).repo && !!refs[s.key]);

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
            buildTask(env, s, refs[s.key]),
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

      {selected.map(s => (
        <ServiceRow
          key={s.key}
          service={s}
          value={refs[s.key] || ''}
          onChange={v => setRefs(prev => ({ ...prev, [s.key]: v }))}
          error={errors[s.key]}
        />
      ))}

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

function ServiceRow(props: {
  service: ServiceItem;
  value: string;
  onChange: (ref: string) => void;
  error?: string;
}) {
  const { service: s, value, onChange, error } = props;
  const p = platformFor(s);
  const { refs, error: refsError, loading } = useGitRefs(p.repo);

  // 候选加载完成后选默认值:注解里的默认分支 > 仓库默认分支 > 第一个分支
  React.useEffect(() => {
    if (!refs || value) return;
    const all = [...refs.branches, ...refs.tags];
    const pick = [p.ref, refs.default, refs.branches[0]].find(r => r && all.includes(r));
    if (pick) onChange(pick);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refs]);

  return (
    <Box
      data-testid={`row-${s.name}`}
      sx={{ mt: 2, p: 1.5, border: 1, borderColor: 'divider', borderRadius: 1 }}
    >
      <Typography variant="subtitle2" sx={{ mb: 1 }}>
        {s.name}{' '}
        <Typography component="span" variant="caption" color="text.secondary">
          {s.kind}
        </Typography>
      </Typography>
      {!p.repo ? (
        <Alert severity="warning">
          该服务没有配置 Git 仓库,请联系平台管理员在工作负载上添加注解 {ANNOTATION_GIT_REPO}。
        </Alert>
      ) : (
        <Box
          sx={{
            display: 'grid',
            gap: 2,
            gridTemplateColumns: { xs: '1fr', md: '2fr 1fr' },
            alignItems: 'center',
          }}
        >
          <Typography
            variant="body2"
            color="text.secondary"
            data-testid={`repo-${s.name}`}
            sx={{ wordBreak: 'break-all' }}
          >
            仓库:{p.repo}
            {p.gitopsRepo && (
              <>
                <br />
                部署配置:{p.gitopsRepo} {p.gitopsBranch} {p.gitopsPath}
              </>
            )}
          </Typography>
          <FormControl size="small" error={!!refsError}>
            <InputLabel id={`ref-label-${s.name}`}>分支 / Tag</InputLabel>
            <Select
              labelId={`ref-label-${s.name}`}
              label="分支 / Tag"
              value={value}
              disabled={loading || !refs}
              onChange={e => onChange(e.target.value as string)}
              data-testid={`ref-${s.name}`}
              endAdornment={loading ? <CircularProgress size={16} sx={{ mr: 3 }} /> : undefined}
            >
              {refs && refs.branches.length > 0 && <ListSubheader>分支</ListSubheader>}
              {refs?.branches.map(b => (
                <MenuItem key={`b-${b}`} value={b}>
                  {b}
                </MenuItem>
              ))}
              {refs && refs.tags.length > 0 && <ListSubheader>Tag</ListSubheader>}
              {refs?.tags.map(t => (
                <MenuItem key={`t-${t}`} value={t}>
                  {t}
                </MenuItem>
              ))}
            </Select>
            {refsError && <FormHelperText>无法获取分支列表:{refsError}</FormHelperText>}
          </FormControl>
        </Box>
      )}
      {error && (
        <Alert severity="error" sx={{ mt: 1.5 }}>
          创建失败:{error}
        </Alert>
      )}
    </Box>
  );
}
