import { K8s } from '@kinvolk/headlamp-plugin/lib';
import { LogViewer } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import {
  BackLink,
  Link,
  Loader,
  NameValueTable,
  SectionBox,
} from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import Event from '@kinvolk/headlamp-plugin/lib/k8s/event';
import {
  Alert,
  LinearProgress,
  Step,
  StepLabel,
  Stepper,
  Tab,
  Tabs,
  Typography,
} from '@mui/material';
import React from 'react';
import { useParams } from 'react-router-dom';
import { PhaseChip } from './PhaseChip';
import { Phase, ReleaseTask } from './ReleaseTask';

// 与控制器编排的 Pod 容器一一对应
const STEPS = ['checkout', 'prepare', 'build', 'release'];

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
            { name: 'Service', value: `${task.spec.service.name} (${task.spec.service.kind})` },
            { name: 'Environment', value: task.spec.environment },
            { name: 'Git', value: `${task.spec.git.repo} @ ${task.spec.git.ref}` },
            ...(task.spec.gitops
              ? [
                  {
                    name: 'GitOps',
                    value: `${task.spec.gitops.repo} ${task.spec.gitops.branch || ''} ${
                      task.spec.gitops.path || ''
                    }`,
                  },
                ]
              : []),
            { name: 'Started', value: task.status.startTime || '-' },
            { name: 'Finished', value: task.status.completionTime || '-' },
            {
              name: 'Executor pod',
              value: task.status.executorPodName || '-',
              hide: !task.status.executorPodName,
            },
          ]}
        />
      </SectionBox>
      <SectionBox title="Events">
        <TaskEvents task={task} />
      </SectionBox>
      <SectionBox title="Logs">
        <TaskLogs task={task} />
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

function TaskLogs({ task }: { task: ReleaseTask }) {
  const podName = task.status.executorPodName;
  const ns = task.metadata.namespace;
  const [pod] = K8s.ResourceClasses.Pod.useGet(podName || '', ns);
  const [step, setStep] = React.useState('release');
  const [logs, setLogs] = React.useState<string[]>([]);
  const started = pod ? containerStarted(pod, step) : false;

  // 未出现 ExecutorPodName 前没有日志可看;出现后按所选步骤跟随输出
  React.useEffect(() => {
    setLogs([]);
    if (!pod || !started) return;
    const cancel = pod.getLogs(step, ({ logs }: { logs: string[] }) => setLogs(logs), {
      tailLines: 1000,
      follow: !task.phase.match(/Success|Failed/),
      showTimestamps: false,
    } as any);
    return () => cancel && cancel();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pod?.metadata.uid, step, started, task.phase]);

  if (!podName)
    return (
      <Typography color="text.secondary">
        Waiting for the controller to create the executor pod…
      </Typography>
    );
  if (!pod) return <Loader title="Loading executor pod" />;
  return (
    <>
      <Typography variant="body2" sx={{ mb: 1 }}>
        Executor pod: <Link kubeObject={pod}>{podName}</Link>
      </Typography>
      <Tabs value={step} onChange={(_, v) => setStep(v)}>
        {STEPS.map(s => (
          <Tab key={s} value={s} label={s} data-testid={`tab-${s}`} />
        ))}
      </Tabs>
      <div style={{ height: 360 }} data-testid="log-view">
        {started ? (
          <LogViewer logs={logs} open onClose={() => {}} noDialog title={step} />
        ) : (
          <Typography color="text.secondary" sx={{ p: 2 }}>
            This step has not started yet
          </Typography>
        )}
      </div>
    </>
  );
}

// init 容器和主容器的状态分开存放;只有已运行或已结束的容器才有日志
function containerStarted(pod: any, name: string): boolean {
  const statuses = [
    ...(pod.jsonData.status?.initContainerStatuses || []),
    ...(pod.jsonData.status?.containerStatuses || []),
  ];
  const cs = statuses.find((c: any) => c.name === name);
  return !!cs && !!(cs.state?.running || cs.state?.terminated);
}
