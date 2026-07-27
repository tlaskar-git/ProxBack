import { useCallback, useMemo, useState } from 'react'
import { Download, KeyRound, RefreshCw, ShieldCheck, Terminal, Trash2 } from 'lucide-react'
import { AGENT_DOWNLOADS, createEnrollToken, deleteAgent, errorMessage, listAgents } from '../api'
import type { Agent, EnrollToken } from '../api'
import { useConfirm } from '../components/Confirm'
import { useToast } from '../components/Toast'
import { CopyField } from '../components/CopyField'
import {
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorBlock,
  IconButton,
  Num,
  PageHeader,
  SectionNote,
  SkeletonRows,
  StatusPill,
  toneForStatus,
} from '../components/ui'
import { useAsync } from '../lib/useAsync'
import { formatDateTime, formatRelative } from '../lib/format'

const TOKEN_PLACEHOLDER = '<ENROLLMENT-TOKEN>'

function linuxCommand(origin: string, token: string): string {
  return [
    `curl -fsSL ${origin}${AGENT_DOWNLOADS.linux} -o /usr/local/bin/proxback-agent`,
    'chmod +x /usr/local/bin/proxback-agent',
    `/usr/local/bin/proxback-agent --server ${origin} --token ${token} --install`,
  ].join(' && ')
}

function windowsCommand(origin: string, token: string): string {
  const dir = '"$env:ProgramFiles\\ProxBack"'
  const exe = '"$env:ProgramFiles\\ProxBack\\proxback-agent.exe"'
  return [
    `New-Item -ItemType Directory -Force ${dir} | Out-Null`,
    `Invoke-WebRequest ${origin}${AGENT_DOWNLOADS.windows} -OutFile ${exe}`,
    `& ${exe} --server ${origin} --token ${token} --install`,
  ].join('; ')
}

