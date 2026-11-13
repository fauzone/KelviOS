/**
 * KelviOS API client — thin wrapper around fetch with auth token handling.
 * All endpoints are relative to the same origin (served by the Go backend).
 */

const API_BASE = '/api';

function getAuthHeader(): Record<string, string> {
  const token = localStorage.getItem('kelvios_token');
  if (!token) return {};
  return { Authorization: `Bearer ${token}` };
}

async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...getAuthHeader(),
    ...(options.headers as Record<string, string> || {}),
  };

  const res = await fetch(`${API_BASE}${path}`, {
    ...options,
    headers,
  });

  if (!res.ok) {
    let message = `Request failed: ${res.status}`;
    try {
      const err = await res.json();
      if (err.error) message = err.error;
    } catch {
      // ignore parse error
    }
    throw new Error(message);
  }

  if (res.status === 204) {
    return undefined as T;
  }

  return res.json();
}

// ===== Auth =====

export interface SetupRequest {
  username: string;
  password: string;
}

export interface SetupResponse {
  username: string;
}

export interface LoginRequest {
  username: string;
  password: string;
}

export interface LoginResponse {
  token: string;
  expires_at: number;
}

export interface MeResponse {
  username: string;
}

export async function apiSetup(data: SetupRequest): Promise<SetupResponse> {
  return request('/auth/setup', { method: 'POST', body: JSON.stringify(data) });
}

export async function apiLogin(data: LoginRequest): Promise<LoginResponse> {
  return request('/auth/login', { method: 'POST', body: JSON.stringify(data) });
}

export async function apiMe(): Promise<MeResponse> {
  return request('/auth/me');
}

export async function apiLogout(): Promise<void> {
  await request('/auth/logout', { method: 'POST' });
}

// ===== Setup Wizard Endpoints =====

export interface SetupStateResponse {
  has_admin: boolean;
  language_set: boolean;
  timezone_set: boolean;
  setup_complete: boolean;
  stage: 'language' | 'timezone' | 'admin' | 'preset' | 'complete';
}

export async function apiGetSetupState(): Promise<SetupStateResponse> {
  return request('/setup/state');
}

export interface SetupPreferencesRequest {
  language?: 'en' | 'id';
  timezone?: string;
}

export interface SetupPreferencesResponse {
  language?: string;
  timezone?: string;
}

export async function apiSetupPreferences(data: SetupPreferencesRequest): Promise<SetupPreferencesResponse> {
  return request('/setup/preferences', { method: 'POST', body: JSON.stringify(data) });
}

export interface SetupImportRequest {
  format: string;
  version: number;
  exported_at: number;
  settings: Record<string, any>;
}

export interface SetupImportResponse {
  imported: boolean;
}

export async function apiSetupImport(data: SetupImportRequest): Promise<SetupImportResponse> {
  return request('/setup/import', { method: 'POST', body: JSON.stringify(data) });
}

export async function apiSetupComplete(): Promise<void> {
  await request('/setup/complete', { method: 'POST' });
}

// ===== System Settings Endpoints =====

export interface SystemSettings {
  language: 'en' | 'id';
  timezone: string;
}

export async function apiGetSystemSettings(): Promise<SystemSettings> {
  return request('/settings/system');
}

export interface SystemSettingsUpdateRequest {
  language?: 'en' | 'id';
  timezone?: string;
}

export async function apiSetSystemSettings(data: SystemSettingsUpdateRequest): Promise<SystemSettings> {
  return request('/settings/system', { method: 'PUT', body: JSON.stringify(data) });
}

export interface ExportSettingsResponse {
  format: string;
  version: number;
  exported_at: number;
  settings: Record<string, any>;
}

export async function apiExportSettings(): Promise<ExportSettingsResponse> {
  return request('/settings/export');
}

// ===== Agent Modes =====

export type AgentPreset = 'agent_inside' | 'managed_outside' | 'both' | 'no_agent';

export interface AgentModes {
  resident_harness: boolean;
  external_mcp: boolean;
  preset: AgentPreset;
}

export async function apiGetAgentModes(): Promise<AgentModes> {
  return request('/agent-modes');
}

export interface AgentModesUpdateRequest {
  resident_harness: boolean;
  external_mcp: boolean;
}

export async function apiSetAgentModes(data: AgentModesUpdateRequest): Promise<AgentModes> {
  return request('/agent-modes', { method: 'PUT', body: JSON.stringify(data) });
}

// ===== Preset helpers =====

export const PRESET_LABELS: Record<AgentPreset, string> = {
  agent_inside: 'Agent inside',
  managed_outside: 'Managed from outside',
  both: 'Both',
  no_agent: 'No agent',
};

export const PRESET_DESCRIPTIONS: Record<AgentPreset, string> = {
  agent_inside: 'Resident harness ON · External MCP OFF',
  managed_outside: 'Resident harness OFF · External MCP ON',
  both: 'Resident harness ON · External MCP ON',
  no_agent: 'Resident harness OFF · External MCP OFF',
};

