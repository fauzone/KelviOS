import { useState, useEffect, type ReactNode, useCallback } from 'react';
import {
  apiListComposeArtifacts,
  apiGetComposeArtifact,
  apiCreateComposeArtifact,
  apiUpdateComposeArtifact,
  apiActivateComposeArtifact,
  apiDisableComposeArtifact,
  apiRollbackComposeArtifact,
  apiDeleteComposeArtifact,
  type ComposeArtifact,
  type ComposeArtifactDetail,
  type ComposeKind,
  type ComposeStatus,
  COMPOSE_KIND_LABELS,
  COMPOSE_STATUS_LABELS,
} from '../api';

function Section({ title, children, className = '' }: { title: string; children: ReactNode; className?: string }) {
  return (
    <section className={`settings-section ${className}`}>
      <h2 className="settings-section-title">{title}</h2>
      {children}
    </section>
  );
}

function Badge({ children, variant = 'neutral' }: { children: ReactNode; variant?: 'success' | 'warning' | 'neutral' }) {
  return (
    <span className={`badge badge--${variant}`}>{children}</span>
  );
}

function Button({
  children,
  onClick,
  variant = 'secondary',
  disabled,
  className = '',
  type = 'button',
}: {
  children: ReactNode;
  onClick?: (e: React.MouseEvent<HTMLButtonElement>) => void;
  variant?: 'primary' | 'secondary' | 'ghost';
  disabled?: boolean;
  className?: string;
  type?: 'button' | 'submit';
}) {
  return (
    <button
      type={type}
      className={`button button--${variant} ${className}`}
      onClick={onClick}
      disabled={disabled}
    >
      {children}
    </button>
  );
}

function formatTimestamp(ts: number): string {
  const date = new Date(ts);
  return date.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
}

const KINDS: ComposeKind[] = ['report_schedule', 'automation_rule', 'recipe', 'tool_chain'];
const STATUSES: ComposeStatus[] = ['pending', 'active', 'disabled'];

const EXAMPLE_SPECS: Record<ComposeKind, Record<string, any>> = {
  report_schedule: {
    name: 'Daily Backup Report',
    cron: '0 2 * * *',
    template: 'backup_summary',
    recipients: ['admin@example.com'],
  },
  automation_rule: {
    name: 'Disk Space Alert',
    trigger: { type: 'disk_usage', threshold: 85 },
    actions: [{ type: 'notify', channel: 'email' }],
  },
  recipe: {
    name: 'Deploy Web App',
    steps: [
      { type: 'build', image: 'node:20' },
      { type: 'test', command: 'npm test' },
      { type: 'deploy', target: 'kubernetes' },
    ],
  },
  tool_chain: {
    name: 'CI Pipeline',
    tools: ['git', 'docker', 'kubectl'],
    env: { REGISTRY: 'ghcr.io' },
  },
};

