export function setApiKey(key: string) {
  try {
    localStorage.setItem('AGENTOS_API_KEY', key);
  } catch (e) {
    // ignore
  }
}

export function clearApiKey() {
  try {
    localStorage.removeItem('AGENTOS_API_KEY');
  } catch (e) {}
}

export function getApiKey(): string | null {
  try {
    return localStorage.getItem('AGENTOS_API_KEY');
  } catch (e) {
    return null;
  }
}
