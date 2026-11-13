import { useState, useEffect, type ReactNode, type ChangeEvent } from 'react';
import {
  apiSetAgentModes,
  apiGetSetupState,
  apiSetupPreferences,
  apiSetupImport,
  apiSetupComplete,
  apiSetup,
  apiLogin,
  type AgentPreset,
  switchesForPreset,
} from '../api';
import { stageToStep, type WizardStep } from '../setupStage';
import { t, type Lang, getSupportedLangs } from '../i18n';

interface SetupWizardProps {
  onComplete: () => void;
}

const PRESETS: AgentPreset[] = ['agent_inside', 'managed_outside', 'both', 'no_agent'];

// Static fallback timezones (IANA) if Intl.supportedValuesOf is unavailable
const FALLBACK_TIMEZONES = [
  'UTC',
  'Asia/Jakarta',
  'Asia/Singapore',
  'Asia/Kuala_Lumpur',
  'Asia/Bangkok',
  'Asia/Ho_Chi_Minh',
  'Asia/Manila',
  'Asia/Tokyo',
  'Asia/Seoul',
  'Asia/Shanghai',
  'Asia/Hong_Kong',
  'Asia/Taipei',
  'Asia/Dubai',
  'Asia/Riyadh',
  'Asia/Tehran',
  'Asia/Kolkata',
  'Europe/London',
  'Europe/Paris',
  'Europe/Berlin',
  'Europe/Rome',
  'Europe/Madrid',
  'Europe/Amsterdam',
  'Europe/Stockholm',
  'Europe/Oslo',
  'Europe/Copenhagen',
  'Europe/Warsaw',
  'Europe/Moscow',
  'Europe/Istanbul',
  'America/New_York',
  'America/Chicago',
  'America/Denver',
  'America/Los_Angeles',
  'America/Anchorage',
  'America/Honolulu',
  'America/Sao_Paulo',
  'America/Argentina/Buenos_Aires',
  'Australia/Sydney',
  'Australia/Melbourne',
  'Australia/Brisbane',
  'Australia/Perth',
  'Australia/Adelaide',
  'Pacific/Auckland',
  'Pacific/Fiji',
];

