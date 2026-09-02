// Honesty layer: which dashboard sections are backed by live API endpoints
// versus which still show static demo content. Views listed here render a
// "Demo data" badge so the UI never presents fake numbers as real telemetry
// (project rule: no silent mocking).

export const IS_DEMO_VIEW: ReadonlySet<string> = new Set([
  'Workflows',
  'Tools',
  'Knowledge',
  'Analytics',
  'Usage',
  'Security',
  'Infrastructure',
])

export function isDemoView(view: string): boolean {
  return IS_DEMO_VIEW.has(view)
}
