import './App.css'

const navItems = [
  'Overview',
  'Agents',
  'Runs',
  'Workflows',
  'Tools',
  'Usage',
  'Security',
  'Infrastructure',
]

const metrics = [
  { label: 'Healthy agents', value: '28', delta: '+6.2%', tone: 'positive' },
  { label: 'Runs today', value: '1,284', delta: '+18.4%', tone: 'positive' },
  { label: 'Avg latency', value: '1.8s', delta: '-12.1%', tone: 'neutral' },
  { label: 'Cost / 1K runs', value: '$14.70', delta: '-8.9%', tone: 'positive' },
]

const agentRows = [
  { name: 'Support triage', owner: 'Customer ops', status: 'Healthy', version: 'v42', runRate: '87%' },
  { name: 'Code reviewer', owner: 'Platform', status: 'Running', version: 'v19', runRate: '64%' },
  { name: 'Ops copilot', owner: 'Infra', status: 'Degraded', version: 'v08', runRate: '41%' },
  { name: 'Pricing analyst', owner: 'Finance', status: 'Healthy', version: 'v31', runRate: '92%' },
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

function App() {
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
          {navItems.map((item, index) => (
            <button key={item} className={index === 0 ? 'nav-item active' : 'nav-item'} type="button">
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
        <header className="topbar">
          <div>
            <p className="eyebrow">Command center</p>
            <h2>Operations overview</h2>
          </div>
          <div className="topbar-actions">
            <button type="button" className="ghost-button">Export</button>
            <button type="button" className="primary-button">Deploy agent</button>
          </div>
        </header>

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
                <p className="eyebrow">Fleet</p>
                <h3>Agent health</h3>
              </div>
              <button type="button" className="link-button">View all</button>
            </div>

            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Agent</th>
                    <th>Owner</th>
                    <th>Status</th>
                    <th>Version</th>
                    <th>Utilization</th>
                  </tr>
                </thead>
                <tbody>
                  {agentRows.map((agent) => (
                    <tr key={agent.name}>
                      <td>{agent.name}</td>
                      <td>{agent.owner}</td>
                      <td>
                        <span className={`status-badge ${agent.status.toLowerCase().replace(/\s+/g, '-')}`}>
                          {agent.status}
                        </span>
                      </td>
                      <td>{agent.version}</td>
                      <td>{agent.runRate}</td>
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
                    <th>Run ID</th>
                    <th>Agent</th>
                    <th>Status</th>
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
                <p className="eyebrow">SLO</p>
                <h3>Service quality</h3>
              </div>
            </div>

            <div className="quality-list">
              <div>
                <label>Success rate</label>
                <div className="meter"><span style={{ width: '96%' }} /></div>
                <strong>96.2%</strong>
              </div>
              <div>
                <label>Queue health</label>
                <div className="meter"><span style={{ width: '88%' }} /></div>
                <strong>88.4%</strong>
              </div>
              <div>
                <label>Model reliability</label>
                <div className="meter"><span style={{ width: '93%' }} /></div>
                <strong>93.1%</strong>
              </div>
            </div>
          </article>
        </section>
      </main>
    </div>
  )
}

export default App
