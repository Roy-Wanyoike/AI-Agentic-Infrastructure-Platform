import { useState } from 'react'
import './App.css'

const navItems = ['Overview', 'Agents', 'Runs', 'Workflows', 'Tools', 'Knowledge', 'Analytics', 'Usage', 'Security', 'Infrastructure', 'Settings'] as const

type ViewName = (typeof navItems)[number]

const metrics = [
  { label: 'Healthy agents', value: '28', delta: '+6.2%', tone: 'positive' },
  { label: 'Runs today', value: '1,284', delta: '+18.4%', tone: 'positive' },
  { label: 'Avg latency', value: '1.8s', delta: '-12.1%', tone: 'neutral' },
  { label: 'Cost / 1K runs', value: '$14.70', delta: '-8.9%', tone: 'positive' },
]

const runRows = [
  { id: 'RUN-4821', agent: 'Support triage', status: 'Completed', duration: '2m 18s', cost: '$0.42' },
  { id: 'RUN-4817', agent: 'Code reviewer', status: 'Running', duration: '1m 04s', cost: '$0.17' },
  { id: 'RUN-4810', agent: 'Ops copilot', status: 'Pending approval', duration: '—', cost: '$0.09' },
  { id: 'RUN-4803', agent: 'Pricing analyst', status: 'Failed', duration: '32s', cost: '$0.24' },
]

const approvalRows = [
  { item: 'Payment escalation', risk: 'High', requestedBy: 'Alice', ttl: '03m' },
  { item: 'Write to CRM', risk: 'Medium', requestedBy: 'Marcus', ttl: '11m' },
  { item: 'Customer email', risk: 'Low', requestedBy: 'Nina', ttl: '17m' },
]

const agentCards = [
  {
    name: 'Support triage',
    description: 'Customer support autopilot for ticket routing and policy-safe replies.',
    status: 'Healthy',
    model: 'gpt-4.1',
    occupancy: '78%',
    latency: '1.4s',
  },
  {
    name: 'Code reviewer',
    description: 'Repository-aware engineering assistant for review summaries and risk checks.',
    status: 'Running',
    model: 'claude-3.7',
    occupancy: '64%',
    latency: '2.1s',
  },
  {
    name: 'Ops copilot',
    description: 'Incident triage and runbook assistant for reliable operational guidance.',
    status: 'Degraded',
    model: 'gpt-4o-mini',
    occupancy: '41%',
    latency: '3.6s',
  },
]

const runTimeline = [
  { label: 'Queued', value: '184', tone: 'neutral' },
  { label: 'Running', value: '32', tone: 'running' },
  { label: 'Completed', value: '1,048', tone: 'healthy' },
  { label: 'Failed', value: '21', tone: 'warning' },
]

const workflowRows = [
  { name: 'Customer handoff', status: 'Live', owner: 'Support ops', steps: '5 steps', latency: '3.1s' },
  { name: 'Incident response', status: 'Paused', owner: 'Platform', steps: '9 steps', latency: '5.4s' },
  { name: 'Release audit', status: 'Review', owner: 'Engineering', steps: '4 steps', latency: '1.9s' },
  { name: 'Billing sync', status: 'Healthy', owner: 'Finance', steps: '6 steps', latency: '2.2s' },
]

const toolCards = [
  { name: 'Slack notifier', category: 'Communication', status: 'Healthy', latency: '180ms', permissions: 'write:messages' },
  { name: 'Postgres query', category: 'Data access', status: 'Running', latency: '590ms', permissions: 'read:analytics' },
  { name: 'Kubernetes deploy', category: 'Infrastructure', status: 'Review', latency: '1.2s', permissions: 'deploy:staging' },
  { name: 'CRM sync', category: 'Sales ops', status: 'Healthy', latency: '240ms', permissions: 'write:records' },
]

const usageRows = [
  { label: 'Agent calls', value: '1.28M', delta: '+12.6%' },
  { label: 'Inference spend', value: '$18.4K', delta: '+3.8%' },
  { label: 'Storage', value: '942 GB', delta: '+6.1%' },
  { label: 'Peak concurrency', value: '3,842', delta: '-2.2%' },
]

const environmentHealth = [
  { name: 'API', status: 'Operational' },
  { name: 'Agent runtime', status: 'Operational' },
  { name: 'Queue', status: 'Operational' },
  { name: 'Workers', status: 'Operational' },
  { name: 'AI providers', status: 'Operational' },
  { name: 'Database', status: 'Operational' },
]

