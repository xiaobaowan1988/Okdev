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
import { Builder, Refs, useGateway } from './gateway';
import { RELEASE_NAMESPACE } from './ReleaseTask';

// 每个被选中的服务一行:选构建器(一个服务可能有多个)和分支/Tag。仓库由 deployer 里的构建器决定,只读展示。
interface Row {
  builder: string;
  ref: string;
}

async function createTask(env: string, service: string, builder: string, ref: string) {
  const body = {
    apiVersion: 'ci.okdev.io/v1',
    kind: 'ReleaseTask',
    metadata: { generateName: `${service}-${env}-`.toLowerCase(), namespace: RELEASE_NAMESPACE },
    spec: { service, builder, environment: env, ...(ref && { ref }) },
  };
  const res: any = await ApiProxy.post(
    `/apis/ci.okdev.io/v1/namespaces/${RELEASE_NAMESPACE}/releasetasks`,
    body,
    true,
    { cluster: getCluster() || undefined }
  );
  return res.metadata.name as string;
}

// 运行工作流:一次选多个服务,每个服务各选分支;每个服务创建一个 ReleaseTask,互不影响。
export function RunForm(props: { env: string; onEnvChange: (env: string) => void }) {
  const { env, onEnvChange } = props;
  const history = useHistory();
  const { data: builders } = useGateway<Builder[]>('/builders');
  const services = React.useMemo(
    () => Array.from(new Set((builders || []).map(b => b.service))).sort(),
    [builders]
  );
  const buildersOf = (service: string) => (builders || []).filter(b => b.service === service);

  const [selected, setSelected] = React.useState<string[]>([]);
  const [rows, setRows] = React.useState<Record<string, Row>>({});
  const [submitting, setSubmitting] = React.useState(false);
  const [errors, setErrors] = React.useState<Record<string, string>>({});
  const [created, setCreated] = React.useState<{ name: string; service: string }[]>([]);

  const choose = (list: string[]) => {
    setSelected(list);
    setRows(prev => {
      const next: Record<string, Row> = {};
      list.forEach(s => (next[s] = prev[s] || { builder: buildersOf(s)[0]?.name || '', ref: '' }));
      return next;
    });
  };
  const patch = (service: string, p: Partial<Row>) =>
    setRows(prev => ({ ...prev, [service]: { ...prev[service], ...p } }));

  const valid =
    !!env && selected.length > 0 && selected.every(s => !!rows[s]?.builder && !!rows[s]?.ref);

  const submit = async () => {
    setSubmitting(true);
    setErrors({});
    const ok: { name: string; service: string }[] = [];
    const failed: Record<string, string> = {};
    // 并发创建;某个服务失败不影响其他服务
    await Promise.all(
      selected.map(async s => {
        try {
          ok.push({ name: await createTask(env, s, rows[s].builder, rows[s].ref), service: s });
        } catch (e: any) {
          failed[s] = e?.message || String(e);
        }
      })
    );
    setSubmitting(false);

    if (ok.length === 1 && Object.keys(failed).length === 0) {
      // 只发布一个服务:直接进入详情页
      history.push(
        Router.createRouteURL('okdev-workflow-detail', {
          namespace: RELEASE_NAMESPACE,
          name: ok[0].name,
        })
      );
      return;
    }
    // 多个服务:成功的从表单移走,失败的留下重试;任务列表会自动出现新任务
    setCreated(ok);
    setErrors(failed);
    setSelected(prev => prev.filter(s => failed[s]));
  };

  return (
    <Paper variant="outlined" sx={{ p: 2 }}>
      <Box sx={{ display: 'grid', gap: 2, gridTemplateColumns: { xs: '1fr', md: '1fr 2fr' } }}>
        <EnvironmentSelect
          value={env}
          onChange={e => {
            onEnvChange(e);
            setCreated([]);
          }}
          label="Target environment"
        />
        <Autocomplete
          multiple
          disableCloseOnSelect
          size="small"
          options={services}
          value={selected}
          onChange={(_, v) => choose(v)}
          renderOption={(p, s, { selected: sel }) => (
            <li {...p} key={s}>
              <Checkbox size="small" checked={sel} sx={{ mr: 1 }} />
              {s}
            </li>
          )}
          renderInput={params => (
            <TextField {...params} label="Services (multiple)" data-testid="service-input" />
          )}
        />
      </Box>

      {selected.map(s => (
        <ServiceRow
          key={s}
          service={s}
          builders={buildersOf(s)}
          row={rows[s] || { builder: '', ref: '' }}
          onChange={p => patch(s, p)}
          error={errors[s]}
        />
      ))}

      {created.length > 0 && (
        <Alert severity="success" sx={{ mt: 2 }} data-testid="created-tasks">
          Created {created.length} task(s):
          {created.map(c => (
            <span key={c.name} style={{ marginLeft: 8 }}>
              <Link
                routeName="okdev-workflow-detail"
                params={{ namespace: RELEASE_NAMESPACE, name: c.name }}
              >
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
          {submitting
            ? 'Creating…'
            : selected.length > 1
            ? `Run ${selected.length} services`
            : 'Run'}
        </Button>
      </Box>
    </Paper>
  );
}

function ServiceRow(props: {
  service: string;
  builders: Builder[];
  row: Row;
  onChange: (p: Partial<Row>) => void;
  error?: string;
}) {
  const { service, builders, row, onChange, error } = props;
  const builder = builders.find(b => b.name === row.builder);
  const {
    data: refs,
    error: refsError,
    loading,
  } = useGateway<Refs>(builder ? `/refs?builder=${encodeURIComponent(builder.name)}` : '');

  // 候选加载完成后选默认值:构建器配置的默认分支 > 仓库默认分支 > 第一个分支
  React.useEffect(() => {
    if (!refs || row.ref) return;
    const all = [...refs.branches, ...refs.tags];
    const pick = [refs.builder_ref, refs.default, refs.branches[0]].find(r => r && all.includes(r));
    if (pick) onChange({ ref: pick });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refs]);

  return (
    <Box
      data-testid={`row-${service}`}
      sx={{ mt: 2, p: 1.5, border: 1, borderColor: 'divider', borderRadius: 1 }}
    >
      <Typography variant="subtitle2" sx={{ mb: 1 }}>
        {service}
      </Typography>
      {builders.length === 0 ? (
        <Alert severity="warning">
          This service has no builder in deployer. Ask a platform admin to register one.
        </Alert>
      ) : (
        <Box
          sx={{
            display: 'grid',
            gap: 2,
            gridTemplateColumns: { xs: '1fr', md: '1fr 1fr 1fr' },
            alignItems: 'start',
          }}
        >
          <FormControl size="small" disabled={builders.length === 1}>
            <InputLabel id={`builder-label-${service}`}>Builder</InputLabel>
            <Select
              labelId={`builder-label-${service}`}
              label="Builder"
              value={row.builder}
              onChange={e => onChange({ builder: e.target.value as string, ref: '' })}
              data-testid={`builder-${service}`}
            >
              {builders.map(b => (
                <MenuItem key={b.name} value={b.name}>
                  {b.name}
                </MenuItem>
              ))}
            </Select>
          </FormControl>
          <Typography
            variant="body2"
            color="text.secondary"
            data-testid={`repo-${service}`}
            sx={{ wordBreak: 'break-all', pt: 0.5 }}
          >
            Repository: {builder?.repo_url}
          </Typography>
          <FormControl size="small" error={!!refsError}>
            <InputLabel id={`ref-label-${service}`}>Branch / Tag</InputLabel>
            <Select
              labelId={`ref-label-${service}`}
              label="Branch / Tag"
              value={row.ref}
              disabled={loading || !refs}
              onChange={e => onChange({ ref: e.target.value as string })}
              data-testid={`ref-${service}`}
              endAdornment={loading ? <CircularProgress size={16} sx={{ mr: 3 }} /> : undefined}
            >
              {refs && refs.branches.length > 0 && <ListSubheader>Branches</ListSubheader>}
              {refs?.branches.map(b => (
                <MenuItem key={`b-${b}`} value={b}>
                  {b}
                </MenuItem>
              ))}
              {refs && refs.tags.length > 0 && <ListSubheader>Tags</ListSubheader>}
              {refs?.tags.map(t => (
                <MenuItem key={`t-${t}`} value={t}>
                  {t}
                </MenuItem>
              ))}
            </Select>
            {refsError && <FormHelperText>Could not load branches: {refsError}</FormHelperText>}
          </FormControl>
        </Box>
      )}
      {error && (
        <Alert severity="error" sx={{ mt: 1.5 }}>
          Creation failed: {error}
        </Alert>
      )}
    </Box>
  );
}
