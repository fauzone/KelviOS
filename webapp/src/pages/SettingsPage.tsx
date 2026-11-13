import { useState, useEffect, type ReactNode } from 'react';
import {
  apiGetAgentModes,
  apiSetAgentModes,
  apiGetSystemSettings,
  apiSetSystemSettings,
  type AgentPreset,
  switchesForPreset,
  presetForSwitches,
} from '../api';
import { t, type Lang, getSupportedLangs, onLangChange } from '../i18n';

function Section({ title, children, className = '' }: { title: string; children: ReactNode; className?: string }) {
  return (
    <section className={`settings-section ${className}`}>
      <h2 className="settings-section-title">{title}</h2>
      {children}
    </section>
  );
}

function RadioOption({
  preset,
  selected,
  onSelect,
  disabled,
}: {
  preset: AgentPreset;
  selected: boolean;
  onSelect: () => void;
  disabled?: boolean;
}) {
  return (
    <label className={`radio-option${selected ? ' radio-option--selected' : ''}${disabled ? ' radio-option--disabled' : ''}`}>
      <input
        type="radio"
        name="agent-preset"
        checked={selected}
        onChange={onSelect}
        disabled={disabled}
        aria-label={t(`settings.agents.preset.${preset}` as any)}
      />
      <div className="radio-option-content">
        <span className="radio-option-label">{t(`settings.agents.preset.${preset}` as any)}</span>
        <span className="radio-option-desc">{t(`settings.agents.presetDesc.${preset}` as any)}</span>
      </div>
      <span className="radio-option-indicator" aria-hidden="true">
        <span className="radio-option-dot" />
      </span>
    </label>
  );
}

