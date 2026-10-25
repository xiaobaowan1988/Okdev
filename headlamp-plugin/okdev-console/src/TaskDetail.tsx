import {
  BackLink,
  Loader,
  LogViewer,
  NameValueTable,
  SectionBox,
} from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import Event from '@kinvolk/headlamp-plugin/lib/k8s/event';
import { Alert, LinearProgress, Step, StepLabel, Stepper, Typography } from '@mui/material';
import React from 'react';
import { useParams } from 'react-router-dom';
import { getText } from './gateway';
import { PhaseChip } from './PhaseChip';
import { Phase, ReleaseTask } from './ReleaseTask';

export function TaskDetail() {
  const { namespace, name } = useParams<{ namespace: string; name: string }>();
  const [task, error] = ReleaseTask.useGet(name, namespace);

  if (error && !task)
    return (
      <Alert severity="error">Failed to load task: {String((error as any).message || error)}</Alert>
    );
  if (!task) return <Loader title="Loading task" />;

  return (
    <>
      <BackLink />
      <SectionBox title={`Release task ${name}`}>
        <PhaseBar phase={task.phase} />
        {task.status.message && (
          <Alert
            severity={task.phase === 'Failed' ? 'error' : 'info'}
            sx={{ mt: 2 }}
            data-testid="task-message"
          >
            {task.status.message}
          </Alert>
        )}
        <NameValueTable
          rows={[
            { name: 'Status', value: <PhaseChip phase={task.phase} /> },
            { name: 'Stage', value: task.status.stage || '-' },
            { name: 'Service', value: task.spec.service },
            { name: 'Builder', value: task.spec.builder },
            { name: 'Environment', value: task.spec.environment },
            { name: 'Git ref', value: task.spec.ref || '(builder default)' },
            { name: 'Image', value: task.status.image || '-' },
            { name: 'Run', value: task.status.runID ? `#${task.status.runID}` : '-' },
            { name: 'Started', value: task.status.startTime || '-' },
            { name: 'Finished', value: task.status.completionTime || '-' },
          ]}
        />
      </SectionBox>
      <SectionBox title="Events">
        <TaskEvents task={task} />
      </SectionBox>
      <SectionBox title="Build log">
        <TaskLog task={task} />
      </SectionBox>
    </>
  );
}

function PhaseBar({ phase }: { phase: Phase }) {
  // Pending -> Running -> Success / Failed
  const active = phase === 'Pending' ? 0 : phase === 'Running' ? 1 : 2;
  const last = phase === 'Failed' ? 'Failed' : 'Success';
  return (
    <>
      <Stepper activeStep={active} alternativeLabel data-testid="phase-stepper">
        <Step completed={active > 0}>
          <StepLabel>Pending</StepLabel>
        </Step>
        <Step completed={active > 1}>
          <StepLabel>Running</StepLabel>
        </Step>
        <Step completed={phase === 'Success'}>
          <StepLabel error={phase === 'Failed'}>{last}</StepLabel>
        </Step>
      </Stepper>
      {(phase === 'Pending' || phase === 'Running') && <LinearProgress sx={{ mt: 2 }} />}
    </>
  );
}

function TaskEvents({ task }: { task: ReleaseTask }) {
  const [events] = Event.useList({
    namespace: task.metadata.namespace,
    fieldSelector: `involvedObject.kind=ReleaseTask,involvedObject.name=${task.metadata.name}`,
  });
  if (events === null) return <Loader title="Loading events" />;
  const sorted = events
    .slice()
    .sort((a, b) =>
      String(a.lastOccurrence || a.metadata.creationTimestamp).localeCompare(
        String(b.lastOccurrence || b.metadata.creationTimestamp)
      )
    );
  if (sorted.length === 0) return <Typography color="text.secondary">No events yet</Typography>;
  return (
    <ol data-testid="event-timeline" style={{ margin: 0, paddingLeft: 20 }}>
      {sorted.map(e => (
        <li key={e.metadata.uid}>
          <Typography component="span" color={e.type === 'Warning' ? 'error' : 'inherit'}>
            <b>{e.reason}</b> — {e.message}
          </Typography>{' '}
          <Typography component="span" variant="caption" color="text.secondary">
            {e.lastOccurrence || e.metadata.creationTimestamp}
          </Typography>
        </li>
      ))}
    </ol>
  );
}

// 构建日志来自 deployer:构建中是构建 Pod 的实时日志,结束后是保存下来的完整日志。
// 任务没结束时每 3 秒刷新一次;结束后再取一次最终日志。
function TaskLog({ task }: { task: ReleaseTask }) {
  const runID = task.status.runID;
  const done = task.phase === 'Success' || task.phase === 'Failed';
  const [lines, setLines] = React.useState<string[]>([]);
  const [error, setError] = React.useState('');

  React.useEffect(() => {
    if (!runID) return;
    let live = true;
    const load = () =>
      getText(`/runs/${runID}/log`)
        .then(t => live && (setLines(t.split('\n')), setError('')))
        .catch((e: any) => live && setError(e?.message || String(e)));
    load();
    const t = done ? undefined : setInterval(load, 3000);
    return () => {
      live = false;
      if (t) clearInterval(t);
    };
  }, [runID, done]);

  if (!runID)
    return <Typography color="text.secondary">Waiting for the run to be submitted…</Typography>;
  return (
    <>
      {error && (
        <Alert severity="warning" sx={{ mb: 1 }}>
          Could not load the log: {error}
        </Alert>
      )}
      <div style={{ height: 360 }} data-testid="log-view">
        <LogViewer logs={lines} open onClose={() => {}} noDialog title={`run ${runID}`} />
      </div>
    </>
  );
}