const securityRows = [
  { name: 'MFA enforcement', status: 'Healthy', coverage: '96%' },
  { name: 'Secret rotation', status: 'Review', coverage: '71%' },
  { name: 'IAM drift', status: 'Running', coverage: '84%' },
  { name: 'Audit trail', status: 'Healthy', coverage: '99%' },
]

const infrastructureRows = [
  { name: 'Core API', region: 'us-east-1', replicas: '8', status: 'Healthy' },
  { name: 'Workers', region: 'eu-west-1', replicas: '6', status: 'Running' },
  { name: 'Queue broker', region: 'us-west-2', replicas: '3', status: 'Review' },
  { name: 'Storage', region: 'global', replicas: '4', status: 'Healthy' },
]

function PageHeader({ eyebrow, title, actions }: { eyebrow: string; title: string; actions?: React.ReactNode }) {
  return (
    <header className="topbar">
      <div>
        <p className="eyebrow">{eyebrow}</p>
        <h2>{title}</h2>
      </div>
      {actions ? <div className="topbar-actions">{actions}</div> : null}
    </header>
  )
}

function SummaryStat({ label, value, accent = 'default' }: { label: string; value: string; accent?: 'default' | 'info' | 'success' | 'warning' }) {
  return (
    <article className={`mini-stat accent-${accent}`}>
      <span>{label}</span>
      <strong>{value}</strong>
    </article>
  )
}

function StatusPill({ status }: { status: string }) {
  return <span className={`status-badge ${status.toLowerCase().replace(/\s+/g, '-')}`}>{status}</span>
}

