import { FormControl, InputLabel, MenuItem, Select } from '@mui/material';
import React from 'react';
import { useGateway } from './gateway';

// 环境来自 deployer(不是 Kubernetes 命名空间:每个 (环境, 服务) 部署在自己的命名空间里)。
export function EnvironmentSelect(props: {
  value: string;
  onChange: (env: string) => void;
  label?: string;
}) {
  const { data: envs } = useGateway<string[]>('/envs');
  const names = (envs || []).slice().sort();
  const label = props.label || 'Environment';

  // 记住的环境不存在了(或第一次打开)时,选第一个
  React.useEffect(() => {
    if (names.length > 0 && !names.includes(props.value)) props.onChange(names[0]);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [envs]);

  return (
    <FormControl size="small" sx={{ minWidth: 220 }}>
      <InputLabel id="okdev-env-label">{label}</InputLabel>
      <Select
        labelId="okdev-env-label"
        label={label}
        value={names.includes(props.value) ? props.value : ''}
        onChange={e => props.onChange(e.target.value as string)}
        data-testid="env-select"
      >
        {names.map(n => (
          <MenuItem key={n} value={n}>
            {n}
          </MenuItem>
        ))}
      </Select>
    </FormControl>
  );
}

const STORAGE_KEY = 'okdev.environment';

// 在环境页和工作流页之间记住所选环境
export function useEnvironment(): [string, (env: string) => void] {
  const [env, setEnv] = React.useState(() => {
    try {
      return localStorage.getItem(STORAGE_KEY) || '';
    } catch {
      return '';
    }
  });
  const set = (v: string) => {
    setEnv(v);
    try {
      localStorage.setItem(STORAGE_KEY, v);
    } catch {
      /* 存储不可用时只在当前页面生效 */
    }
  };
  return [env, set];
}
