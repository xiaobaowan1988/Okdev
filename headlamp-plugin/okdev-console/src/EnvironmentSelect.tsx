import { K8s } from '@kinvolk/headlamp-plugin/lib';
import { FormControl, InputLabel, MenuItem, Select } from '@mui/material';
import React from 'react';

// 环境 = Namespace。
export function EnvironmentSelect(props: {
  value: string;
  onChange: (ns: string) => void;
  label?: string;
}) {
  const [namespaces] = K8s.ResourceClasses.Namespace.useList();
  const names = (namespaces || []).map(n => n.metadata.name).sort();
  return (
    <FormControl size="small" sx={{ minWidth: 220 }}>
      <InputLabel id="okdev-env-label">{props.label || 'Environment'}</InputLabel>
      <Select
        labelId="okdev-env-label"
        label={props.label || 'Environment'}
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

// 在服务页和工作流页之间记住所选环境
export function useEnvironment(): [string, (ns: string) => void] {
  const [env, setEnv] = React.useState(() => {
    try {
      return localStorage.getItem(STORAGE_KEY) || 'default';
    } catch {
      return 'default';
    }
  });
  const set = (ns: string) => {
    setEnv(ns);
    try {
      localStorage.setItem(STORAGE_KEY, ns);
    } catch {
      /* 存储不可用时只在当前页面生效 */
    }
  };
  return [env, set];
}
