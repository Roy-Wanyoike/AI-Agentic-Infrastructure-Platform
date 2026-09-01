import { apiFetch } from './client';

export type Agent = {
  id: string;
  name: string;
  status?: string;
  version?: string;
  owner?: string;
  created_at?: string;
};

export async function listAgents(): Promise<Agent[]> {
  return apiFetch<Agent[]>('/agents');
}

export async function getAgent(id: string): Promise<Agent> {
  return apiFetch<Agent>(`/agents/${encodeURIComponent(id)}`);
}

export async function createAgent(payload: Partial<Agent>): Promise<Agent> {
  return apiFetch<Agent>('/agents', { method: 'POST', body: JSON.stringify(payload) });
}

export async function updateAgent(id: string, payload: Partial<Agent>): Promise<Agent> {
  return apiFetch<Agent>(`/agents/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify(payload) });
}

export async function deleteAgent(id: string): Promise<void> {
  return apiFetch<void>(`/agents/${encodeURIComponent(id)}`, { method: 'DELETE' });
}