export function AgentsPage() {
  const [artifacts, setArtifacts] = useState<ComposeArtifact[]>([]);
  const [loading, setLoading] = useState(true);
  const [filterStatus, setFilterStatus] = useState<ComposeStatus | 'all'>('all');
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [detail, setDetail] = useState<ComposeArtifactDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editSpec, setEditSpec] = useState('');
  const [editError, setEditError] = useState<string | null>(null);
  const [editSaving, setEditSaving] = useState(false);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const [deleteConfirm, setDeleteConfirm] = useState(false);
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const [createName, setCreateName] = useState('');
  const [createKind, setCreateKind] = useState<ComposeKind>('report_schedule');
  const [createSpec, setCreateSpec] = useState(JSON.stringify(EXAMPLE_SPECS.report_schedule, null, 2));

  const loadArtifacts = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await apiListComposeArtifacts(filterStatus === 'all' ? undefined : filterStatus);
      setArtifacts(data);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load artifacts');
    } finally {
      setLoading(false);
    }
  }, [filterStatus]);

  useEffect(() => {
    loadArtifacts();
  }, [loadArtifacts]);

  const handleExpand = async (id: string) => {
    if (expandedId === id) {
      setExpandedId(null);
      setDetail(null);
      return;
    }
    setDetailLoading(true);
    try {
      const data = await apiGetComposeArtifact(id);
      setDetail(data);
      setExpandedId(id);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load artifact detail');
    } finally {
      setDetailLoading(false);
    }
  };

  const handleActivate = async (id: string) => {
    setError(null);
    try {
      await apiActivateComposeArtifact(id);
      setSuccess('Artifact activated');
      setTimeout(() => setSuccess(null), 3000);
      loadArtifacts();
      if (expandedId === id) handleExpand(id);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to activate');
    }
  };

  const handleDisable = async (id: string) => {
    setError(null);
    try {
      await apiDisableComposeArtifact(id);
      setSuccess('Artifact disabled');
      setTimeout(() => setSuccess(null), 3000);
      loadArtifacts();
      if (expandedId === id) handleExpand(id);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to disable');
    }
  };

  const handleRollback = async (id: string, version: number) => {
    setError(null);
    try {
      await apiRollbackComposeArtifact(id, version);
      setSuccess(`Rolled back to version ${version}`);
      setTimeout(() => setSuccess(null), 3000);
      loadArtifacts();
      if (expandedId === id) handleExpand(id);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to rollback');
    }
  };

  const handleDelete = async (id: string) => {
    setError(null);
    try {
      await apiDeleteComposeArtifact(id);
      setSuccess('Artifact deleted');
      setTimeout(() => setSuccess(null), 3000);
      setDeletingId(null);
      setDeleteConfirm(false);
      loadArtifacts();
      if (expandedId === id) {
        setExpandedId(null);
        setDetail(null);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to delete');
      setDeletingId(null);
      setDeleteConfirm(false);
    }
  };

  const handleEditStart = (artifact: ComposeArtifact) => {
    setEditingId(artifact.id);
    setEditSpec(JSON.stringify(artifact.spec, null, 2));
    setEditError(null);
  };

  const handleEditSave = async (artifact: ComposeArtifact) => {
    setEditError(null);
    setEditSaving(true);
    let parsed: Record<string, any>;
    try {
      parsed = JSON.parse(editSpec);
    } catch {
      setEditError('Invalid JSON');
      setEditSaving(false);
      return;
    }
    try {
      await apiUpdateComposeArtifact(artifact.id, parsed);
      setSuccess('Spec updated');
      setTimeout(() => setSuccess(null), 3000);
      setEditingId(null);
      loadArtifacts();
      if (expandedId === artifact.id) handleExpand(artifact.id);
    } catch (err) {
      setEditError(err instanceof Error ? err.message : 'Failed to update spec');
    } finally {
      setEditSaving(false);
    }
  };

  const handleEditCancel = () => {
    setEditingId(null);
    setEditError(null);
  };

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    setCreateError(null);
    setCreating(true);
    let parsed: Record<string, any>;
    try {
      parsed = JSON.parse(createSpec);
    } catch {
      setCreateError('Invalid JSON in spec');
      setCreating(false);
      return;
    }
    try {
      await apiCreateComposeArtifact({ name: createName.trim(), kind: createKind, spec: parsed });
      setSuccess('Artifact created');
      setTimeout(() => setSuccess(null), 3000);
      setCreateName('');
      setCreateKind('report_schedule');
      setCreateSpec(JSON.stringify(EXAMPLE_SPECS.report_schedule, null, 2));
      loadArtifacts();
    } catch (err) {
      setCreateError(err instanceof Error ? err.message : 'Failed to create artifact');
    } finally {
      setCreating(false);
    }
  };

  const handleKindChange = (kind: ComposeKind) => {
    setCreateKind(kind);
    setCreateSpec(JSON.stringify(EXAMPLE_SPECS[kind], null, 2));
  };

  const getStatusBadgeVariant = (status: ComposeStatus): 'success' | 'warning' | 'neutral' => {
    if (status === 'active') return 'success';
    if (status === 'pending') return 'warning';
    return 'neutral';
  };

  const canActivate = (status: ComposeStatus) => status === 'pending' || status === 'disabled';
  const canDisable = (status: ComposeStatus) => status === 'active';
  const canEdit = (status: ComposeStatus) => status === 'pending' || status === 'disabled';
  const canDelete = (status: ComposeStatus) => status === 'pending' || status === 'disabled';
  const canRollback = (status: ComposeStatus) => status !== 'active';

  const anyLoading = detailLoading || editSaving || creating || deletingId !== null;

  if (loading) {
    return (
      <div className="settings-page">
        <div className="settings-loading">Loading…</div>
      </div>
    );
  }

  return (
    <div className="settings-page">
      <div className="settings-header">
        <h1 className="settings-page-title">Agents</h1>
      </div>

      {(error || success) && (
        <div
          className={`settings-message${error ? ' settings-message--error' : ''}${success ? ' settings-message--success' : ''}`}
          role="alert"
        >
          {error || success}
        </div>
      )}

      <Section title="Compose Artifacts">
        <div className="settings-card">
          {/* Filter tabs */}
          <div className="settings-filters" style={{ display: 'flex', gap: 'var(--space-3)', marginBottom: 'var(--space-6)', flexWrap: 'wrap' }}>
            {['all', ...STATUSES].map((status) => (
              <Button
                key={status}
                variant={filterStatus === status ? 'primary' : 'secondary'}
                onClick={() => setFilterStatus(status as ComposeStatus | 'all')}
                className="filter-tab"
              >
                {status === 'all' ? 'All' : COMPOSE_STATUS_LABELS[status as ComposeStatus]}
              </Button>
            ))}
          </div>

          {/* New artifact form */}
          <div className="settings-card" style={{ marginBottom: 'var(--space-6)', padding: 'var(--space-5)' }}>
            <h3 style={{ margin: '0 0 var(--space-4)', fontSize: 'var(--text-base)', fontWeight: 600 }}>New artifact</h3>
            <form onSubmit={handleCreate} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
              {createError && (
                <div className="settings-message settings-message--error" style={{ margin: 0 }}>{createError}</div>
              )}
              <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: 'var(--space-4)' }}>
                <div className="setup-field">
                  <label htmlFor="create-name" className="setup-label">Name</label>
                  <input
                    id="create-name"
                    type="text"
                    className="setup-input"
                    value={createName}
                    onChange={(e) => setCreateName(e.target.value)}
                    placeholder="My artifact"
                    required
                    disabled={creating}
                  />
                </div>
                <div className="setup-field">
                  <label htmlFor="create-kind" className="setup-label">Kind</label>
                  <select
                    id="create-kind"
                    className="setup-input"
                    value={createKind}
                    onChange={(e) => handleKindChange(e.target.value as ComposeKind)}
                    disabled={creating}
                  >
                    {KINDS.map((k) => (
                      <option key={k} value={k}>{COMPOSE_KIND_LABELS[k]}</option>
                    ))}
                  </select>
                </div>
              </div>
              <div className="setup-field">
                <label htmlFor="create-spec" className="setup-label">Spec (JSON)</label>
                <textarea
                  id="create-spec"
                  className="setup-input"
                  style={{ minHeight: '120px', fontFamily: 'var(--font-mono, monospace)', fontSize: 'var(--text-sm)', resize: 'vertical' }}
                  value={createSpec}
                  onChange={(e) => setCreateSpec(e.target.value)}
                  disabled={creating}
                />
              </div>
              <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
                <Button variant="primary" type="submit" disabled={creating || !createName.trim()}>
                  {creating ? 'Creating…' : 'Create'}
                </Button>
              </div>
            </form>
          </div>

          {/* Artifacts list */}
          {artifacts.length === 0 ? (
            <div className="settings-placeholder" style={{ padding: 'var(--space-10)', textAlign: 'center' }}>
              <p style={{ margin: '0 0 var(--space-2)', color: 'var(--text-secondary)' }}>No artifacts found</p>
              <p style={{ margin: 0, fontSize: 'var(--text-sm)', color: 'var(--text-faint)' }}>
                Artifacts are created by agents via MCP or manually using the form above.
                New artifacts start as <strong>pending</strong> and must be activated to run.
              </p>
            </div>
          ) : (
            <div className="artifacts-list" style={{ display: 'flex', flexDirection: 'column' }}>
              {artifacts.map((artifact) => (
                <div
                  key={artifact.id}
                  className="artifact-row"
                  style={{
                    display: 'flex',
                    alignItems: 'center',
                    gap: 'var(--space-4)',
                    padding: 'var(--space-4) 0',
                    borderTop: artifact.id === artifacts[0].id ? 'none' : '1px solid var(--border)',
                    cursor: expandedId === artifact.id ? 'default' : 'pointer',
                  }}
                  onClick={() => !editingId && !deletingId && handleExpand(artifact.id)}
                >
                  <div style={{ flex: 1, minWidth: 0 }}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)', flexWrap: 'wrap', marginBottom: 'var(--space-1)' }}>
                      <span className="artifact-name" style={{ fontWeight: 550, fontSize: 'var(--text-base)' }}>{artifact.name}</span>
                      <Badge variant="neutral">{COMPOSE_KIND_LABELS[artifact.kind]}</Badge>
                      <Badge variant={getStatusBadgeVariant(artifact.status)}>{COMPOSE_STATUS_LABELS[artifact.status]}</Badge>
                      <span className="num" style={{ fontSize: 'var(--text-sm)', color: 'var(--text-secondary)' }}>v{artifact.current_version}</span>
                    </div>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-4)', fontSize: 'var(--text-sm)', color: 'var(--text-secondary)', flexWrap: 'wrap' }}>
                      <span>by {artifact.created_by}</span>
                      <span>{formatTimestamp(artifact.updated_at)}</span>
                    </div>
                  </div>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)', flexWrap: 'wrap' }}>
                    {canActivate(artifact.status) && (
                      <Button
                        variant="primary"
                        onClick={(e) => { e.stopPropagation(); handleActivate(artifact.id); }}
                        disabled={anyLoading}
                      >
                        Activate
                      </Button>
                    )}
                    {canDisable(artifact.status) && (
                      <Button
                        variant="secondary"
                        onClick={(e) => { e.stopPropagation(); handleDisable(artifact.id); }}
                        disabled={anyLoading}
                      >
                        Disable
                      </Button>
                    )}
                    {canEdit(artifact.status) && (
                      <Button
                        variant="secondary"
                        onClick={(e) => { e.stopPropagation(); handleEditStart(artifact); }}
                        disabled={anyLoading}
                      >
                        Edit spec
                      </Button>
                    )}
                    {canDelete(artifact.status) && !deletingId && (
                      <Button
                        variant="ghost"
                        onClick={(e) => { e.stopPropagation(); setDeletingId(artifact.id); setDeleteConfirm(false); }}
                        disabled={anyLoading}
                      >
                        Delete
                      </Button>
                    )}
                    {deletingId === artifact.id && !deleteConfirm && (
                      <>
                        <Button variant="ghost" onClick={() => { setDeleteConfirm(true); }} disabled={anyLoading}>Confirm delete?</Button>
                        <Button variant="ghost" onClick={() => { setDeletingId(null); }} disabled={anyLoading}>Cancel</Button>
                      </>
                    )}
                    {deletingId === artifact.id && deleteConfirm && (
                      <>
                        <Button variant="primary" onClick={() => handleDelete(artifact.id)} disabled={anyLoading}>Delete</Button>
                        <Button variant="ghost" onClick={() => { setDeletingId(null); setDeleteConfirm(false); }} disabled={anyLoading}>Cancel</Button>
                      </>
                    )}
                    {expandedId === artifact.id && (
                      <Button variant="ghost" onClick={(e) => { e.stopPropagation(); setExpandedId(null); setDetail(null); }} disabled={anyLoading}>Collapse</Button>
                    )}
                    {expandedId !== artifact.id && !editingId && !deletingId && (
                      <Button variant="ghost" onClick={(e) => { e.stopPropagation(); handleExpand(artifact.id); }} disabled={anyLoading}>Expand</Button>
                    )}
                  </div>
                </div>
              ))}
            </div>
          )}

          {/* Expanded detail */}
          {expandedId && detail && (
            <div className="artifact-detail" style={{ marginTop: 'var(--space-6)', paddingTop: 'var(--space-6)', borderTop: '1px solid var(--border)' }}>
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--space-4)' }}>
                <h3 style={{ margin: 0, fontSize: 'var(--text-lg)', fontWeight: 600 }}>{detail.name} <span className="num" style={{ fontWeight: 400, color: 'var(--text-secondary)' }}>v{detail.current_version}</span></h3>
                <Button variant="ghost" onClick={() => { setExpandedId(null); setDetail(null); }} disabled={anyLoading}>Close</Button>
              </div>

              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 'var(--space-6)' }}>
                {/* Spec */}
                <div>
                  <h4 style={{ margin: '0 0 var(--space-3)', fontSize: 'var(--text-sm)', fontWeight: 600, color: 'var(--text-secondary)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-label)' }}>Current Spec</h4>
                  <pre style={{ background: 'var(--surface-subtle)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', padding: 'var(--space-4)', overflow: 'auto', fontSize: 'var(--text-xs)', lineHeight: '1.6', maxHeight: '400px' }}>
                    {JSON.stringify(detail.spec, null, 2)}
                  </pre>
                </div>

                {/* Versions */}
                <div>
                  <h4 style={{ margin: '0 0 var(--space-3)', fontSize: 'var(--text-sm)', fontWeight: 600, color: 'var(--text-secondary)', textTransform: 'uppercase', letterSpacing: 'var(--tracking-label)' }}>Versions</h4>
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)', maxHeight: '400px', overflow: 'auto' }}>
                    {detail.versions.map((v) => (
                      <div key={v.version} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 'var(--space-3)', padding: 'var(--space-3)', background: 'var(--surface-subtle)', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)' }}>
                        <div style={{ display: 'flex', flexDirection: 'column', gap: 2, minWidth: 0 }}>
                          <span style={{ fontWeight: 550, fontSize: 'var(--text-sm)' }}>Version {v.version}</span>
                          <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-secondary)' }}>by {v.created_by} · {formatTimestamp(v.created_at)}</span>
                        </div>
                        {canRollback(detail.status) && (
                          <Button
                            variant="secondary"
                            onClick={() => handleRollback(detail.id, v.version)}
                            disabled={anyLoading || v.version === detail.current_version}
                          >
                            Rollback
                          </Button>
                        )}
                      </div>
                    ))}
                  </div>
                </div>
              </div>
            </div>
          )}

          {/* Edit spec inline */}
          {editingId && artifacts.find((a) => a.id === editingId) && (
            <div className="artifact-edit" style={{ marginTop: 'var(--space-6)', paddingTop: 'var(--space-6)', borderTop: '1px solid var(--border)' }}>
              <h3 style={{ margin: '0 0 var(--space-4)', fontSize: 'var(--text-lg)', fontWeight: 600 }}>Edit Spec</h3>
              {editError && <div className="settings-message settings-message--error" style={{ marginBottom: 'var(--space-4)' }}>{editError}</div>}
              <textarea
                className="setup-input"
                style={{ minHeight: '200px', fontFamily: 'var(--font-mono, monospace)', fontSize: 'var(--text-sm)', resize: 'vertical', marginBottom: 'var(--space-4)' }}
                value={editSpec}
                onChange={(e) => setEditSpec(e.target.value)}
                disabled={editSaving}
              />
              <div style={{ display: 'flex', gap: 'var(--space-3)', justifyContent: 'flex-end' }}>
                <Button variant="primary" onClick={() => handleEditSave(artifacts.find((a) => a.id === editingId)!)} disabled={editSaving}>
                  {editSaving ? 'Saving…' : 'Save'}
                </Button>
                <Button variant="ghost" onClick={handleEditCancel} disabled={editSaving}>Cancel</Button>
              </div>
            </div>
          )}
        </div>
      </Section>
    </div>
  );
}