function SwitchRow({
  label,
  description,
  checked,
  onChange,
  disabled,
}: {
  label: string;
  description: string;
  checked: boolean;
  onChange: (checked: boolean) => void;
  disabled?: boolean;
}) {
  return (
    <label className={`switch-row${disabled ? ' switch-row--disabled' : ''}`}>
      <div className="switch-row-info">
        <span className="switch-row-label">{label}</span>
        <span className="switch-row-desc">{description}</span>
      </div>
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        className={`switch${checked ? ' switch--on' : ''}${disabled ? ' switch--disabled' : ''}`}
        onClick={() => !disabled && onChange(!checked)}
        disabled={disabled}
        aria-disabled={disabled}
      >
        <span className="switch-thumb" aria-hidden="true" />
      </button>
    </label>
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
  onClick?: () => void;
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

function Select({
  value,
  onChange,
  options,
  disabled,
  className = '',
}: {
  value: string;
  onChange: (value: string) => void;
  options: { value: string; label: string }[];
  disabled?: boolean;
  className?: string;
}) {
  return (
    <select
      value={value}
      onChange={(e: React.ChangeEvent<HTMLSelectElement>) => onChange(e.target.value)}
      disabled={disabled}
      className={`setup-input ${className}`}
    >
      {options.map((opt) => (
        <option key={opt.value} value={opt.value}>
          {opt.label}
        </option>
      ))}
    </select>
  );
}

const PRESETS: AgentPreset[] = ['agent_inside', 'managed_outside', 'both', 'no_agent'];

export function SettingsPage() {
  const [modes, setModes] = useState<{ resident_harness: boolean; external_mcp: boolean; preset: AgentPreset } | null>(null);
  const [selectedPreset, setSelectedPreset] = useState<AgentPreset>('agent_inside');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState(false);

  // System settings state
  const [systemSettings, setSystemSettings] = useState<{ language: Lang; timezone: string } | null>(null);
  const [systemSaving, setSystemSaving] = useState(false);
  const [systemError, setSystemError] = useState<string | null>(null);
  const [systemSuccess, setSystemSuccess] = useState(false);

  // Load current modes on mount
  useEffect(() => {
    let mounted = true;
    apiGetAgentModes()
      .then((data) => {
        if (mounted) {
          setModes(data);
          setSelectedPreset(data.preset);
        }
      })
      .catch((err) => {
        if (mounted) {
          setError(err instanceof Error ? err.message : 'Failed to load settings');
        }
      });
    return () => { mounted = false; };
  }, []);

  // Load system settings on mount
  useEffect(() => {
    let mounted = true;
    apiGetSystemSettings()
      .then((data) => {
        if (mounted) {
          setSystemSettings(data);
          // Update i18n language from system settings
          import('../i18n').then(({ setLangFromSystemSettings }) => setLangFromSystemSettings(data.language));
        }
      })
      .catch((err) => {
        if (mounted) {
          setSystemError(err instanceof Error ? err.message : 'Failed to load system settings');
        }
      });
    return () => { mounted = false; };
  }, []);

  // Subscribe to language changes to re-render
  useEffect(() => {
    const unsubscribe = onLangChange(() => {
      // Force re-render
      setSystemSettings((s) => s);
    });
    return unsubscribe;
  }, []);

  // Update switches when preset changes (no-op, kept for clarity)
  useEffect(() => {
    if (modes) {
      switchesForPreset(selectedPreset);
      // We don't auto-save on preset change; user must click Save
    }
  }, [selectedPreset]);

  const handlePresetSelect = (preset: AgentPreset) => {
    setSelectedPreset(preset);
    setError(null);
    setSuccess(false);
  };

  const handleSave = async () => {
    if (!modes) return;

    const switches = switchesForPreset(selectedPreset);
    setSaving(true);
    setError(null);
    setSuccess(false);

    try {
      const updated = await apiSetAgentModes(switches);
      setModes(updated);
      setSelectedPreset(updated.preset);
      setSuccess(true);
      setTimeout(() => setSuccess(false), 3000);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to save settings');
    } finally {
      setSaving(false);
    }
  };

  const handleSystemSave = async () => {
    if (!systemSettings) return;

    setSystemSaving(true);
    setSystemError(null);
    setSystemSuccess(false);

    try {
      const updated = await apiSetSystemSettings({
        language: systemSettings.language,
        timezone: systemSettings.timezone,
      });
      setSystemSettings(updated);
      // Update i18n
      import('../i18n').then(({ setLangFromSystemSettings }) => setLangFromSystemSettings(updated.language));
      setSystemSuccess(true);
      setTimeout(() => setSystemSuccess(false), 3000);
    } catch (err) {
      setSystemError(err instanceof Error ? err.message : 'Failed to save system settings');
    } finally {
      setSystemSaving(false);
    }
  };

  if (!modes || !systemSettings) {
    return (
      <div className="settings-page">
        <div className="settings-loading">{t('common.loading')}</div>
      </div>
    );
  }

  const langOptions = getSupportedLangs().map(l => ({ value: l.code, label: l.label }));

  return (
    <div className="settings-page">
      <div className="settings-header">
        <h1 className="settings-page-title">{t('page.settings')}</h1>
      </div>

      {/* System Section - NEW */}
      <Section title={t('settings.system.title')}>
        <div className="settings-card">
          <div className="setup-field">
            <label className="setup-label">{t('settings.system.languageLabel')}</label>
            <Select
              value={systemSettings.language}
              onChange={(lang) => setSystemSettings({ ...systemSettings, language: lang as Lang })}
              options={langOptions}
              disabled={systemSaving}
            />
            <p style={{ marginTop: 'var(--space-2)', fontSize: 'var(--text-sm)', color: 'var(--text-secondary)' }}>{t('settings.system.languageDesc')}</p>
          </div>

          <div className="settings-divider" />

          <div className="setup-field">
            <label className="setup-label">{t('settings.system.timezoneLabel')}</label>
            <input
              type="text"
              className="setup-input"
              value={systemSettings.timezone}
              onChange={(e) => setSystemSettings({ ...systemSettings, timezone: e.target.value })}
              disabled={systemSaving}
              placeholder="e.g., Asia/Jakarta"
            />
            <p style={{ marginTop: 'var(--space-2)', fontSize: 'var(--text-sm)', color: 'var(--text-secondary)' }}>{t('settings.system.timezoneDesc')}</p>
          </div>

          {(systemError || systemSuccess) && (
            <div
              className={`settings-message${systemError ? ' settings-message--error' : ''}${systemSuccess ? ' settings-message--success' : ''}`}
              role="alert"
            >
              {systemError || t('settings.system.saved')}
            </div>
          )}

          <div className="settings-actions">
            <Button variant="primary" onClick={handleSystemSave} disabled={systemSaving}>
              {systemSaving ? t('settings.system.saving') : t('settings.agents.save')}
            </Button>
          </div>
        </div>
      </Section>

      {/* Agents & Automation Section */}
      <Section title={t('settings.agents.title')}>
        <div className="settings-card">
          <div className="settings-presets" role="radiogroup" aria-label={t('settings.agents.title')}>
            {PRESETS.map((preset) => (
              <RadioOption
                key={preset}
                preset={preset}
                selected={selectedPreset === preset}
                onSelect={() => handlePresetSelect(preset)}
              />
            ))}
          </div>

          <div className="settings-divider" />

          <div className="settings-switches">
            <SwitchRow
              label={t('settings.agents.residentHarnessLabel')}
              description={t('settings.agents.residentHarnessDesc')}
              checked={modes.resident_harness}
              onChange={(checked) => {
                const newPreset = presetForSwitches(checked, modes.external_mcp);
                setSelectedPreset(newPreset);
              }}
            />
            <SwitchRow
              label={t('settings.agents.externalMcpLabel')}
              description={t('settings.agents.externalMcpDesc')}
              checked={modes.external_mcp}
              onChange={(checked) => {
                const newPreset = presetForSwitches(modes.resident_harness, checked);
                setSelectedPreset(newPreset);
              }}
            />
          </div>

          <p className="settings-note">{t('settings.agents.note')}</p>

          {(error || success) && (
            <div
              className={`settings-message${error ? ' settings-message--error' : ''}${success ? ' settings-message--success' : ''}`}
              role="alert"
            >
              {error || t('settings.agents.save')}
            </div>
          )}

          <div className="settings-actions">
            <Button variant="primary" onClick={handleSave} disabled={saving}>
              {saving ? t('settings.agents.saving') : t('settings.agents.save')}
            </Button>
          </div>
        </div>
      </Section>

      {/* Placeholder for other settings sections */}
      <Section title={t('nav.storage')}>
        <div className="settings-card settings-placeholder">
          <p>{t('nav.storage')} settings — coming soon</p>
        </div>
      </Section>

      <Section title={t('nav.files')}>
        <div className="settings-card settings-placeholder">
          <p>{t('nav.files')} settings — coming soon</p>
        </div>
      </Section>

      <Section title={t('nav.activity')}>
        <div className="settings-card settings-placeholder">
          <p>{t('nav.activity')} settings — coming soon</p>
        </div>
      </Section>

      <Section title={t('nav.approvals')}>
        <div className="settings-card settings-placeholder">
          <p>{t('nav.approvals')} settings — coming soon</p>
        </div>
      </Section>

      <Section title={t('nav.apps')}>
        <div className="settings-card settings-placeholder">
          <p>{t('nav.apps')} settings — coming soon</p>
        </div>
      </Section>

      <Section title={t('nav.backup')}>
        <div className="settings-card settings-placeholder">
          <p>{t('nav.backup')} settings — coming soon</p>
        </div>
      </Section>

      <Section title={t('nav.tv')}>
        <div className="settings-card settings-placeholder">
          <p>{t('nav.tv')} settings — coming soon</p>
        </div>
      </Section>

      <Section title={t('settings.agents.title')}>
        <div className="settings-card settings-placeholder">
          <p>Users & Security — coming soon</p>
        </div>
      </Section>

      <Section title="About">
        <div className="settings-card settings-placeholder">
          <p>KelviOS version info — coming soon</p>
        </div>
      </Section>
    </div>
  );
}