export function switchesForPreset(preset: AgentPreset): { resident_harness: boolean; external_mcp: boolean } {
  switch (preset) {
    case 'agent_inside':
      return { resident_harness: true, external_mcp: false };
    case 'managed_outside':
      return { resident_harness: false, external_mcp: true };
    case 'both':
      return { resident_harness: true, external_mcp: true };
    case 'no_agent':
      return { resident_harness: false, external_mcp: false };
  }
}

export function presetForSwitches(resident_harness: boolean, external_mcp: boolean): AgentPreset {
  if (resident_harness && external_mcp) return 'both';
  if (resident_harness && !external_mcp) return 'agent_inside';
  if (!resident_harness && external_mcp) return 'managed_outside';
  return 'no_agent';
}

// ===== Compose Artifacts =====

export type ComposeKind = 'report_schedule' | 'automation_rule' | 'recipe' | 'tool_chain';
export type ComposeStatus = 'pending' | 'active' | 'disabled';

export interface ComposeArtifact {
  id: string;
  name: string;
  kind: ComposeKind;
  status: ComposeStatus;
  current_version: number;
  spec: Record<string, any>;
  created_by: string;
  created_at: number;
  updated_at: number;
}

export interface ComposeVersion {
  artifact_id: string;
  version: number;
  spec: Record<string, any>;
  created_by: string;
  created_at: number;
}

export interface ComposeArtifactDetail extends ComposeArtifact {
  versions: ComposeVersion[];
}

export interface ComposeArtifactListResponse {
  artifacts: ComposeArtifact[];
}

export interface ComposeArtifactCreateRequest {
  name: string;
  kind: ComposeKind;
  spec: Record<string, any>;
}

export interface ComposeArtifactUpdateRequest {
  spec: Record<string, any>;
}

export interface ComposeArtifactRollbackRequest {
  version: number;
}

export async function apiListComposeArtifacts(status?: ComposeStatus, kind?: ComposeKind): Promise<ComposeArtifact[]> {
  const params = new URLSearchParams();
  if (status) params.set('status', status);
  if (kind) params.set('kind', kind);
  const query = params.toString() ? `?${params.toString()}` : '';
  return request<ComposeArtifactListResponse>(`/compose/artifacts${query}`).then((r) => r.artifacts);
}

export async function apiGetComposeArtifact(id: string): Promise<ComposeArtifactDetail> {
  return request(`/compose/artifacts/${id}`);
}

export async function apiCreateComposeArtifact(data: ComposeArtifactCreateRequest): Promise<ComposeArtifact> {
  return request('/compose/artifacts', { method: 'POST', body: JSON.stringify(data) });
}

export async function apiUpdateComposeArtifact(id: string, spec: Record<string, any>): Promise<ComposeArtifact> {
  return request(`/compose/artifacts/${id}`, { method: 'PUT', body: JSON.stringify({ spec }) });
}

export async function apiActivateComposeArtifact(id: string): Promise<ComposeArtifact> {
  return request(`/compose/artifacts/${id}/activate`, { method: 'POST' });
}

export async function apiDisableComposeArtifact(id: string): Promise<ComposeArtifact> {
  return request(`/compose/artifacts/${id}/disable`, { method: 'POST' });
}

export async function apiRollbackComposeArtifact(id: string, version: number): Promise<ComposeArtifact> {
  return request(`/compose/artifacts/${id}/rollback`, { method: 'POST', body: JSON.stringify({ version }) });
}

export async function apiDeleteComposeArtifact(id: string): Promise<void> {
  return request(`/compose/artifacts/${id}`, { method: 'DELETE' });
}

export const COMPOSE_KIND_LABELS: Record<ComposeKind, string> = {
  report_schedule: 'Report schedule',
  automation_rule: 'Automation rule',
  recipe: 'Recipe',
  tool_chain: 'Tool chain',
};

export const COMPOSE_STATUS_LABELS: Record<ComposeStatus, string> = {
  pending: 'Pending',
  active: 'Active',
  disabled: 'Disabled',
};

// ===== Activity & Automation =====

export type ActivityStatus =
  | 'running'
  | 'scheduled'
  | 'paused'
  | 'failed'
  | 'idle'
  | 'manual'
  | 'unavailable';

export type ActivitySource =
  | 'compose'
  | 'restore_test'
  | 'maintenance'
  | 'power'
  | 'agent';

export type ActivityAction = 'pause' | 'resume' | 'cancel' | 'run-now';

export interface ActivityRun {
  result: string;
  started_at: number;
  duration_seconds: number;
  summary?: string;
  failure_reason?: string;
  recovery_hint?: string;
}

export interface ActivityItem {
  id: string;
  source: ActivitySource;
  kind: string;
  name: string;
  status: ActivityStatus;
  next_run_at?: number;
  last_run?: ActivityRun;
  recent_runs?: ActivityRun[];
  controls: ActivityAction[];
  source_error?: string;
}

export interface ActivityListSummary {
  total: number;
  by_status: Record<string, number>;
  failed: string[];
}

export interface ActivityListResponse {
  items: ActivityItem[];
  summary: ActivityListSummary;
}