function OverviewView() {
  return (
    <>
      <div className="hero-panel">
        <div>
          <p className="eyebrow">Mission control</p>
          <h2>Good morning.</h2>
          <p className="hero-copy">Monitor your AI infrastructure, execution, safety, and cost across every tenant and workflow.</p>
        </div>
        <div className="hero-actions">
          <button type="button" className="ghost-button">Run agent</button>
          <button type="button" className="primary-button">Create agent</button>
        </div>
      </div>

      <section className="kpi-grid" aria-label="Key metrics">
        {metrics.map((metric) => (
          <article key={metric.label} className="kpi-card">
            <p>{metric.label}</p>
            <div className="kpi-row">
              <strong>{metric.value}</strong>
              <span className={metric.tone === 'positive' ? 'delta positive' : 'delta neutral'}>{metric.delta}</span>
            </div>
          </article>
        ))}
      </section>

      <section className="content-grid">
        <article className="panel wide">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Execution</p>
              <h3>Agent runs</h3>
            </div>
            <button type="button" className="link-button">View analytics</button>
          </div>

          <div className="mini-chart" aria-label="Execution chart">
            {[14, 24, 18, 28, 40, 33, 46, 52, 49, 63, 58, 72].map((value, index) => (
              <span key={index} style={{ height: `${value}%` }} />
            ))}
          </div>
        </article>

        <article className="panel">
          <div className="panel-header">
            <div>
              <p className="eyebrow">System</p>
              <h3>System health</h3>
            </div>
          </div>

          <div className="health-stack">
            {environmentHealth.map((service) => (
              <div key={service.name} className="health-stack-row">
                <span>{service.name}</span>
                <span className="status-dot online" />
                <strong>{service.status}</strong>
              </div>
            ))}
          </div>
        </article>
      </section>

      <section className="bottom-grid">
        <article className="panel wide">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Activity</p>
              <h3>Recent runs</h3>
            </div>
          </div>

          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Agent</th>
                  <th>Run ID</th>
                  <th>Status</th>
                  <th>Duration</th>
                  <th>Cost</th>
                </tr>
              </thead>
              <tbody>
                {runRows.map((run) => (
                  <tr key={run.id}>
                    <td>{run.agent}</td>
                    <td>{run.id}</td>
                    <td>
                      <StatusPill status={run.status} />
                    </td>
                    <td>{run.duration}</td>
                    <td>{run.cost}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </article>

        <article className="panel">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Approvals</p>
              <h3>Action queue</h3>
            </div>
          </div>

          <div className="approval-stack">
            {approvalRows.map((action) => (
              <div key={action.item} className="approval-item">
                <div>
                  <strong>{action.item}</strong>
                  <small>{action.requestedBy}</small>
                </div>
                <div className="approval-meta">
                  <span className={`risk-badge ${action.risk.toLowerCase()}`}>{action.risk}</span>
                  <span>{action.ttl}</span>
                </div>
              </div>
            ))}
          </div>
        </article>
      </section>
    </>
  )
}

function AgentsView() {
  return (
    <>
      <PageHeader
        eyebrow="Catalog"
        title="Agents"
        actions={
          <>
            <button type="button" className="ghost-button">Filters</button>
            <button type="button" className="primary-button">New agent</button>
          </>
        }
      />

      <section className="summary-grid">
        <SummaryStat label="Total agents" value="28" accent="default" />
        <SummaryStat label="Active" value="19" accent="info" />
        <SummaryStat label="Drafts" value="5" accent="success" />
        <SummaryStat label="Needs review" value="4" accent="warning" />
      </section>

      <section className="agent-grid">
        {agentCards.map((agent) => (
          <article key={agent.name} className="agent-card">
            <div className="agent-card-header">
              <div>
                <h3>{agent.name}</h3>
                <p>{agent.description}</p>
              </div>
              <StatusPill status={agent.status} />
            </div>

            <div className="detail-grid">
              <div>
                <label>Model</label>
                <strong>{agent.model}</strong>
              </div>
              <div>
                <label>Occupancy</label>
                <strong>{agent.occupancy}</strong>
              </div>
              <div>
                <label>Latency</label>
                <strong>{agent.latency}</strong>
              </div>
            </div>

            <div className="card-actions">
              <button type="button" className="ghost-button">Inspect</button>
              <button type="button" className="primary-button small">Run</button>
            </div>
          </article>
        ))}
      </section>
    </>
  )
}

function RunsView() {
  return (
    <>
      <PageHeader
        eyebrow="Execution"
        title="Runs"
        actions={
          <>
            <button type="button" className="ghost-button">Filters</button>
            <button type="button" className="primary-button">Trigger run</button>
          </>
        }
      />

      <section className="summary-grid">
        {runTimeline.map((item) => (
          <article key={item.label} className="mini-stat">
            <span>{item.label}</span>
            <strong>{item.value}</strong>
          </article>
        ))}
      </section>

      <section className="content-grid">
        <article className="panel wide">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Live</p>
              <h3>Recent executions</h3>
            </div>
            <button type="button" className="link-button">Export log</button>
          </div>

          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Run ID</th>
                  <th>Agent</th>
                  <th>Status</th>
                  <th>Started</th>
                  <th>Duration</th>
                  <th>Cost</th>
                </tr>
              </thead>
              <tbody>
                {runRows.map((run) => (
                  <tr key={run.id}>
                    <td>{run.id}</td>
                    <td>{run.agent}</td>
                    <td>
                      <span className={`status-badge ${run.status.toLowerCase().replace(/\s+/g, '-')}`}>
                        {run.status}
                      </span>
                    </td>
                    <td>2m ago</td>
                    <td>{run.duration}</td>
                    <td>{run.cost}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </article>

        <article className="panel">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Status</p>
              <h3>Run health</h3>
            </div>
          </div>

          <div className="quality-list">
            <div>
              <label>Queue backlog</label>
              <div className="meter"><span style={{ width: '72%' }} /></div>
              <strong>72% healthy</strong>
            </div>
            <div>
              <label>Failure rate</label>
              <div className="meter"><span style={{ width: '11%' }} /></div>
              <strong>11% of runs</strong>
            </div>
            <div>
              <label>Approval hold</label>
              <div className="meter"><span style={{ width: '28%' }} /></div>
              <strong>28% waiting</strong>
            </div>
          </div>
        </article>
      </section>
    </>
  )
}

function WorkflowsView() {
  return (
    <>
      <PageHeader
        eyebrow="Automation"
        title="Workflows"
        actions={
          <>
            <button type="button" className="ghost-button">Templates</button>
            <button type="button" className="primary-button">Create workflow</button>
          </>
        }
      />

      <section className="summary-grid">
        <SummaryStat label="Live workflows" value="12" accent="default" />
        <SummaryStat label="Review queue" value="7" accent="info" />
        <SummaryStat label="Avg duration" value="2.6m" accent="success" />
        <SummaryStat label="Success" value="94%" accent="warning" />
      </section>

      <section className="workflow-grid">
        {workflowRows.map((workflow) => (
          <article key={workflow.name} className="workflow-card">
            <div className="workflow-card-header">
              <div>
                <p className="eyebrow small">Workflow</p>
                <h3>{workflow.name}</h3>
              </div>
              <StatusPill status={workflow.status} />
            </div>

            <div className="workflow-meta">
              <div>
                <label>Owner</label>
                <strong>{workflow.owner}</strong>
              </div>
              <div>
                <label>Steps</label>
                <strong>{workflow.steps}</strong>
              </div>
              <div>
                <label>Latency</label>
                <strong>{workflow.latency}</strong>
              </div>
            </div>

            <div className="card-actions">
              <button type="button" className="ghost-button">Inspect</button>
              <button type="button" className="primary-button small">Run</button>
            </div>
          </article>
        ))}
      </section>
    </>
  )
}

function ToolsView() {
  return (
    <>
      <PageHeader
        eyebrow="Tooling"
        title="Tools"
        actions={
          <>
            <button type="button" className="ghost-button">Marketplace</button>
            <button type="button" className="primary-button">Add tool</button>
          </>
        }
      />

      <section className="summary-grid">
        <SummaryStat label="Total tools" value="36" accent="default" />
        <SummaryStat label="Secure" value="31" accent="info" />
        <SummaryStat label="Needs review" value="3" accent="warning" />
        <SummaryStat label="Calls / hr" value="24.8k" accent="success" />
      </section>

      <section className="tool-grid">
        {toolCards.map((tool) => (
          <article key={tool.name} className="tool-card">
            <div className="tool-card-header">
              <div>
                <p className="eyebrow small">{tool.category}</p>
                <h3>{tool.name}</h3>
              </div>
              <StatusPill status={tool.status} />
            </div>

            <div className="tool-meta">
              <div>
                <label>Latency</label>
                <strong>{tool.latency}</strong>
              </div>
              <div>
                <label>Permission</label>
                <strong>{tool.permissions}</strong>
              </div>
            </div>

            <div className="card-actions">
              <button type="button" className="ghost-button">Permissions</button>
              <button type="button" className="primary-button small">Enable</button>
            </div>
          </article>
        ))}
      </section>
    </>
  )
}

function UsageView() {
  return (
    <>
      <PageHeader
        eyebrow="Billing"
        title="Usage"
        actions={
          <>
            <button type="button" className="ghost-button">Adjust plan</button>
            <button type="button" className="primary-button">Export report</button>
          </>
        }
      />

      <section className="summary-grid">
        {usageRows.map((item) => (
          <article key={item.label} className="mini-stat">
            <span>{item.label}</span>
            <strong>{item.value}</strong>
            <div className="delta positive" style={{ marginTop: 10 }}>{item.delta}</div>
          </article>
        ))}
      </section>

      <section className="content-grid">
        <article className="panel">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Forecast</p>
              <h3>Consumption trend</h3>
            </div>
            <button type="button" className="link-button">View detail</button>
          </div>

          <div className="usage-chart">
            {[64, 72, 54, 81, 92, 88, 96].map((value, index) => (
              <div key={index} className="usage-bar-wrap">
                <span className="usage-bar" style={{ height: `${value}%` }} />
              </div>
            ))}
          </div>
        </article>

        <article className="panel">
          <div className="panel-header">
            <div>
              <p className="eyebrow">Allocations</p>
              <h3>Quota overview</h3>
            </div>
          </div>

          <div className="quality-list">
            <div>
              <label>Inference budget</label>
              <div className="meter"><span style={{ width: '76%' }} /></div>
              <strong>76% consumed</strong>
            </div>
            <div>
              <label>Storage pool</label>
              <div className="meter"><span style={{ width: '54%' }} /></div>
              <strong>54% used</strong>
            </div>
            <div>
              <label>API requests</label>
              <div className="meter"><span style={{ width: '91%' }} /></div>
              <strong>91% of cap</strong>
            </div>
          </div>
        </article>
      </section>
    </>
  )
}

function SecurityView() {
  return (
    <>
      <PageHeader
        eyebrow="Governance"
        title="Security"
        actions={
          <>
            <button type="button" className="ghost-button">Policies</button>
            <button type="button" className="primary-button">Run audit</button>
          </>
        }
      />

      <section className="summary-grid">
        <article className="mini-stat">
          <span>Controls</span>
          <strong>41</strong>
        </article>
        <article className="mini-stat">
          <span>Protected</span>
          <strong>98%</strong>
        </article>
        <article className="mini-stat">
          <span>Incidents</span>
          <strong>2</strong>
        </article>
        <article className="mini-stat">
          <span>MTTR</span>
          <strong>14m</strong>
        </article>
      </section>

      <section className="security-grid">
        {securityRows.map((control) => (
          <article key={control.name} className="security-card">
            <div className="security-card-header">
              <h3>{control.name}</h3>
              <StatusPill status={control.status} />
            </div>
            <div className="security-row">
              <label>Coverage</label>
              <strong>{control.coverage}</strong>
            </div>
            <div className="meter"><span style={{ width: control.coverage }} /></div>
          </article>
        ))}
      </section>
    </>
  )
}

function InfrastructureView() {
  return (
    <>
      <PageHeader
        eyebrow="Operations"
        title="Infrastructure"
        actions={
          <>
            <button type="button" className="ghost-button">Scale plan</button>
            <button type="button" className="primary-button">Deploy update</button>
          </>
        }
      />

      <section className="summary-grid">
        <article className="mini-stat">
          <span>Clusters</span>
          <strong>7</strong>
        </article>
        <article className="mini-stat">
          <span>Replicas</span>
          <strong>26</strong>
        </article>
        <article className="mini-stat">
          <span>Uptime</span>
          <strong>99.97%</strong>
        </article>
        <article className="mini-stat">
          <span>Alerts</span>
          <strong>3</strong>
        </article>
      </section>

      <section className="infra-grid">
        {infrastructureRows.map((service) => (
          <article key={service.name} className="infra-card">
            <div className="infra-card-header">
              <div>
                <p className="eyebrow small">{service.region}</p>
                <h3>{service.name}</h3>
              </div>
              <StatusPill status={service.status} />
            </div>

            <div className="tool-meta">
              <div>
                <label>Replicas</label>
                <strong>{service.replicas}</strong>
              </div>
              <div>
                <label>Region</label>
                <strong>{service.region}</strong>
              </div>
            </div>
          </article>
        ))}
      </section>
    </>
  )
}

function App() {
  const [activeView, setActiveView] = useState<ViewName>('Overview')

  return (
    <div className="dashboard-shell">
      <aside className="sidebar">
        <div className="brand-block">
          <div className="brand-mark">A</div>
          <div>
            <p className="eyebrow">Platform</p>
            <h1>AgentOS</h1>
          </div>
        </div>

        <nav className="nav" aria-label="Main menu">
          {navItems.map((item) => (
            <button
              key={item}
              className={item === activeView ? 'nav-item active' : 'nav-item'}
              type="button"
              onClick={() => setActiveView(item)}
            >
              {item}
            </button>
          ))}
        </nav>

        <div className="sidebar-card">
          <p className="eyebrow">System health</p>
          <div className="health-row">
            <span className="dot green" />
            <span>All core services online</span>
          </div>
          <div className="health-row">
            <span className="dot amber" />
            <span>1 workflow awaiting approval</span>
          </div>
        </div>
      </aside>

      <main className="main-panel">
        <header className="global-header">
          <div className="searchbox">
            <span className="search-icon">⌕</span>
            <span>Search AgentOS...</span>
          </div>
          <div className="global-header-actions">
            <div className="status-chip"><span className="status-dot online" />All systems operational</div>
            <button type="button" className="ghost-button">Notifications</button>
            <button type="button" className="ghost-button">Org: Acme AI</button>
          </div>
        </header>

        {activeView === 'Overview' ? <OverviewView /> : activeView === 'Agents' ? <AgentsView /> : activeView === 'Runs' ? <RunsView /> : activeView === 'Workflows' ? <WorkflowsView /> : activeView === 'Tools' ? <ToolsView /> : activeView === 'Knowledge' ? <OverviewView /> : activeView === 'Analytics' ? <OverviewView /> : activeView === 'Usage' ? <UsageView /> : activeView === 'Security' ? <SecurityView /> : activeView === 'Infrastructure' ? <InfrastructureView /> : activeView === 'Settings' ? <OverviewView /> : <OverviewView />}
      </main>
    </div>
  )
}

export default App
