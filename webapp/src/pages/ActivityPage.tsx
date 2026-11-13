import { useState, useEffect, type ReactNode, useCallback, useRef } from 'react';
import {
  apiListActivity,
  apiGetActivityItem,
  apiActivityAction,
  type ActivityItem,
  type ActivityStatus,
  type ActivitySource,
  type ActivityAction,
  type ActivityListResponse,
  ACTIVITY_SOURCE_LABELS,
  ACTIVITY_STATUS_ORDER,
  apiGetRulesCatalog,
  apiListRules,
  apiCreateRule,
  apiGetRule,
  apiUpdateRule,
  apiDeleteRule,
  apiTestFireRule,
  apiListRuleRuns,
  type Rule,
  type RuleCreateRequest,
  type RuleCatalog,
  type ParamInfo,
  type Run,
  type TestFireRequest,
  RULE_STATE_LABELS,
  RULE_STATE_VARIANTS,
} from '../api';

function Section({ title, children, className = '' }: { title: string; children: ReactNode; className?: string }) {
  return (
    <section className={`settings-section ${className}`}>
      <h2 className="settings-section-title">{title}</h2>
      {children}
    </section>
  );
}

function Badge({ children, variant = 'neutral' }: { children: ReactNode; variant?: 'success' | 'warning' | 'danger' | 'info' | 'neutral' }) {
  return (
    <span className={`badge badge--${variant}`}>{children}</span>
  );
}

function Button({
  children,
  onClick,
  variant = 'secondary',
  size = 'md',
  disabled,
  className = '',
  type = 'button',
}: {
  children: ReactNode;
  onClick?: (e: React.MouseEvent<HTMLButtonElement>) => void;
  variant?: 'primary' | 'secondary' | 'ghost' | 'danger';
  size?: 'sm' | 'md';
  disabled?: boolean;
  className?: string;
  type?: 'button' | 'submit';
}) {
  return (
    <button
      type={type}
      className={`button button--${variant} button--${size} ${className}`}
      onClick={onClick}
      disabled={disabled}
    >
      {children}
    </button>
  );
}

