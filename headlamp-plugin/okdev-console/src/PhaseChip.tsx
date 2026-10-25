import { Chip } from '@mui/material';
import React from 'react';
import { Phase } from './ReleaseTask';

const COLORS: Record<Phase, 'default' | 'info' | 'success' | 'error'> = {
  Pending: 'default',
  Running: 'info',
  Success: 'success',
  Failed: 'error',
};

export function PhaseChip({ phase }: { phase: Phase }) {
  return (
    <Chip size="small" label={phase} color={COLORS[phase] || 'default'} data-testid="phase-chip" />
  );
}