function getTimezones(): string[] {
  try {
    return Intl.supportedValuesOf('timeZone');
  } catch {
    return FALLBACK_TIMEZONES;
  }
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

function LanguageSelect({
  value,
  onChange,
  disabled,
}: {
  value: Lang;
  onChange: (lang: Lang) => void;
  disabled?: boolean;
}) {
  const langs = getSupportedLangs();
  return (
    <select
      value={value}
      onChange={(e: ChangeEvent<HTMLSelectElement>) => onChange(e.target.value as Lang)}
      disabled={disabled}
      className="setup-input"
      aria-label={t('wizard.step1.languageLabel')}
    >
      {langs.map((l) => (
        <option key={l.code} value={l.code}>
          {l.label}
        </option>
      ))}
    </select>
  );
}

function TimezoneSelect({
  value,
  onChange,
  disabled,
  searchQuery,
  onSearchChange,
}: {
  value: string;
  onChange: (tz: string) => void;
  disabled?: boolean;
  searchQuery: string;
  onSearchChange: (q: string) => void;
}) {
  const timezones = getTimezones();
  const filtered = timezones.filter((tz) =>
    tz.toLowerCase().includes(searchQuery.toLowerCase())
  );

  return (
    <div className="setup-field">
      <input
        type="text"
        value={searchQuery}
        onChange={(e: ChangeEvent<HTMLInputElement>) => onSearchChange(e.target.value)}
        placeholder={t('wizard.step2.searchPlaceholder')}
        className="setup-input"
        aria-label={t('wizard.step2.searchPlaceholder')}
        disabled={disabled}
      />
      <select
        value={value}
        onChange={(e: ChangeEvent<HTMLSelectElement>) => onChange(e.target.value)}
        disabled={disabled}
        className="setup-input"
        aria-label={t('wizard.step2.timezoneLabel')}
      >
        {filtered.length === 0 ? (
          <option value="" disabled>
            {t('wizard.step2.noResults')}
          </option>
        ) : (
          filtered.map((tz) => (
            <option key={tz} value={tz}>
              {tz}
            </option>
          ))
        )}
      </select>
    </div>
  );
}

function FileUpload({
  onFile,
  onPaste,
  disabled,
}: {
  onFile: (file: File) => void;
  onPaste: (text: string) => void;
  disabled?: boolean;
}) {
  const [dragActive, setDragActive] = useState(false);
  const fileInputRef = useState<HTMLInputElement | null>(null);

  const handleDrag = (e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    if (e.type === 'dragenter' || e.type === 'dragover') {
      setDragActive(true);
    } else if (e.type === 'dragleave') {
      setDragActive(false);
    }
  };

  const handleDrop = (e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    setDragActive(false);
    if (e.dataTransfer.files.length > 0) {
      onFile(e.dataTransfer.files[0]);
    }
  };

  const handleFileChange = (e: ChangeEvent<HTMLInputElement>) => {
    if (e.target.files && e.target.files.length > 0) {
      onFile(e.target.files[0]);
    }
  };

  const handlePaste = (e: React.ClipboardEvent<HTMLTextAreaElement>) => {
    const text = e.clipboardData.getData('text');
    if (text.trim()) {
      onPaste(text);
    }
  };

  return (
    <div className="setup-field">
      <div
        className={`setup-field${dragActive ? ' setup-field--drag-active' : ''}`}
        onDragEnter={handleDrag}
        onDragOver={handleDrag}
        onDragLeave={handleDrag}
        onDrop={handleDrop}
      >
        <input
          type="file"
          accept=".json"
          onChange={handleFileChange}
          disabled={disabled}
          ref={fileInputRef as any}
          className="setup-input"
          id="import-file"
          aria-label={t('wizard.step5.uploadLabel')}
          style={{ display: 'none' }}
        />
        <label htmlFor="import-file" className="setup-label" style={{ cursor: 'pointer', display: 'block' }}>
          <span style={{ display: 'block', textAlign: 'center', padding: 'var(--space-6)', border: '2px dashed var(--border)', borderRadius: 'var(--radius)', background: dragActive ? 'var(--accent-subtle)' : 'var(--surface-subtle)', transition: 'all 120ms ease' }}>
            <span style={{ fontSize: '2rem', display: 'block', marginBottom: 'var(--space-2)' }}>📁</span>
            <span style={{ display: 'block', fontWeight: 550, marginBottom: 'var(--space-1)' }}>{t('wizard.step5.uploadLabel')}</span>
            <span style={{ fontSize: 'var(--text-sm)', color: 'var(--text-secondary)' }}>{t('wizard.step5.pasteLabel')}</span>
          </span>
        </label>
      </div>
      <textarea
        placeholder={t('wizard.step5.pasteLabel')}
        onPaste={handlePaste}
        disabled={disabled}
        className="setup-input"
        aria-label={t('wizard.step5.pasteLabel')}
        rows={8}
        spellCheck={false}
        style={{ fontFamily: 'inherit', resize: 'vertical' }}
      />
    </div>
  );
}

export function SetupWizard({ onComplete }: SetupWizardProps) {
  const [step, setStep] = useState<WizardStep>(1);
  const [language, setLanguage] = useState<Lang>('en');
  const [timezone, setTimezone] = useState('');
  const [timezoneSearch, setTimezoneSearch] = useState('');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [selectedPreset, setSelectedPreset] = useState<AgentPreset>('agent_inside');
  const [userInteracted, setUserInteracted] = useState(false);
  const [importFile, setImportFile] = useState<File | null>(null);
  const [importText, setImportText] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [hasAdmin, setHasAdmin] = useState(false);
  const [setupComplete, setSetupComplete] = useState(false);

  // Load initial setup state on mount
  useEffect(() => {
    let mounted = true;
    apiGetSetupState()
      .then((state) => {
        if (mounted) {
          setHasAdmin(state.has_admin);
          if (state.language_set) setLanguage(state.language_set ? (language === 'id' ? 'id' : 'en') : 'en');
          if (state.timezone_set) setTimezone(state.timezone_set ? timezone : '');
          setStep(stageToStep(state.stage));
          if (state.setup_complete) {
            setSetupComplete(true);
            onComplete();
          }
        }
      })
      .catch((err) => {
        if (mounted) {
          setError(err instanceof Error ? err.message : 'Failed to load setup state');
        }
      });
    return () => { mounted = false; };
  }, []);

  // Persist language to i18n when changed in step 1
  useEffect(() => {
    if (step === 1) {
      import('../i18n').then(({ setLangFromWizard }) => setLangFromWizard(language));
    }
  }, [language, step]);

  // Auto-detect timezone on step 2 mount
  useEffect(() => {
    if (step === 2 && !timezone) {
      try {
        const detected = Intl.DateTimeFormat().resolvedOptions().timeZone;
        if (getTimezones().includes(detected)) {
          setTimezone(detected);
        }
      } catch {
        // ignore
      }
    }
  }, [step]);

  const handleBack = () => {
    if (step > 1) setStep((step - 1) as WizardStep);
  };

  const handleNext = async () => {
    setError(null);
    setSaving(true);

    try {
      switch (step) {
        case 1: {
          await apiSetupPreferences({ language });
          setStep(2);
          break;
        }
        case 2: {
          if (!timezone) {
            setError('Please select a time zone');
            setSaving(false);
            return;
          }
          await apiSetupPreferences({ timezone });
          setStep(3);
          break;
        }
        case 3: {
          if (!username.trim() || !password) {
            setError('Username and password are required');
            setSaving(false);
            return;
          }
          if (password.length < 12) {
            setError('Password must be at least 12 characters');
            setSaving(false);
            return;
          }
          if (hasAdmin) {
            const loginRes = await apiLogin({ username: username.trim(), password });
            localStorage.setItem('kelvios_token', loginRes.token);
          } else {
            await apiSetup({ username: username.trim(), password });
            const loginRes = await apiLogin({ username: username.trim(), password });
            localStorage.setItem('kelvios_token', loginRes.token);
          }
          setStep(4);
          break;
        }
        case 4: {
          const switches = switchesForPreset(selectedPreset);
          const isDefault = selectedPreset === 'agent_inside' && !userInteracted;
          if (!isDefault) {
            await apiSetAgentModes(switches);
          }
          setStep(5);
          break;
        }
        case 5: {
          if (importFile) {
            const text = await importFile.text();
            const data = JSON.parse(text);
            await apiSetupImport(data);
          } else if (importText.trim()) {
            const data = JSON.parse(importText);
            await apiSetupImport(data);
          }
          setStep(6);
          break;
        }
        case 6: {
          await apiSetupComplete();
          onComplete();
          break;
        }
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Operation failed');
    } finally {
      setSaving(false);
    }
  };

  const handleSkip = () => {
    if (step === 5) {
      setStep(6);
    } else if (step < 6) {
      setStep((step + 1) as WizardStep);
    }
  };

  const handleImportFile = (file: File) => {
    setImportFile(file);
    setImportText('');
    setError(null);
  };

  const handleImportPaste = (text: string) => {
    setImportText(text);
    setImportFile(null);
    setError(null);
  };

  const canGoNext = () => {
    switch (step) {
      case 1:
        return true;
      case 2:
        return !!timezone;
      case 3:
        return username.trim().length > 0 && password.length >= 12;
      case 4:
        return true;
      case 5:
        return true; // optional
      case 6:
        return true;
      default:
        return false;
    }
  };

  const renderStep = () => {
    switch (step) {
      case 1:
        return (
          <>
            <div className="setup-wizard-header">
              <h1 className="setup-wizard-title">{t('wizard.step1.title')}</h1>
              <p className="setup-wizard-subtitle">{t('wizard.step1.subtitle')}</p>
            </div>
            <div className="setup-field">
              <label className="setup-label">{t('wizard.step1.languageLabel')}</label>
              <LanguageSelect value={language} onChange={setLanguage} disabled={saving} />
            </div>
          </>
        );
      case 2:
        return (
          <>
            <div className="setup-wizard-header">
              <h1 className="setup-wizard-title">{t('wizard.step2.title')}</h1>
              <p className="setup-wizard-subtitle">{t('wizard.step2.subtitle')}</p>
            </div>
            <div className="setup-field">
              <label className="setup-label">{t('wizard.step2.timezoneLabel')}</label>
              <TimezoneSelect
                value={timezone}
                onChange={setTimezone}
                disabled={saving}
                searchQuery={timezoneSearch}
                onSearchChange={setTimezoneSearch}
              />
            </div>
          </>
        );
      case 3:
        return (
          <>
            <div className="setup-wizard-header">
              <h1 className="setup-wizard-title">
                {hasAdmin ? t('wizard.step3.title.login') : t('wizard.step3.title.create')}
              </h1>
              <p className="setup-wizard-subtitle">
                {hasAdmin ? t('wizard.step3.subtitle.login') : t('wizard.step3.subtitle.create')}
              </p>
            </div>
            <div className="setup-field">
              <label htmlFor="wizard-username" className="setup-label">
                {t('wizard.step3.usernameLabel')}
              </label>
              <input
                id="wizard-username"
                type="text"
                className="setup-input"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                required
                autoComplete="username"
                disabled={saving}
              />
            </div>
            <div className="setup-field">
              <label htmlFor="wizard-password" className="setup-label">
                {t('wizard.step3.passwordLabel')}
              </label>
              <input
                id="wizard-password"
                type="password"
                className="setup-input"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
                minLength={12}
                autoComplete={hasAdmin ? 'current-password' : 'new-password'}
                disabled={saving}
              />
              <p className="setup-hint" style={{ marginTop: 'var(--space-2)', fontSize: 'var(--text-sm)', color: 'var(--text-secondary)' }}>{t('wizard.step3.passwordHint')}</p>
            </div>
          </>
        );
      case 4:
        return (
          <>
            <div className="setup-wizard-header">
              <h1 className="setup-wizard-title">{t('wizard.step4.title')}</h1>
              <p className="setup-wizard-subtitle">{t('wizard.step4.subtitle')}</p>
            </div>
            <div className="setup-wizard-options" role="radiogroup" aria-label={t('wizard.step4.title')}>
              {PRESETS.map((preset) => (
                <RadioOption
                  key={preset}
                  preset={preset}
                  selected={selectedPreset === preset}
                  onSelect={() => {
                    handlePresetSelect(preset);
                  }}
                />
              ))}
            </div>
            <p className="setup-wizard-note" dangerouslySetInnerHTML={{ __html: t('wizard.step4.note') }} />
          </>
        );
      case 5:
        return (
          <>
            <div className="setup-wizard-header">
              <h1 className="setup-wizard-title">{t('wizard.step5.title')}</h1>
              <p className="setup-wizard-subtitle">{t('wizard.step5.subtitle')}</p>
            </div>
            <FileUpload
              onFile={handleImportFile}
              onPaste={handleImportPaste}
              disabled={saving}
            />
            {(importFile || importText) && (
              <p style={{ marginTop: 'var(--space-3)', fontSize: 'var(--text-sm)', color: 'var(--text-secondary)', textAlign: 'center' }}>
                {importFile
                  ? t('wizard.step5.fileSelected', { name: importFile.name })
                  : t('wizard.step5.fileSelected', { name: 'pasted content' })}
              </p>
            )}
          </>
        );
      case 6:
        return (
          <>
            <div className="setup-wizard-header">
              <h1 className="setup-wizard-title">{t('wizard.step6.title')}</h1>
              <p className="setup-wizard-subtitle">{t('wizard.step6.subtitle')}</p>
            </div>
            <div style={{ textAlign: 'center', fontSize: '4rem', color: 'var(--ok)', marginBottom: 'var(--space-4)' }} aria-hidden="true">✓</div>
          </>
        );
      default:
        return null;
    }
  };

  const handlePresetSelect = (preset: AgentPreset) => {
    setSelectedPreset(preset);
    setUserInteracted(true);
    setError(null);
  };

  if (setupComplete) {
    return null;
  }

  return (
    <div className="setup-wizard">
      <div className="setup-wizard-card">
        {error && (
          <div className="setup-wizard-error" role="alert">
            {error}
          </div>
        )}

        <div style={{ display: 'flex', justifyContent: 'center', gap: 'var(--space-2)', marginBottom: 'var(--space-8)' }} aria-hidden="true">
          {[1, 2, 3, 4, 5, 6].map((s) => (
            <span
              key={s}
              style={{
                width: '10px',
                height: '10px',
                borderRadius: '50%',
                background: s < step ? 'var(--ok)' : s === step ? 'var(--accent)' : 'var(--border-strong)',
                border: s === step ? '2px solid var(--accent)' : 'none',
                transition: 'all 200ms ease',
              }}
            />
          ))}
        </div>

        {renderStep()}

        <div className="setup-wizard-actions">
          {step > 1 && (
            <Button variant="ghost" onClick={handleBack} disabled={saving}>
              {t('wizard.back')}
            </Button>
          )}
          {step < 6 ? (
            <>
              {step === 5 && (
                <Button variant="ghost" onClick={handleSkip} disabled={saving}>
                  {t('wizard.skip')}
                </Button>
              )}
              <Button variant="primary" onClick={handleNext} disabled={saving || !canGoNext()}>
                {saving ? t('wizard.step3.submitting') : step === 6 ? t('wizard.step6.completing') : t('wizard.next')}
              </Button>
            </>
          ) : (
            <Button variant="primary" onClick={handleNext} disabled={saving}>
              {saving ? t('wizard.step6.completing') : t('wizard.step6.completeButton')}
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}