function formatDuration(seconds: number): string {
  if (seconds < 60) return `${seconds} s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  const remainingMinutes = minutes % 60;
  return remainingMinutes > 0 ? `${hours} h ${remainingMinutes} min` : `${hours} h`;
}

function getStatusDotClass(status: ActivityStatus): string {
  switch (status) {
    case 'running':
      return 'dot--accent';
    case 'failed':
      return 'dot--danger';
    case 'paused':
      return 'dot--warning';
    case 'scheduled':
    case 'manual':
      return 'dot--neutral';
    case 'idle':
      return 'dot--ok';
    case 'unavailable':
      return 'dot--danger';
    default:
      return 'dot--neutral';
  }
}

function getStatusBadgeVariant(status: ActivityStatus): 'success' | 'warning' | 'danger' | 'info' | 'neutral' {
  switch (status) {
    case 'running':
      return 'info';
    case 'failed':
      return 'danger';
    case 'paused':
      return 'warning';
    case 'scheduled':
    case 'manual':
      return 'neutral';
    case 'idle':
      return 'success';
    case 'unavailable':
      return 'danger';
    default:
      return 'neutral';
  }
}

function getStatusLabel(status: ActivityStatus): string {
  return status.charAt(0).toUpperCase() + status.slice(1);
}

const SOURCES: ActivitySource[] = ['compose', 'restore_test', 'maintenance', 'power', 'agent'];
const STATUSES: ActivityStatus[] = ['running', 'scheduled', 'paused', 'failed', 'idle', 'manual', 'unavailable'];

const FILTER_STATUSES: (ActivityStatus | 'all')[] = ['all', ...STATUSES];
const FILTER_SOURCES: (ActivitySource | 'all')[] = ['all', ...SOURCES];

// ===== Rules Engine UI Helpers =====

function Input({
  label,
  value,
  onChange,
  type = 'text',
  required,
  disabled,
  error,
  help,
  className = '',
}: {
  label: string;
  value: string | number;
  onChange: (v: string | number) => void;
  type?: 'text' | 'number' | 'email' | 'password';
  required?: boolean;
  disabled?: boolean;
  error?: string;
  help?: string;
  className?: string;
}) {
  return (
    <div className={`form-field ${className}`}>
      <label className="form-label" style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 'var(--text-sm)', fontWeight: 500 }}>
        {label} {required && <span style={{ color: 'var(--danger)' }}>*</span>}
      </label>
      <input
        type={type}
        value={value}
        onChange={(e) => onChange(type === 'number' ? Number(e.target.value) : e.target.value)}
        disabled={disabled}
        className="form-input"
        style={{ width: '100%' }}
        aria-invalid={error ? 'true' : 'false'}
        aria-describedby={error ? `${label}-error` : help ? `${label}-help` : undefined}
      />
      {error && <p id={`${label}-error`} className="form-error" style={{ margin: 'var(--space-1) 0 0', fontSize: 'var(--text-xs)', color: 'var(--danger)' }}>{error}</p>}
      {help && !error && <p id={`${label}-help`} className="form-help" style={{ margin: 'var(--space-1) 0 0', fontSize: 'var(--text-xs)', color: 'var(--text-faint)' }}>{help}</p>}
    </div>
  );
}

function Select({
  label,
  value,
  onChange,
  options,
  required,
  disabled,
  error,
  help,
  className = '',
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  options: { value: string; label: string }[];
  required?: boolean;
  disabled?: boolean;
  error?: string;
  help?: string;
  className?: string;
}) {
  return (
    <div className={`form-field ${className}`}>
      <label className="form-label" style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 'var(--text-sm)', fontWeight: 500 }}>
        {label} {required && <span style={{ color: 'var(--danger)' }}>*</span>}
      </label>
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={disabled}
        className="form-select"
        style={{ width: '100%' }}
        aria-invalid={error ? 'true' : 'false'}
        aria-describedby={error ? `${label}-error` : help ? `${label}-help` : undefined}
      >
        <option value="">Select…</option>
        {options.map((opt) => (
          <option key={opt.value} value={opt.value}>{opt.label}</option>
        ))}
      </select>
      {error && <p id={`${label}-error`} className="form-error" style={{ margin: 'var(--space-1) 0 0', fontSize: 'var(--text-xs)', color: 'var(--danger)' }}>{error}</p>}
      {help && !error && <p id={`${label}-help`} className="form-help" style={{ margin: 'var(--space-1) 0 0', fontSize: 'var(--text-xs)', color: 'var(--text-faint)' }}>{help}</p>}
    </div>
  );
}

function Checkbox({
  label,
  checked,
  onChange,
  disabled,
  help,
}: {
  label: string;
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  help?: string;
}) {
  return (
    <label className="form-checkbox" style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)', cursor: disabled ? 'not-allowed' : 'pointer' }}>
      <input
        type="checkbox"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
        disabled={disabled}
        className="form-checkbox-input"
      />
      <span style={{ fontSize: 'var(--text-sm)', color: disabled ? 'var(--text-faint)' : 'var(--text)' }}>{label}</span>
      {help && <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-faint)' }}>{help}</span>}
    </label>
  );
}

function Textarea({
  label,
  value,
  onChange,
  required,
  disabled,
  error,
  help,
  rows = 4,
  className = '',
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  required?: boolean;
  disabled?: boolean;
  error?: string;
  help?: string;
  rows?: number;
  className?: string;
}) {
  return (
    <div className={`form-field ${className}`}>
      <label className="form-label" style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 'var(--text-sm)', fontWeight: 500 }}>
        {label} {required && <span style={{ color: 'var(--danger)' }}>*</span>}
      </label>
      <textarea
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={disabled}
        className="form-textarea"
        style={{ width: '100%', fontFamily: 'ui-monospace, Menlo, Consolas, monospace', fontSize: 'var(--text-xs)' }}
        rows={rows}
        aria-invalid={error ? 'true' : 'false'}
        aria-describedby={error ? `${label}-error` : help ? `${label}-help` : undefined}
      />
      {error && <p id={`${label}-error`} className="form-error" style={{ margin: 'var(--space-1) 0 0', fontSize: 'var(--text-xs)', color: 'var(--danger)' }}>{error}</p>}
      {help && !error && <p id={`${label}-help`} className="form-help" style={{ margin: 'var(--space-1) 0 0', fontSize: 'var(--text-xs)', color: 'var(--text-faint)' }}>{help}</p>}
    </div>
  );
}

function Modal({
  isOpen,
  onClose,
  title,
  children,
  size = 'md',
}: {
  isOpen: boolean;
  onClose: () => void;
  title: string;
  children: ReactNode;
  size?: 'sm' | 'md' | 'lg' | 'xl';
}) {
  if (!isOpen) return null;
  const sizeClasses = { sm: '400px', md: '600px', lg: '800px', xl: '1000px' };
  return (
    <div className="modal-overlay" onClick={onClose} style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.5)', display: 'flex', alignItems: 'center', justifyContent: 'center', zIndex: 1000, padding: 'var(--space-4)' }}>
      <div className="modal" onClick={(e) => e.stopPropagation()} style={{ background: 'var(--surface)', borderRadius: 'var(--radius-lg)', boxShadow: 'var(--shadow-xl)', width: '100%', maxWidth: sizeClasses[size], maxHeight: '90vh', overflow: 'auto', display: 'flex', flexDirection: 'column' }}>
        <div className="modal-header" style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: 'var(--space-4) var(--space-6)', borderBottom: '1px solid var(--border)' }}>
          <h3 style={{ margin: 0, fontSize: 'var(--text-lg)', fontWeight: 600 }}>{title}</h3>
          <Button variant="ghost" onClick={onClose} aria-label="Close modal">✕</Button>
        </div>
        <div className="modal-body" style={{ padding: 'var(--space-6)', overflow: 'auto' }}>{children}</div>
      </div>
    </div>
  );
}

function formatTimestamp(ts: number | null): string {
  if (!ts) return '—';
  const date = new Date(ts * 1000);
  return date.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
}

function formatCooldown(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const remainingMinutes = minutes % 60;
  return remainingMinutes > 0 ? `${hours}h ${remainingMinutes}m` : `${hours}h`;
}

function getEventLabel(catalog: RuleCatalog, eventType: string): string {
  const event = catalog.events.find((e) => e.type === eventType);
  return event?.label || eventType;
}

function getActionLabels(catalog: RuleCatalog, actions: Rule['actions']): string {
  return actions
    .map((a) => {
      const actionInfo = catalog.actions.find((act) => act.type === a.type);
      return actionInfo?.label || a.type;
    })
    .join(', ');
}

function renderParamInput(
  param: ParamInfo,
  value: any,
  onChange: (v: any) => void,
  disabled: boolean,
  allAgentNames: string[]
): ReactNode {
  const isCommonDelay = param.name === 'delay_seconds';

  switch (param.type) {
    case 'boolean':
      return (
        <Checkbox
          label={param.description || param.name}
          checked={Boolean(value)}
          onChange={onChange}
          disabled={disabled}
        />
      );
    case 'select':
      return (
        <Select
          label={param.description || param.name}
          value={value || ''}
          onChange={onChange}
          options={param.options?.map((opt) => ({ value: opt, label: opt })) || []}
          required={param.required}
          disabled={disabled}
          help={param.description}
        />
      );
    case 'number':
      return (
        <Input
          label={param.description || param.name}
          type="number"
          value={value ?? param.default ?? ''}
          onChange={onChange}
          required={param.required}
          disabled={disabled}
          help={param.description}
          className={isCommonDelay ? 'delay-input' : ''}
        />
      );
    default:
      // For agent selection, use a select with agent names
      if (param.name === 'agent' && allAgentNames.length > 0) {
        return (
          <Select
            label={param.description || param.name}
            value={value || ''}
            onChange={onChange}
            options={allAgentNames.map((name) => ({ value: name, label: name }))}
            required={param.required}
            disabled={disabled}
            help={param.description}
          />
        );
      }
      return (
        <Input
          label={param.description || param.name}
          type="text"
          value={value ?? param.default ?? ''}
          onChange={onChange}
          required={param.required}
          disabled={disabled}
          help={param.description}
        />
      );
  }
}

export function ActivityPage() {
  const [items, setItems] = useState<ActivityItem[]>([]);
  const [summary, setSummary] = useState<ActivityListResponse['summary'] | null>(null);
  const [loading, setLoading] = useState(true);
  const [filterSource, setFilterSource] = useState<ActivitySource | 'all'>('all');
  const [filterStatus, setFilterStatus] = useState<ActivityStatus | 'all'>('all');
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [detail, setDetail] = useState<ActivityItem | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [actionLoadingId, setActionLoadingId] = useState<string | null>(null);
  const [actionError, setActionError] = useState<{ id: string; message: string; code: string } | null>(null);

  // Rules state
  const [rules, setRules] = useState<Rule[]>([]);
  const [rulesLoading, setRulesLoading] = useState(true);
  const [catalog, setCatalog] = useState<RuleCatalog | null>(null);
  const [catalogLoading, setCatalogLoading] = useState(true);
  const [filterEnabled, setFilterEnabled] = useState<'all' | 'enabled' | 'disabled'>('all');
  const [ruleError, setRuleError] = useState<string | null>(null);
  const [ruleSuccess, setRuleSuccess] = useState<string | null>(null);
  const [ruleActionLoadingId, setRuleActionLoadingId] = useState<string | null>(null);
  const [ruleActionError, setRuleActionError] = useState<{ id: string; message: string } | null>(null);
  const [expandedRuleId, setExpandedRuleId] = useState<string | null>(null);
  const [ruleDetail, setRuleDetail] = useState<Rule | null>(null);
  const [ruleDetailLoading, setRuleDetailLoading] = useState(false);

  // Modals
  const [createRuleOpen, setCreateRuleOpen] = useState(false);
  const [editRuleId, setEditRuleId] = useState<string | null>(null);
  const [testFireRuleId, setTestFireRuleId] = useState<string | null>(null);
  const [viewRunsRuleId, setViewRunsRuleId] = useState<string | null>(null);
  const [deleteRuleId, setDeleteRuleId] = useState<string | null>(null);

  // Form state
  const [formName, setFormName] = useState('');
  const [formEventType, setFormEventType] = useState('');
  const [formEventParams, setFormEventParams] = useState<Record<string, any>>({});
  const [formActions, setFormActions] = useState<Array<{ type: string; params: Record<string, any> }>>([{ type: '', params: {} }]);
  const [formEnabled, setFormEnabled] = useState(true);
  const [formCooldown, setFormCooldown] = useState(3600);
  const [formMaxFires, setFormMaxFires] = useState(10);
  const [formErrors, setFormErrors] = useState<Record<string, string>>({});

  // Test fire form
  const [testFireEventRef, setTestFireEventRef] = useState('');
  const [testFireEventData, setTestFireEventData] = useState('{}');
  const [testFireError, setTestFireError] = useState<string | null>(null);
  const [testFireResult, setTestFireResult] = useState<Run | null>(null);

  // Runs pagination
  const [runsPage, setRunsPage] = useState(0);
  const [runs, setRuns] = useState<Run[]>([]);
  const [runsTotal, setRunsTotal] = useState(0);
  const [runsLoading, setRunsLoading] = useState(false);
  const RUNS_PAGE_SIZE = 20;

  const pollIntervalRef = useRef<number | null>(null);
  const isActionPendingRef = useRef(false);

  const loadItems = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const source = filterSource === 'all' ? undefined : filterSource;
      const status = filterStatus === 'all' ? undefined : filterStatus;
      const data = await apiListActivity(source, status);
      setItems(data.items);
      setSummary(data.summary);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load activity');
    } finally {
      setLoading(false);
    }
  }, [filterSource, filterStatus]);

  const loadCatalog = useCallback(async () => {
    setCatalogLoading(true);
    try {
      const data = await apiGetRulesCatalog();
      setCatalog(data);
    } catch (err) {
      setRuleError(err instanceof Error ? err.message : 'Failed to load rules catalog');
    } finally {
      setCatalogLoading(false);
    }
  }, []);

  const loadRules = useCallback(async () => {
    setRulesLoading(true);
    setRuleError(null);
    try {
      const data = await apiListRules();
      setRules(data);
    } catch (err) {
      setRuleError(err instanceof Error ? err.message : 'Failed to load rules');
    } finally {
      setRulesLoading(false);
    }
  }, []);

  useEffect(() => {
    loadItems();
    loadCatalog();
    loadRules();
  }, [loadItems, loadCatalog, loadRules]);

  // Form handlers
  const resetForm = useCallback(() => {
    setFormName('');
    setFormEventType('');
    setFormEventParams({});
    setFormActions([{ type: '', params: {} }]);
    setFormEnabled(true);
    setFormCooldown(3600);
    setFormMaxFires(10);
    setFormErrors({});
  }, []);

  const openCreateRule = useCallback(() => {
    resetForm();
    setCreateRuleOpen(true);
  }, [resetForm]);

  const openEditRule = useCallback(async (id: string) => {
    setRuleDetailLoading(true);
    try {
      const rule = await apiGetRule(id);
      setFormName(rule.name);
      setFormEventType(rule.event_type);
      setFormEventParams(rule.event_params || {});
      setFormActions(rule.actions.map((a) => ({ type: a.type, params: a.params || {} })));
      setFormEnabled(rule.enabled);
      setFormCooldown(rule.cooldown_seconds);
      setFormMaxFires(rule.max_fires_per_day);
      setFormErrors({});
      setEditRuleId(id);
    } catch (err) {
      setRuleError(err instanceof Error ? err.message : 'Failed to load rule');
    } finally {
      setRuleDetailLoading(false);
    }
  }, []);

  const handleEventTypeChange = useCallback((eventType: string) => {
    setFormEventType(eventType);
    setFormEventParams({});
    // Set defaults from catalog
    if (catalog) {
      const eventInfo = catalog.events.find((e) => e.type === eventType);
      if (eventInfo) {
        const defaults: Record<string, any> = {};
        eventInfo.params.forEach((p) => {
          if (p.default !== undefined) defaults[p.name] = p.default;
        });
        setFormEventParams(defaults);
      }
    }
  }, [catalog]);

  const handleEventParamChange = useCallback((paramName: string, value: any) => {
    setFormEventParams((prev) => ({ ...prev, [paramName]: value }));
  }, []);

  const handleActionTypeChange = useCallback((index: number, actionType: string) => {
    setFormActions((prev) => {
      const next = [...prev];
      const defaults: Record<string, any> = {};
      if (catalog) {
        const actionInfo = catalog.actions.find((a) => a.type === actionType);
        if (actionInfo) {
          actionInfo.params.forEach((p) => {
            if (p.default !== undefined) defaults[p.name] = p.default;
          });
        }
      }
      // Always add delay_seconds default
      defaults.delay_seconds = 0;
      next[index] = { type: actionType, params: defaults };
      return next;
    });
  }, [catalog]);

  const handleActionParamChange = useCallback((index: number, paramName: string, value: any) => {
    setFormActions((prev) => {
      const next = [...prev];
      next[index] = { ...next[index], params: { ...next[index].params, [paramName]: value } };
      return next;
    });
  }, []);

  const addAction = useCallback(() => {
    if (formActions.length < 3) {
      setFormActions((prev) => [...prev, { type: '', params: {} }]);
    }
  }, [formActions.length]);

  const removeAction = useCallback((index: number) => {
    if (formActions.length > 1) {
      setFormActions((prev) => prev.filter((_, i) => i !== index));
    }
  }, [formActions.length]);

  const validateForm = useCallback((): boolean => {
    const errors: Record<string, string> = {};
    if (!formName.trim()) errors.name = 'Name is required';
    if (!formEventType) errors.eventType = 'Event type is required';
    if (formActions.length === 0) errors.actions = 'At least one action is required';
    formActions.forEach((action, idx) => {
      if (!action.type) errors[`action${idx}Type`] = 'Action type is required';
      // Validate required params
      if (catalog) {
        const actionInfo = catalog.actions.find((a) => a.type === action.type);
        if (actionInfo) {
          actionInfo.params.forEach((p) => {
            if (p.required && (action.params[p.name] === undefined || action.params[p.name] === '')) {
              errors[`action${idx}${p.name}`] = `${p.description || p.name} is required`;
            }
          });
        }
      }
      // Validate event required params
      if (catalog) {
        const eventInfo = catalog.events.find((e) => e.type === formEventType);
        if (eventInfo) {
          eventInfo.params.forEach((p) => {
            if (p.required && (formEventParams[p.name] === undefined || formEventParams[p.name] === '')) {
              errors[`event${p.name}`] = `${p.description || p.name} is required`;
            }
          });
        }
      }
    });
    setFormErrors(errors);
    return Object.keys(errors).length === 0;
  }, [formName, formEventType, formActions, formEventParams, catalog]);

  const handleSubmitRule = useCallback(async () => {
    if (!validateForm()) return;

    const ruleData: RuleCreateRequest = {
      name: formName.trim(),
      event_type: formEventType,
      event_params: formEventParams,
      actions: formActions.map((a) => ({ type: a.type, params: a.params })),
      enabled: formEnabled,
      cooldown_seconds: formCooldown,
      max_fires_per_day: formMaxFires,
    };

    setRuleActionLoadingId(editRuleId || 'create');
    setRuleError(null);
    try {
      if (editRuleId) {
        await apiUpdateRule(editRuleId, ruleData);
        setRuleSuccess('Rule updated');
      } else {
        await apiCreateRule(ruleData);
        setRuleSuccess('Rule created');
      }
      setCreateRuleOpen(false);
      setEditRuleId(null);
      resetForm();
      await loadRules();
      setTimeout(() => setRuleSuccess(null), 3000);
    } catch (err) {
      setRuleError(err instanceof Error ? err.message : editRuleId ? 'Failed to update rule' : 'Failed to create rule');
    } finally {
      setRuleActionLoadingId(null);
    }
  }, [validateForm, formName, formEventType, formEventParams, formActions, formEnabled, formCooldown, formMaxFires, editRuleId, loadRules, resetForm]);

  const handleDeleteRule = useCallback(async () => {
    if (!deleteRuleId) return;
    setRuleActionLoadingId(deleteRuleId);
    setRuleError(null);
    try {
      await apiDeleteRule(deleteRuleId);
      setRuleSuccess('Rule deleted');
      setDeleteRuleId(null);
      await loadRules();
      setTimeout(() => setRuleSuccess(null), 3000);
    } catch (err) {
      setRuleError(err instanceof Error ? err.message : 'Failed to delete rule');
    } finally {
      setRuleActionLoadingId(null);
    }
  }, [deleteRuleId, loadRules]);

  const handleTestFire = useCallback(async () => {
    if (!testFireRuleId) return;
    setRuleActionLoadingId(testFireRuleId);
    setTestFireError(null);
    setTestFireResult(null);
    try {
      let eventData: Record<string, any> | undefined;
      try {
        eventData = testFireEventData ? JSON.parse(testFireEventData) : undefined;
      } catch {
        setTestFireError('Invalid JSON in event data');
        return;
      }
      const data: TestFireRequest = {
        event_ref: testFireEventRef || undefined,
        event_data: eventData,
      };
      const result = await apiTestFireRule(testFireRuleId, data);
      setTestFireResult(result);
      setRuleSuccess('Test fire completed');
      setTimeout(() => setRuleSuccess(null), 3000);
    } catch (err) {
      setTestFireError(err instanceof Error ? err.message : 'Test fire failed');
    } finally {
      setRuleActionLoadingId(null);
    }
  }, [testFireRuleId, testFireEventRef, testFireEventData]);

  const loadRuns = useCallback(async (page = 0) => {
    if (!viewRunsRuleId) return;
    setRunsLoading(true);
    try {
      const data = await apiListRuleRuns(viewRunsRuleId, RUNS_PAGE_SIZE, page * RUNS_PAGE_SIZE);
      setRuns(data.runs);
      setRunsTotal(data.total);
      setRunsPage(page);
    } catch (err) {
      setRuleError(err instanceof Error ? err.message : 'Failed to load runs');
    } finally {
      setRunsLoading(false);
    }
  }, [viewRunsRuleId]);

  const openViewRuns = useCallback((id: string) => {
    setViewRunsRuleId(id);
    setRunsPage(0);
    loadRuns(0);
  }, [loadRuns]);

  const openTestFire = useCallback((id: string) => {
    setTestFireRuleId(id);
    setTestFireEventRef('');
    setTestFireEventData('{}');
    setTestFireError(null);
    setTestFireResult(null);
  }, []);

  const openDeleteConfirm = useCallback((id: string) => {
    setDeleteRuleId(id);
  }, []);

  const filteredRules = rules.filter((rule) => {
    if (filterEnabled === 'enabled') return rule.enabled;
    if (filterEnabled === 'disabled') return !rule.enabled;
    return true;
  });

  const anyLoading = detailLoading || actionLoadingId !== null;
  const anyRuleLoading = ruleDetailLoading || ruleActionLoadingId !== null || runsLoading;

  // Polling: 15 seconds, stops when tab hidden or action pending
  useEffect(() => {
    const handleVisibilityChange = () => {
      if (document.hidden) {
        if (pollIntervalRef.current) {
          window.clearInterval(pollIntervalRef.current);
          pollIntervalRef.current = null;
        }
      } else if (!isActionPendingRef.current && !pollIntervalRef.current) {
        pollIntervalRef.current = window.setInterval(() => {
          loadItems();
        }, 15000);
      }
    };

    // Start polling
    pollIntervalRef.current = window.setInterval(() => {
      if (!document.hidden && !isActionPendingRef.current) {
        loadItems();
      }
    }, 15000);

    document.addEventListener('visibilitychange', handleVisibilityChange);

    return () => {
      if (pollIntervalRef.current) {
        window.clearInterval(pollIntervalRef.current);
        pollIntervalRef.current = null;
      }
      document.removeEventListener('visibilitychange', handleVisibilityChange);
    };
  }, [loadItems]);

  const handleExpand = async (id: string) => {
    if (expandedId === id) {
      setExpandedId(null);
      setDetail(null);
      return;
    }
    setDetailLoading(true);
    setActionError(null);
    try {
      const data = await apiGetActivityItem(id);
      setDetail(data.item);
      setExpandedId(id);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load item detail');
    } finally {
      setDetailLoading(false);
    }
  };

  const handleAction = async (id: string, action: ActivityAction) => {
    setActionError(null);
    setError(null);
    setActionLoadingId(id);
    isActionPendingRef.current = true;

    // Stop polling during action
    if (pollIntervalRef.current) {
      window.clearInterval(pollIntervalRef.current);
      pollIntervalRef.current = null;
    }

    try {
      const data = await apiActivityAction(id, action);
      setSuccess('Action completed');
      setTimeout(() => setSuccess(null), 3000);
      // Update the item optimistically from response
      setItems((prev) => prev.map((item) => (item.id === id ? data.item : item)));
      // Refresh list and detail if open
      await loadItems();
      if (expandedId === id) {
        await handleExpand(id);
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : 'Action failed';
      let friendlyMessage = message;
      if (message === 'control_not_supported') {
        friendlyMessage = "This item doesn't support that action.";
      } else if (message === 'already_running') {
        friendlyMessage = 'This job is already running.';
      } else if (message === 'not_running') {
        friendlyMessage = 'This job is not running.';
      } else if (message === 'item_not_found') {
        friendlyMessage = 'This item no longer exists — the list has been refreshed.';
        await loadItems();
        if (expandedId === id) {
          setExpandedId(null);
          setDetail(null);
        }
      } else if (message === 'source_unavailable') {
        friendlyMessage = 'That source is unavailable right now.';
      }
      setActionError({ id, message: friendlyMessage, code: message });
    } finally {
      setActionLoadingId(null);
      isActionPendingRef.current = false;
      // Restart polling if tab visible
      if (!document.hidden && !pollIntervalRef.current) {
        pollIntervalRef.current = window.setInterval(() => {
          if (!document.hidden && !isActionPendingRef.current) {
            loadItems();
          }
        }, 15000);
      }
    }
  };

  if (loading) {
    return (
      <div className="settings-page">
        <div className="settings-loading">Loading…</div>
      </div>
    );
  }

  // Build summary chips from summary (unfiltered state)
  const summaryChips = summary
    ? ACTIVITY_STATUS_ORDER.filter((status) => (summary.by_status[status] ?? 0) > 0).map((status) => (
        <Badge key={status} variant={getStatusBadgeVariant(status)}>
          {getStatusLabel(status)}: {summary.by_status[status]}
        </Badge>
      ))
    : [];

  // Failed banner
  const failedItems = summary?.failed?.length
    ? items.filter((item) => summary.failed.includes(item.id))
    : [];

  return (
    <div className="settings-page">
      <div className="settings-header">
        <h1 className="settings-page-title">Activity & Automation</h1>
      </div>

      {(error || success) && (
        <div
          className={`settings-message${error ? ' settings-message--error' : ''}${success ? ' settings-message--success' : ''}`}
          role="alert"
        >
          {error || success}
        </div>
      )}

      {/* Summary header */}
      <div className="activity-summary" style={{ marginBottom: 'var(--space-6)' }}>
        <p style={{ margin: '0 0 var(--space-3)', color: 'var(--text-secondary)', fontSize: 'var(--text-sm)' }}>
          Total: <span className="num" style={{ fontWeight: 600 }}>{summary?.total ?? items.length}</span> jobs
        </p>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
          {summaryChips}
        </div>
      </div>

      {/* Failed banner */}
      {failedItems.length > 0 && (
        <div className="activity-fail-banner" style={{ marginBottom: 'var(--space-6)' }}>
          {failedItems.map((item) => (
            <div key={item.id} style={{ display: 'flex', gap: 'var(--space-4)', alignItems: 'flex-start', padding: 'var(--space-4)', background: 'var(--danger-subtle)', border: '1px solid var(--danger)', borderRadius: 'var(--radius-lg)' }}>
              <span style={{ fontSize: 'var(--text-lg)', flexShrink: 0 }}>⛔</span>
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ fontWeight: 600, color: 'var(--danger)', marginBottom: 'var(--space-1)' }}>
                  {item.name}
                </div>
                {item.last_run?.failure_reason && (
                  <p style={{ margin: '0 0 var(--space-2)', fontSize: 'var(--text-sm)', color: 'var(--danger)' }}>
                    {item.last_run.failure_reason}
                  </p>
                )}
                {item.last_run?.recovery_hint && (
                  <p style={{ margin: 0, fontSize: 'var(--text-sm)', color: 'var(--text-secondary)' }}>
                    {item.last_run.recovery_hint}
                  </p>
                )}
              </div>
              <Button
                variant="secondary"
                className="activity-fail-retry"
                onClick={() => handleExpand(item.id)}
                disabled={anyLoading}
              >
                View details
              </Button>
            </div>
          ))}
        </div>
      )}

      <Section title="Activity">
        <div className="settings-card">
          {/* Filter tabs */}
          <div className="activity-filters" style={{ display: 'flex', gap: 'var(--space-2)', marginBottom: 'var(--space-6)', flexWrap: 'wrap' }}>
            <div style={{ display: 'flex', gap: 'var(--space-2)', flexWrap: 'wrap' }}>
              {FILTER_SOURCES.map((source) => (
                <Button
                  key={source}
                  variant={filterSource === source ? 'primary' : 'secondary'}
                  onClick={() => setFilterSource(source)}
                  className="filter-tab"
                  disabled={anyLoading}
                >
                  {source === 'all' ? 'All sources' : ACTIVITY_SOURCE_LABELS[source as ActivitySource]}
                </Button>
              ))}
            </div>
            <div style={{ display: 'flex', gap: 'var(--space-2)', flexWrap: 'wrap', marginLeft: 'auto' }}>
              {FILTER_STATUSES.map((status) => (
                <Button
                  key={status}
                  variant={filterStatus === status ? 'primary' : 'secondary'}
                  onClick={() => setFilterStatus(status)}
                  className="filter-tab"
                  disabled={anyLoading}
                >
                  {status === 'all' ? 'All statuses' : getStatusLabel(status as ActivityStatus)}
                </Button>
              ))}
            </div>
          </div>

          {/* Items list */}
          {items.length === 0 ? (
            <div className="settings-placeholder" style={{ padding: 'var(--space-10)', textAlign: 'center' }}>
              <p style={{ margin: '0 0 var(--space-2)', color: 'var(--text-secondary)' }}>
                {filterSource !== 'all' || filterStatus !== 'all'
                  ? 'No items match the current filters'
                  : 'No activity items found'}
              </p>
              <p style={{ margin: 0, fontSize: 'var(--text-sm)', color: 'var(--text-faint)' }}>
                {filterSource !== 'all' || filterStatus !== 'all'
                  ? 'Try adjusting the filters above.'
                  : 'Background jobs will appear here as they are scheduled or run.'}
              </p>
            </div>
          ) : (
            <div className="activity-list" style={{ display: 'flex', flexDirection: 'column' }}>
              {items.map((item) => (
                <div
                  key={item.id}
                  className="activity-row"
                  style={{
                    display: 'flex',
                    gap: 'var(--space-4)',
                    padding: 'var(--space-4) 0',
                    borderTop: item.id === items[0].id ? 'none' : '1px solid var(--border)',
                    cursor: expandedId === item.id ? 'default' : 'pointer',
                    alignItems: 'flex-start',
                  }}
                  onClick={() => !actionLoadingId && !detailLoading && handleExpand(item.id)}
                >
                  {/* Status dot */}
                  <span
                    className={`dot ${getStatusDotClass(item.status)}`}
                    style={{
                      width: '10px',
                      height: '10px',
                      borderRadius: '50%',
                      flexShrink: 0,
                      marginTop: '6px',
                    }}
                  />

                  {/* Main block */}
                  <div style={{ flex: 1, minWidth: 0 }}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)', flexWrap: 'wrap', marginBottom: 'var(--space-1)' }}>
                      <span className="activity-name" style={{ fontWeight: 550, fontSize: 'var(--text-base)' }}>
                        {item.name}
                      </span>
                      <Badge variant={getStatusBadgeVariant(item.status)}>
                        {getStatusLabel(item.status)}
                      </Badge>
                    </div>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-4)', fontSize: 'var(--text-sm)', color: 'var(--text-secondary)', flexWrap: 'wrap' }}>
                      <span>{ACTIVITY_SOURCE_LABELS[item.source]}</span>
                      {item.kind && item.kind !== item.source && <span>· {item.kind}</span>}
                    </div>
                  </div>

                  {/* Right column: Next/Last + Actions */}
                  <div
                    className="activity-right"
                    style={{
                      display: 'flex',
                      flexDirection: 'column',
                      alignItems: 'flex-end',
                      gap: 'var(--space-2)',
                      flexShrink: 0,
                      minWidth: '160px',
                    }}
                  >
                    {/* Next run / Manual label */}
                    <div style={{ textAlign: 'right', display: 'flex', flexDirection: 'column', alignItems: 'flex-end', gap: 2 }}>
                      {item.next_run_at ? (
                        <>
                          <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-faint)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-label)' }}>
                            Next
                          </span>
                          <span className="num" style={{ fontSize: 'var(--text-sm)', fontWeight: 550 }}>
                            {formatTimestamp(item.next_run_at)}
                          </span>
                        </>
                      ) : item.status === 'manual' ? (
                        <>
                          <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-faint)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-label)' }}>
                            Manual
                          </span>
                          <span style={{ fontSize: 'var(--text-sm)', fontWeight: 550, color: 'var(--text-secondary)' }}>
                            Manual
                          </span>
                        </>
                      ) : (
                        <>
                          <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-faint)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-label)' }}>
                            Next
                          </span>
                          <span style={{ fontSize: 'var(--text-sm)', fontWeight: 550, color: 'var(--text-faint)' }}>
                            —
                          </span>
                        </>
                      )}
                    </div>

                    {/* Last run */}
                    <div style={{ textAlign: 'right', display: 'flex', flexDirection: 'column', alignItems: 'flex-end', gap: 2 }}>
                      <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-faint)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-label)' }}>
                        Last
                      </span>
                      {item.last_run ? (
                        <span
                          className="num"
                          style={{
                            fontSize: 'var(--text-sm)',
                            fontWeight: 550,
                            color: item.last_run.result === 'ok' ? 'var(--ok)' : item.last_run.result === 'failed' ? 'var(--danger)' : 'var(--text)',
                          }}
                        >
                          {item.last_run.result === 'ok' ? 'OK' : item.last_run.result === 'failed' ? 'Failed' : item.last_run.result} ·{' '}
                          {formatDuration(item.last_run.duration_seconds)}
                        </span>
                      ) : (
                        <span style={{ fontSize: 'var(--text-sm)', fontWeight: 550, color: 'var(--text-faint)' }}>
                          Never run
                        </span>
                      )}
                    </div>

                    {/* Actions */}
                    <div
                      className="activity-actions"
                      style={{
                        display: 'flex',
                        gap: 'var(--space-2)',
                        flexWrap: 'wrap',
                        justifyContent: 'flex-end',
                        marginTop: 'var(--space-2)',
                      }}
                    >
                      {item.controls.map((action) => (
                        <Button
                          key={action}
                          variant={action === 'run-now' ? 'primary' : action === 'cancel' ? 'secondary' : 'secondary'}
                          onClick={(e) => {
                            e.stopPropagation();
                            handleAction(item.id, action);
                          }}
                          disabled={anyLoading || actionLoadingId === item.id}
                        >
                          {action === 'pause'
                            ? 'Pause'
                            : action === 'resume'
                            ? 'Resume'
                            : action === 'cancel'
                            ? 'Cancel'
                            : 'Run now'}
                        </Button>
                      ))}
                      {item.controls.length === 0 && (
                        <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-faint)', alignSelf: 'center' }}>
                          No actions
                        </span>
                      )}
                      {actionError?.id === item.id && (
                        <div
                          style={{
                            width: '100%',
                            marginTop: 'var(--space-2)',
                            padding: 'var(--space-2) var(--space-3)',
                            background: 'var(--danger-subtle)',
                            border: '1px solid var(--danger)',
                            borderRadius: 'var(--radius-sm)',
                            fontSize: 'var(--text-xs)',
                            color: 'var(--danger)',
                            display: 'flex',
                            alignItems: 'center',
                            gap: 'var(--space-2)',
                          }}
                        >
                          <span>{actionError.message}</span>
                          <span style={{ fontFamily: 'monospace', opacity: 0.7 }}>{actionError.code}</span>
                        </div>
                      )}
                    </div>
                  </div>
                </div>
              ))}
            </div>
          )}

          {/* Expanded detail */}
          {expandedId && detail && (
            <div
              className="activity-detail"
              style={{
                marginTop: 'var(--space-6)',
                paddingTop: 'var(--space-6)',
                borderTop: '1px solid var(--border)',
              }}
            >
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--space-4)' }}>
                <h3 style={{ margin: 0, fontSize: 'var(--text-lg)', fontWeight: 600 }}>
                  {detail.name}
                  <span className="num" style={{ fontWeight: 400, color: 'var(--text-secondary)', marginLeft: 'var(--space-3)' }}>
                    {detail.source}:{detail.kind}
                  </span>
                </h3>
                <Button variant="ghost" onClick={() => { setExpandedId(null); setDetail(null); }} disabled={anyLoading}>
                  Close
                </Button>
              </div>

              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 'var(--space-6)' }}>
                {/* Last Run */}
                <div>
                  <h4 style={{ margin: '0 0 var(--space-3)', fontSize: 'var(--text-sm)', fontWeight: 600, color: 'var(--text-secondary)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-label)' }}>
                    Last Run
                  </h4>
                  {detail.last_run ? (
                    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)', flexWrap: 'wrap' }}>
                        <Badge variant={detail.last_run.result === 'ok' ? 'success' : detail.last_run.result === 'failed' ? 'danger' : 'neutral'}>
                          {detail.last_run.result === 'ok' ? 'OK' : detail.last_run.result === 'failed' ? 'Failed' : detail.last_run.result}
                        </Badge>
                        <span className="num" style={{ fontSize: 'var(--text-sm)', color: 'var(--text-secondary)' }}>
                          Started {formatTimestamp(detail.last_run.started_at)}
                        </span>
                        <span className="num" style={{ fontSize: 'var(--text-sm)', color: 'var(--text-secondary)' }}>
                          Duration: {formatDuration(detail.last_run.duration_seconds)}
                        </span>
                      </div>
                      {detail.last_run.summary && (
                        <div
                          className={detail.last_run.result === 'failed' ? 'activity-log' : ''}
                          style={{
                            fontFamily: 'ui-monospace, Menlo, Consolas, monospace',
                            fontSize: 'var(--text-xs)',
                            lineHeight: '1.5',
                            whiteSpace: 'pre-wrap',
                            background: detail.last_run.result === 'failed' ? '#17181c' : 'var(--surface-subtle)',
                            color: detail.last_run.result === 'failed' ? '#c9d1d9' : 'var(--text)',
                            borderRadius: 'var(--radius-sm)',
                            padding: 'var(--space-3)',
                            border: detail.last_run.result === 'failed' ? 'none' : '1px solid var(--border)',
                            maxHeight: '300px',
                            overflow: 'auto',
                          }}
                        >
                          {detail.last_run.summary}
                        </div>
                      )}
                      {detail.last_run.result === 'failed' && (
                        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
                          {detail.last_run.failure_reason && (
                            <div style={{ padding: 'var(--space-3)', background: 'var(--danger-subtle)', border: '1px solid var(--danger)', borderRadius: 'var(--radius-sm)', color: 'var(--danger)' }}>
                              <div style={{ fontWeight: 600, marginBottom: 'var(--space-1)' }}>Failure Reason</div>
                              <div style={{ fontSize: 'var(--text-sm)' }}>{detail.last_run.failure_reason}</div>
                            </div>
                          )}
                          {detail.last_run.recovery_hint && (
                            <div style={{ padding: 'var(--space-3)', background: 'var(--ok-subtle)', border: '1px solid var(--ok)', borderRadius: 'var(--radius-sm)', color: 'var(--ok)' }}>
                              <div style={{ fontWeight: 600, marginBottom: 'var(--space-1)' }}>Recovery Hint</div>
                              <div style={{ fontSize: 'var(--text-sm)' }}>{detail.last_run.recovery_hint}</div>
                            </div>
                          )}
                        </div>
                      )}
                    </div>
                  ) : (
                    <p style={{ margin: 0, color: 'var(--text-faint)', fontSize: 'var(--text-sm)' }}>Never run</p>
                  )}
                </div>

                {/* Recent Runs */}
                <div>
                  <h4 style={{ margin: '0 0 var(--space-3)', fontSize: 'var(--text-sm)', fontWeight: 600, color: 'var(--text-secondary)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-label)' }}>
                    Recent Runs (max 5)
                  </h4>
                  {detail.recent_runs && detail.recent_runs.length > 0 ? (
                    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)', maxHeight: '400px', overflow: 'auto' }}>
                      {detail.recent_runs.map((run, idx) => (
                        <div
                          key={idx}
                          style={{
                            display: 'flex',
                            alignItems: 'center',
                            justifyContent: 'space-between',
                            gap: 'var(--space-3)',
                            padding: 'var(--space-3)',
                            background: 'var(--surface-subtle)',
                            border: '1px solid var(--border)',
                            borderRadius: 'var(--radius-sm)',
                          }}
                        >
                          <div style={{ display: 'flex', flexDirection: 'column', gap: 2, minWidth: 0 }}>
                            <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)' }}>
                              <Badge variant={run.result === 'ok' ? 'success' : run.result === 'failed' ? 'danger' : 'neutral'}>
                                {run.result === 'ok' ? 'OK' : run.result === 'failed' ? 'Failed' : run.result}
                              </Badge>
                              <span className="num" style={{ fontSize: 'var(--text-xs)', color: 'var(--text-secondary)' }}>
                                {formatTimestamp(run.started_at)}
                              </span>
                            </div>
                            <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-faint)' }}>
                              Duration: {formatDuration(run.duration_seconds)}
                            </span>
                          </div>
                        </div>
                      ))}
                    </div>
                  ) : (
                    <p style={{ margin: 0, color: 'var(--text-faint)', fontSize: 'var(--text-sm)' }}>No recent runs</p>
                  )}
                </div>
              </div>

              {/* Source error for unavailable items */}
              {detail.source_error && (
                <div
                  style={{
                    marginTop: 'var(--space-6)',
                    padding: 'var(--space-4)',
                    background: 'var(--danger-subtle)',
                    border: '1px solid var(--danger)',
                    borderRadius: 'var(--radius-sm)',
                    color: 'var(--danger)',
                  }}
                >
                  <div style={{ fontWeight: 600, marginBottom: 'var(--space-1)' }}>Source Error</div>
                  <div style={{ fontSize: 'var(--text-sm)' }}>{detail.source_error}</div>
                </div>
              )}
            </div>
          )}
        </div>
      </Section>

      {/* Rules Section */}
      <Section title="Event→Action Rules">
        <div className="settings-card">
          {(ruleError || ruleSuccess) && (
            <div
              className={`settings-message${ruleError ? ' settings-message--error' : ''}${ruleSuccess ? ' settings-message--success' : ''}`}
              role="alert"
              style={{ marginBottom: 'var(--space-4)' }}
            >
              {ruleError || ruleSuccess}
            </div>
          )}

          {/* Toolbar */}
          <div className="rules-toolbar" style={{ display: 'flex', gap: 'var(--space-3)', marginBottom: 'var(--space-6)', flexWrap: 'wrap', alignItems: 'center' }}>
            <Button variant="primary" onClick={openCreateRule} disabled={catalogLoading || anyRuleLoading}>
              {catalogLoading ? 'Loading…' : 'Add Rule'}
            </Button>
            <div style={{ display: 'flex', gap: 'var(--space-2)', marginLeft: 'auto' }}>
              {(['all', 'enabled', 'disabled'] as const).map((filter) => (
                <Button
                  key={filter}
                  variant={filterEnabled === filter ? 'primary' : 'secondary'}
                  onClick={() => setFilterEnabled(filter)}
                  disabled={anyRuleLoading}
                  className="filter-tab"
                >
                  {filter === 'all' ? 'All' : filter === 'enabled' ? 'Enabled' : 'Disabled'}
                </Button>
              ))}
            </div>
          </div>

          {/* Rules list table */}
          {rulesLoading ? (
            <div className="settings-loading" style={{ padding: 'var(--space-10)', textAlign: 'center' }}>Loading rules…</div>
          ) : filteredRules.length === 0 ? (
            <div className="settings-placeholder" style={{ padding: 'var(--space-10)', textAlign: 'center' }}>
              <p style={{ margin: '0 0 var(--space-2)', color: 'var(--text-secondary)' }}>
                {filterEnabled !== 'all' ? 'No rules match the current filter' : 'No rules configured'}
              </p>
              <p style={{ margin: 0, fontSize: 'var(--text-sm)', color: 'var(--text-faint)' }}>
                {filterEnabled !== 'all' ? 'Try adjusting the filter above.' : 'Click "Add Rule" to create your first event→action rule.'}
              </p>
            </div>
          ) : (
            <div className="rules-table-container" style={{ overflowX: 'auto' }}>
              <table className="rules-table" style={{ width: '100%', borderCollapse: 'collapse', fontSize: 'var(--text-sm)' }}>
                <thead>
                  <tr style={{ borderBottom: '2px solid var(--border)', textAlign: 'left' }}>
                    <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Name</th>
                    <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Event</th>
                    <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Actions</th>
                    <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Enabled</th>
                    <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Cooldown</th>
                    <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Max/Day</th>
                    <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Last Fired</th>
                    <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>State</th>
                    <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {filteredRules.map((rule) => (
                    <tr key={rule.id} style={{ borderBottom: '1px solid var(--border)' }}>
                      <td style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 500 }}>{rule.name}</td>
                      <td style={{ padding: 'var(--space-3) var(--space-4)', color: 'var(--text-secondary)', maxWidth: '200px', textOverflow: 'ellipsis', overflow: 'hidden', whiteSpace: 'nowrap' }}>
                        {catalog ? getEventLabel(catalog, rule.event_type) : rule.event_type}
                      </td>
                      <td style={{ padding: 'var(--space-3) var(--space-4)', color: 'var(--text-secondary)', maxWidth: '250px', textOverflow: 'ellipsis', overflow: 'hidden', whiteSpace: 'nowrap' }}>
                        {catalog ? getActionLabels(catalog, rule.actions) : rule.actions.map((a) => a.type).join(', ')}
                      </td>
                      <td style={{ padding: 'var(--space-3) var(--space-4)' }}>
                        <Badge variant={rule.enabled ? 'success' : 'neutral'}>{rule.enabled ? 'Yes' : 'No'}</Badge>
                      </td>
                      <td style={{ padding: 'var(--space-3) var(--space-4)', fontFamily: 'monospace', fontSize: 'var(--text-xs)' }}>
                        {formatCooldown(rule.cooldown_seconds)}
                      </td>
                      <td style={{ padding: 'var(--space-3) var(--space-4)', fontFamily: 'monospace', fontSize: 'var(--text-xs)' }}>
                        {rule.max_fires_per_day}
                      </td>
                      <td style={{ padding: 'var(--space-3) var(--space-4)', fontSize: 'var(--text-xs)', color: 'var(--text-secondary)' }}>
                        {formatTimestamp(rule.last_fired_at)}
                      </td>
                      <td style={{ padding: 'var(--space-3) var(--space-4)' }}>
                        <Badge variant={RULE_STATE_VARIANTS[rule.state]}>{RULE_STATE_LABELS[rule.state]}</Badge>
                      </td>
                      <td style={{ padding: 'var(--space-3) var(--space-4)' }}>
                        <div style={{ display: 'flex', gap: 'var(--space-2)', flexWrap: 'wrap' }}>
                          <Button
                            variant="secondary"
                            size="sm"
                            onClick={() => openEditRule(rule.id)}
                            disabled={anyRuleLoading || ruleActionLoadingId === rule.id}
                          >
                            Edit
                          </Button>
                          <Button
                            variant="secondary"
                            size="sm"
                            onClick={() => openTestFire(rule.id)}
                            disabled={anyRuleLoading || ruleActionLoadingId === rule.id}
                          >
                            Test Fire
                          </Button>
                          <Button
                            variant="secondary"
                            size="sm"
                            onClick={() => openViewRuns(rule.id)}
                            disabled={anyRuleLoading || ruleActionLoadingId === rule.id}
                          >
                            View Runs
                          </Button>
                          <Button
                            variant="secondary"
                            size="sm"
                            onClick={() => openDeleteConfirm(rule.id)}
                            disabled={anyRuleLoading || ruleActionLoadingId === rule.id}
                          >
                            Delete
                          </Button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </Section>

      {/* Create/Edit Rule Modal */}
      <Modal isOpen={createRuleOpen || editRuleId !== null} onClose={() => { setCreateRuleOpen(false); setEditRuleId(null); resetForm(); }} title={editRuleId ? 'Edit Rule' : 'Create Rule'} size="lg">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-6)' }}>
          <Input
            label="Name"
            value={formName}
            onChange={setFormName}
            required
            error={formErrors.name}
            disabled={ruleActionLoadingId !== null}
          />

          <Select
            label="Event Type"
            value={formEventType}
            onChange={handleEventTypeChange}
            options={catalog?.events.map((e) => ({ value: e.type, label: e.label })) || []}
            required
            error={formErrors.eventType}
            disabled={ruleActionLoadingId !== null || editRuleId !== null}
            help="Select the event that triggers this rule"
          />

          {formEventType && catalog && (
            <div style={{ border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', padding: 'var(--space-4)', background: 'var(--surface-subtle)' }}>
              <h4 style={{ margin: '0 0 var(--space-3)', fontSize: 'var(--text-sm)', fontWeight: 600, color: 'var(--text-secondary)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-label)' }}>
                Event Parameters
              </h4>
              {catalog.events.find((e) => e.type === formEventType)?.params.map((param) => (
                <div key={param.name} style={{ marginBottom: 'var(--space-3)' }}>
                  {renderParamInput(
                    param,
                    formEventParams[param.name],
                    (v) => handleEventParamChange(param.name, v),
                    ruleActionLoadingId !== null,
                    []
                  )}
                  {formErrors[`event${param.name}`] && (
                    <p style={{ margin: 'var(--space-1) 0 0', fontSize: 'var(--text-xs)', color: 'var(--danger)' }}>{formErrors[`event${param.name}`]}</p>
                  )}
                </div>
              ))}
              {catalog.events.find((e) => e.type === formEventType)?.params.length === 0 && (
                <p style={{ margin: 0, fontSize: 'var(--text-sm)', color: 'var(--text-faint)' }}>No parameters for this event type.</p>
              )}
            </div>
          )}

          <div style={{ border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', padding: 'var(--space-4)', background: 'var(--surface-subtle)' }}>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--space-4)' }}>
              <h4 style={{ margin: 0, fontSize: 'var(--text-sm)', fontWeight: 600, color: 'var(--text-secondary)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-label)' }}>
                Actions (1–3)
              </h4>
              {formActions.length < 3 && (
                <Button variant="secondary" size="sm" onClick={addAction} disabled={ruleActionLoadingId !== null}>
                  + Add Action
                </Button>
              )}
            </div>
            {formActions.map((action, idx) => (
              <div key={idx} style={{ borderTop: idx > 0 ? '1px solid var(--border)' : 'none', paddingTop: idx > 0 ? 'var(--space-4)' : 0, marginTop: idx > 0 ? 'var(--space-4)' : 0 }}>
                <div style={{ display: 'flex', alignItems: 'flex-start', gap: 'var(--space-3)', marginBottom: 'var(--space-3)' }}>
                  <Select
                    label={`Action ${idx + 1} Type`}
                    value={action.type}
                    onChange={(v) => handleActionTypeChange(idx, v)}
                    options={catalog?.actions.map((a) => ({ value: a.type, label: a.label })) || []}
                    required
                    error={formErrors[`action${idx}Type`]}
                    disabled={ruleActionLoadingId !== null}
                    help="Select the action to perform when the event fires"
                  />
                  {formActions.length > 1 && (
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => removeAction(idx)}
                      disabled={ruleActionLoadingId !== null}
                      style={{ marginTop: 'var(--space-6)', alignSelf: 'flex-start' }}
                    >
                      Remove
                    </Button>
                  )}
                </div>
                {action.type && catalog && (
                  <div style={{ paddingLeft: 'var(--space-4)', borderLeft: '2px solid var(--border)' }}>
                    {catalog.actions.find((a) => a.type === action.type)?.params.map((param) => (
                      <div key={param.name} style={{ marginBottom: 'var(--space-3)' }}>
                        {renderParamInput(
                          param,
                          action.params[param.name],
                          (v) => handleActionParamChange(idx, param.name, v),
                          ruleActionLoadingId !== null,
                          [] // agent names would come from another API
                        )}
                        {formErrors[`action${idx}${param.name}`] && (
                          <p style={{ margin: 'var(--space-1) 0 0', fontSize: 'var(--text-xs)', color: 'var(--danger)' }}>{formErrors[`action${idx}${param.name}`]}</p>
                        )}
                      </div>
                    ))}
                    {/* delay_seconds is common to all actions */}
                    <div style={{ marginBottom: 'var(--space-3)' }}>
                      {renderParamInput(
                        { name: 'delay_seconds', type: 'number', required: false, default: 0, description: 'Delay before executing (seconds)', min: 0, max: 86400 },
                        action.params.delay_seconds,
                        (v) => handleActionParamChange(idx, 'delay_seconds', v),
                        ruleActionLoadingId !== null,
                        []
                      )}
                      {formErrors[`action${idx}delay_seconds`] && (
                        <p style={{ margin: 'var(--space-1) 0 0', fontSize: 'var(--text-xs)', color: 'var(--danger)' }}>{formErrors[`action${idx}delay_seconds`]}</p>
                      )}
                    </div>
                  </div>
                )}
              </div>
            ))}
          </div>

          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 'var(--space-4)' }}>
            <Checkbox
              label="Enabled"
              checked={formEnabled}
              onChange={setFormEnabled}
              disabled={ruleActionLoadingId !== null}
            />
            <Input
              label="Cooldown (seconds)"
              type="number"
              value={formCooldown}
              onChange={setFormCooldown}
              required
              disabled={ruleActionLoadingId !== null}
              help="Minimum time between rule firings (default 3600)"
            />
            <Input
              label="Max Fires/Day"
              type="number"
              value={formMaxFires}
              onChange={setFormMaxFires}
              required
              disabled={ruleActionLoadingId !== null}
              help="Maximum times this rule can fire per day (default 10)"
            />
          </div>

          <div style={{ display: 'flex', gap: 'var(--space-3)', justifyContent: 'flex-end', marginTop: 'var(--space-4)', paddingTop: 'var(--space-4)', borderTop: '1px solid var(--border)' }}>
            <Button variant="secondary" onClick={() => { setCreateRuleOpen(false); setEditRuleId(null); resetForm(); }} disabled={ruleActionLoadingId !== null}>
              Cancel
            </Button>
            <Button variant="primary" onClick={handleSubmitRule} disabled={ruleActionLoadingId !== null}>
              {ruleActionLoadingId ? 'Saving…' : editRuleId ? 'Update Rule' : 'Create Rule'}
            </Button>
          </div>
        </div>
      </Modal>

      {/* Test Fire Modal */}
      <Modal isOpen={testFireRuleId !== null} onClose={() => { setTestFireRuleId(null); setTestFireEventRef(''); setTestFireEventData('{}'); setTestFireError(null); setTestFireResult(null); }} title="Test Fire Rule" size="md">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
          <Input
            label="Event Ref (optional)"
            value={testFireEventRef}
            onChange={setTestFireEventRef}
            help="Reference ID for the test event (e.g., backup run ID)"
          />
          <Textarea
            label="Event Data (JSON)"
            value={testFireEventData}
            onChange={setTestFireEventData}
            rows={6}
            help="Optional event payload as JSON"
          />
          {testFireError && (
            <div style={{ padding: 'var(--space-3)', background: 'var(--danger-subtle)', border: '1px solid var(--danger)', borderRadius: 'var(--radius-sm)', color: 'var(--danger)', fontSize: 'var(--text-sm)' }}>
              {testFireError}
            </div>
          )}
          {testFireResult && (
            <div style={{ padding: 'var(--space-3)', background: 'var(--ok-subtle)', border: '1px solid var(--ok)', borderRadius: 'var(--radius-sm)', color: 'var(--ok)' }}>
              <div style={{ fontWeight: 600, marginBottom: 'var(--space-2)' }}>Test Fire Result</div>
              <div style={{ fontFamily: 'monospace', fontSize: 'var(--text-xs)', whiteSpace: 'pre-wrap' }}>
                {JSON.stringify(testFireResult, null, 2)}
              </div>
            </div>
          )}
          <div style={{ display: 'flex', gap: 'var(--space-3)', justifyContent: 'flex-end', marginTop: 'var(--space-2)', paddingTop: 'var(--space-4)', borderTop: '1px solid var(--border)' }}>
            <Button variant="secondary" onClick={() => { setTestFireRuleId(null); setTestFireEventRef(''); setTestFireEventData('{}'); setTestFireError(null); setTestFireResult(null); }} disabled={ruleActionLoadingId !== null}>
              Close
            </Button>
            <Button variant="primary" onClick={handleTestFire} disabled={ruleActionLoadingId !== null}>
              {ruleActionLoadingId === testFireRuleId ? 'Firing…' : 'Test Fire'}
            </Button>
          </div>
        </div>
      </Modal>

      {/* View Runs Modal */}
      <Modal isOpen={viewRunsRuleId !== null} onClose={() => { setViewRunsRuleId(null); setRuns([]); setRunsTotal(0); setRunsPage(0); }} title="Rule Runs" size="xl">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
          {runsLoading ? (
            <div className="settings-loading" style={{ padding: 'var(--space-10)', textAlign: 'center' }}>Loading runs…</div>
          ) : runs.length === 0 ? (
            <div className="settings-placeholder" style={{ padding: 'var(--space-10)', textAlign: 'center' }}>
              <p style={{ margin: 0, color: 'var(--text-secondary)' }}>No runs recorded for this rule</p>
            </div>
          ) : (
            <>
              <div className="runs-table-container" style={{ overflowX: 'auto' }}>
                <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 'var(--text-sm)' }}>
                  <thead>
                    <tr style={{ borderBottom: '2px solid var(--border)', textAlign: 'left' }}>
                      <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Result</th>
                      <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Detail</th>
                      <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Alert ID</th>
                      <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Test Fire</th>
                      <th style={{ padding: 'var(--space-3) var(--space-4)', fontWeight: 600, color: 'var(--text-secondary)', whiteSpace: 'nowrap' }}>Fired At</th>
                    </tr>
                  </thead>
                  <tbody>
                    {runs.map((run) => (
                      <tr key={run.id} style={{ borderBottom: '1px solid var(--border)' }}>
                        <td style={{ padding: 'var(--space-3) var(--space-4)' }}>
                          <Badge variant={run.result === 'ok' ? 'success' : run.result === 'failed' ? 'danger' : 'warning'}>
                            {run.result === 'ok' ? 'OK' : run.result === 'failed' ? 'Failed' : 'Skipped'}
                          </Badge>
                        </td>
                        <td style={{ padding: 'var(--space-3) var(--space-4)', fontFamily: 'monospace', fontSize: 'var(--text-xs)', color: 'var(--text-secondary)', maxWidth: '300px', textOverflow: 'ellipsis', overflow: 'hidden', whiteSpace: 'nowrap' }}>
                          {run.detail || '—'}
                        </td>
                        <td style={{ padding: 'var(--space-3) var(--space-4)', fontFamily: 'monospace', fontSize: 'var(--text-xs)', color: 'var(--text-secondary)' }}>
                          {run.alert_id || '—'}
                        </td>
                        <td style={{ padding: 'var(--space-3) var(--space-4)' }}>
                          <Badge variant={run.test_fire ? 'info' : 'neutral'}>{run.test_fire ? 'Yes' : 'No'}</Badge>
                        </td>
                        <td style={{ padding: 'var(--space-3) var(--space-4)', fontSize: 'var(--text-xs)', color: 'var(--text-secondary)' }}>
                          {formatTimestamp(run.fired_at)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              {/* Pagination */}
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', paddingTop: 'var(--space-4)', borderTop: '1px solid var(--border)' }}>
                <span style={{ fontSize: 'var(--text-sm)', color: 'var(--text-secondary)' }}>
                  Page {runsPage + 1} of {Math.ceil(runsTotal / RUNS_PAGE_SIZE)} · Total: {runsTotal}
                </span>
                <div style={{ display: 'flex', gap: 'var(--space-2)' }}>
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => loadRuns(runsPage - 1)}
                    disabled={runsPage === 0 || runsLoading}
                  >
                    Previous
                  </Button>
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => loadRuns(runsPage + 1)}
                    disabled={(runsPage + 1) * RUNS_PAGE_SIZE >= runsTotal || runsLoading}
                  >
                    Next
                  </Button>
                </div>
              </div>
            </>
          )}
          <div style={{ display: 'flex', gap: 'var(--space-3)', justifyContent: 'flex-end', marginTop: 'var(--space-2)', paddingTop: 'var(--space-4)', borderTop: '1px solid var(--border)' }}>
            <Button variant="secondary" onClick={() => { setViewRunsRuleId(null); setRuns([]); setRunsTotal(0); setRunsPage(0); }} disabled={runsLoading}>
              Close
            </Button>
          </div>
        </div>
      </Modal>

      {/* Delete Confirmation Modal */}
      <Modal isOpen={deleteRuleId !== null} onClose={() => setDeleteRuleId(null)} title="Delete Rule" size="sm">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
          <p style={{ margin: 0, color: 'var(--text-secondary)' }}>
            Are you sure you want to delete this rule? This action cannot be undone.
          </p>
          <div style={{ display: 'flex', gap: 'var(--space-3)', justifyContent: 'flex-end' }}>
            <Button variant="secondary" onClick={() => setDeleteRuleId(null)} disabled={ruleActionLoadingId !== null}>
              Cancel
            </Button>
            <Button variant="danger" onClick={handleDeleteRule} disabled={ruleActionLoadingId !== null}>
              {ruleActionLoadingId === deleteRuleId ? 'Deleting…' : 'Delete'}
            </Button>
          </div>
        </div>
      </Modal>
    </div>
  );
}