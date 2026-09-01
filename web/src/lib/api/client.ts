const API_BASE = import.meta.env.VITE_API_URL || 'http://localhost:8080';

export type APIError = {
  status: number;
  message: string;
  body?: any;
};

function getApiKeyHeader(): string | null {
  try {
    return localStorage.getItem('AGENTOS_API_KEY');
  } catch (e) {
    return null;
  }
}

export async function apiFetch<T = any>(path: string, init: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = {
    'Accept': 'application/json',
    'Content-Type': 'application/json',
    ...(init.headers as Record<string, string> || {}),
  };

  const apiKey = getApiKeyHeader();
  if (apiKey) headers['X-API-Key'] = apiKey;

  const res = await fetch(`${API_BASE}/api/v1${path}`, { ...init, headers });

  const text = await res.text();
  const content = text ? JSON.parse(text) : null;

  if (!res.ok) {
    const err: APIError = { status: res.status, message: res.statusText, body: content };
    throw err;
  }

  return content as T;
}

export { API_BASE };