export async function apiListActivity(
  source?: ActivitySource,
  status?: ActivityStatus
): Promise<ActivityListResponse> {
  const params = new URLSearchParams();
  if (source) params.set('source', source);
  if (status) params.set('status', status);
  const query = params.toString() ? `?${params.toString()}` : '';
  return request<ActivityListResponse>(`/activity${query}`);
}

export async function apiGetActivityItem(id: string): Promise<{ item: ActivityItem }> {
  return request(`/activity/${encodeURIComponent(id)}`);
}

export async function apiActivityAction(
  id: string,
  action: ActivityAction
): Promise<{ item: ActivityItem }> {
  return request(`/activity/${encodeURIComponent(id)}/${action}`, { method: 'POST' });
}

// Human-readable labels for source

export const ACTIVITY_SOURCE_LABELS: Record<ActivitySource, string> = {
  compose: 'Automation',
  restore_test: 'Restore test',
  maintenance: 'Maintenance',
  power: 'Power schedule',
  agent: 'Agent',
};

// Status order for summary chips (failed first, per spec)
export const ACTIVITY_STATUS_ORDER: ActivityStatus[] = [
  'failed',
  'running',
  'scheduled',
  'paused',
  'idle',
  'manual',
  'unavailable',
];

// ===== Rules Engine =====

export interface ParamInfo {
  name: string;
  type: 'string' | 'number' | 'boolean' | 'select';
  required: boolean;
  default?: any;
  description?: string;
  options?: string[]; // for select type
  min?: number;
  max?: number;
}

export interface EventTypeInfo {
  type: string;
  label: string;
  description: string;
  params: ParamInfo[];
}

export interface ActionTypeInfo {
  type: string;
  label: string;
  description: string;
  params: ParamInfo[];
}

export interface RuleCatalog {
  events: EventTypeInfo[];
  actions: ActionTypeInfo[];
}

export interface RuleAction {
  type: string;
  params: Record<string, any>;
}

export interface Rule {
  id: string;
  name: string;
  event_type: string;
  event_params: Record<string, any>;
  actions: RuleAction[];
  enabled: boolean;
  cooldown_seconds: number;
  max_fires_per_day: number;
  state: 'active' | 'cooldown' | 'disabled' | 'exhausted';
  last_fired_at: number | null;
  created_at: number;
  updated_at: number;
}

export interface RuleCreateRequest {
  name: string;
  event_type: string;
  event_params: Record<string, any>;
  actions: RuleAction[];
  enabled?: boolean;
  cooldown_seconds?: number;
  max_fires_per_day?: number;
}

export interface RuleUpdateRequest {
  name?: string;
  event_type?: string;
  event_params?: Record<string, any>;
  actions?: RuleAction[];
  enabled?: boolean;
  cooldown_seconds?: number;
  max_fires_per_day?: number;
}

export interface Run {
  id: string;
  rule_id: string;
  result: 'ok' | 'failed' | 'skipped';
  detail?: string;
  alert_id?: string;
  test_fire: boolean;
  fired_at: number;
}

export interface ListRunsResponse {
  runs: Run[];
  total: number;
  limit: number;
  offset: number;
}

export interface TestFireRequest {
  event_ref?: string;
  event_data?: Record<string, any>;
}

export async function apiGetRulesCatalog(): Promise<RuleCatalog> {
  return request('/rules/catalog');
}

export async function apiListRules(): Promise<Rule[]> {
  return request('/rules');
}

export async function apiCreateRule(data: RuleCreateRequest): Promise<Rule> {
  return request('/rules', { method: 'POST', body: JSON.stringify(data) });
}

export async function apiGetRule(id: string): Promise<Rule> {
  return request(`/rules/${encodeURIComponent(id)}`);
}

export async function apiUpdateRule(id: string, data: RuleUpdateRequest): Promise<Rule> {
  return request(`/rules/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify(data) });
}

export async function apiDeleteRule(id: string): Promise<{ status: string }> {
  return request(`/rules/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

export async function apiTestFireRule(id: string, data: TestFireRequest): Promise<Run> {
  return request(`/rules/${encodeURIComponent(id)}/test-fire`, { method: 'POST', body: JSON.stringify(data) });
}

export async function apiListRuleRuns(id: string, limit = 50, offset = 0): Promise<ListRunsResponse> {
  const params = new URLSearchParams();
  params.set('limit', String(limit));
  params.set('offset', String(offset));
  return request(`/rules/${encodeURIComponent(id)}/runs?${params.toString()}`);
}

// Human-readable labels for rule state
export const RULE_STATE_LABELS: Record<Rule['state'], string> = {
  active: 'Active',
  cooldown: 'Cooldown',
  disabled: 'Disabled',
  exhausted: 'Exhausted',
};

export const RULE_STATE_VARIANTS: Record<Rule['state'], 'success' | 'warning' | 'danger' | 'info' | 'neutral'> = {
  active: 'success',
  cooldown: 'warning',
  disabled: 'neutral',
  exhausted: 'danger',
};