function AgentTable({ agents, onChanged }: { agents: Agent[]; onChanged: () => void }) {
  const toast = useToast()
  const confirm = useConfirm()
  const [deleting, setDeleting] = useState<string | null>(null)

  const onDelete = async (agent: Agent) => {
    const ok = await confirm({
      title: 'Remove agent',
      message: (
        <>
          Remove <span className="font-medium text-slate-100">{agent.hostname}</span>? Its API key is
          revoked and jobs that use it stop running. Restore points already on your targets are kept.
        </>
      ),
      confirmLabel: 'Remove agent',
    })
    if (!ok) return

    setDeleting(String(agent.id))
    try {
      await deleteAgent(agent.id)
      toast.success(`Agent “${agent.hostname}” removed.`)
      onChanged()
    } catch (err) {
      toast.error('Could not remove agent', errorMessage(err))
    } finally {
      setDeleting(null)
    }
  }

  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[48rem] text-sm">
        <thead>
          <tr className="border-b border-slate-800 text-left text-micro font-semibold tracking-wide text-slate-500 uppercase">
            <th className="px-5 py-2.5 font-medium">Hostname</th>
            <th className="px-5 py-2.5 font-medium">Platform</th>
            <th className="px-5 py-2.5 font-medium">Version</th>
            <th className="px-5 py-2.5 font-medium">Status</th>
            <th className="px-5 py-2.5 font-medium">Last seen</th>
            <th className="px-5 py-2.5 font-medium">Enrolled</th>
            <th className="px-5 py-2.5" />
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-800/70">
          {agents.map((agent) => (
            <tr key={String(agent.id)} className="transition-colors duration-150 hover:bg-slate-800/30">
              <td className="px-5 py-3">
                <div className="flex items-center gap-2.5">
                  <span className="flex size-7 shrink-0 items-center justify-center rounded-lg border border-slate-800 bg-slate-950/60 text-accent-400">
                    <ShieldCheck className="size-3.5" aria-hidden />
                  </span>
                  <span className="truncate font-medium text-slate-100">{agent.hostname}</span>
                </div>
              </td>
              <td className="px-5 py-3 whitespace-nowrap text-slate-400">
                {agent.os}/{agent.arch}
              </td>
              <td className="px-5 py-3 font-mono text-xs whitespace-nowrap text-slate-400">
                {agent.version}
              </td>
              <td className="px-5 py-3">
                <StatusPill tone={toneForStatus(agent.status)} label={agent.status} />
              </td>
              <td className="px-5 py-3 whitespace-nowrap">
                <Num className="text-slate-400">{formatRelative(agent.lastSeen)}</Num>
              </td>
              <td className="px-5 py-3 whitespace-nowrap">
                <Num className="text-slate-500">{formatDateTime(agent.registeredAt)}</Num>
              </td>
              <td className="px-5 py-3 text-right">
                <IconButton
                  variant="dangerQuiet"
                  aria-label={`Remove ${agent.hostname}`}
                  title="Remove agent"
                  loading={deleting === String(agent.id)}
                  onClick={() => void onDelete(agent)}
                >
                  <Trash2 className="size-4" aria-hidden />
                </IconButton>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function DeploySection() {
  const toast = useToast()
  const [token, setToken] = useState<EnrollToken | null>(null)
  const [generating, setGenerating] = useState(false)

  const origin = useMemo(
    () => (typeof window === 'undefined' ? '' : window.location.origin),
    [],
  )
  const tokenValue = token?.token ?? TOKEN_PLACEHOLDER

  const onGenerate = async () => {
    setGenerating(true)
    try {
      const next = await createEnrollToken()
      setToken(next)
      toast.success('Enrollment token generated.', 'Single use — it expires after 24 hours.')
    } catch (err) {
      toast.error('Could not generate token', errorMessage(err))
    } finally {
      setGenerating(false)
    }
  }

  return (
    <Card>
      <CardHeader
        title="Deploy Agent"
        subtitle="Generate a single-use enrollment token, then run one command inside the guest."
        actions={
          <Button
            variant="primary"
            loading={generating}
            icon={<KeyRound className="size-4" aria-hidden />}
            onClick={() => void onGenerate()}
          >
            {token ? 'Generate new token' : 'Generate enrollment token'}
          </Button>
        }
      />

      <div className="space-y-5 px-5 py-5">
        {token ? (
          <div className="space-y-2">
            <CopyField
              label="Enrollment token"
              value={token.token}
              copyLabel="Token"
              caption={`Single use — expires ${formatDateTime(token.expiresAt)} (${formatRelative(
                token.expiresAt,
              )}).`}
            />
          </div>
        ) : (
          <SectionNote>
            Generate a token to fill the commands below. The agent exchanges it once for a permanent
            API key, then heartbeats every 15 seconds. Agents never see your storage credentials —
            all data is transferred through this server.
          </SectionNote>
        )}

        <div className="space-y-4">
          <CopyField
            label="Linux — bash (root)"
            value={linuxCommand(origin, tokenValue)}
            copyLabel="Linux install command"
            caption="Downloads the static binary, installs a systemd unit, and enrolls the agent."
          />
          <CopyField
            label="Windows — PowerShell (administrator)"
            value={windowsCommand(origin, tokenValue)}
            copyLabel="Windows install command"
            caption="Downloads the executable, registers the service, and enrolls the agent."
          />
        </div>

        <div className="flex flex-wrap items-center gap-2 border-t border-slate-800 pt-4">
          <p className="mr-auto flex items-center gap-2 text-xs text-slate-500">
            <Terminal className="size-3.5" aria-hidden />
            Prefer a manual install? Download the binary and pass{' '}
            <code className="font-mono text-slate-400">--server</code> and{' '}
            <code className="font-mono text-slate-400">--token</code> yourself.
          </p>
          <a href={AGENT_DOWNLOADS.linux} download>
            <Button size="sm" icon={<Download className="size-3.5" aria-hidden />}>
              Linux amd64
            </Button>
          </a>
          <a href={AGENT_DOWNLOADS.windows} download>
            <Button size="sm" icon={<Download className="size-3.5" aria-hidden />}>
              Windows amd64
            </Button>
          </a>
        </div>
      </div>
    </Card>
  )
}

export function AgentsPage() {
  const loader = useCallback(() => listAgents(), [])
  const { data, loading, error, reload, refresh } = useAsync(loader)
  const agents = data ?? []

  return (
    <>
      <PageHeader
        title="Agents"
        description="In-guest agents for file-level backup of Windows and Linux machines."
        actions={
          <Button
            icon={<RefreshCw className="size-4" aria-hidden />}
            onClick={() => void reload()}
            loading={loading}
          >
            Refresh
          </Button>
        }
      />

      <div className="space-y-4">
        {loading && !data ? (
          <Card>
            <SkeletonRows count={3} />
          </Card>
        ) : error && !data ? (
          <ErrorBlock message={error} onRetry={() => void reload()} />
        ) : agents.length === 0 ? (
          <EmptyState
            icon={<ShieldCheck className="size-5" aria-hidden />}
            title="No agents enrolled"
            description="Agents cover file-level backup inside a guest — useful when you need individual folders rather than a whole VM image. Generate a token below and run the one-line installer."
          />
        ) : (
          <Card>
            <CardHeader
              title="Enrolled agents"
              subtitle={`${agents.length} ${agents.length === 1 ? 'agent' : 'agents'} · online means a heartbeat within the last minute`}
            />
            <AgentTable agents={agents} onChanged={() => void refresh()} />
          </Card>
        )}

        <DeploySection />
      </div>
    </>
  )
}
