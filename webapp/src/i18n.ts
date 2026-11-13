/**
 * KelviOS i18n — minimal, dependency-free internationalization.
 * Active language resolution order (highest priority first):
 *   1. Authenticated GET /api/settings/system → language
 *   2. Setup wizard state (first-boot, pre-auth) → language from wizard step 1
 *   3. localStorage 'kelvios_lang' cache
 *   4. Browser navigator.language (en/id prefix match)
 *   5. 'en' (ultimate fallback)
 */

export type Lang = 'en' | 'id';

/** All translation keys used across the webapp. */
export type I18nKey =
  // App loading & general
  | 'app.loading'
  // TopBar / logout
  | 'topbar.logout'
  // Navigation (NAV_ITEMS + MOBILE_ANCHORS)
  | 'nav.home'
  | 'nav.storage'
  | 'nav.files'
  | 'nav.activity'
  | 'nav.approvals'
  | 'nav.agents'
  | 'nav.apps'
  | 'nav.backup'
  | 'nav.tv'
  | 'nav.settings'
  // Page titles (PAGE_TITLES)
  | 'page.home'
  | 'page.storage'
  | 'page.files'
  | 'page.activity'
  | 'page.approvals'
  | 'page.agents'
  | 'page.apps'
  | 'page.backup'
  | 'page.tv'
  | 'page.settings'
  // Setup Wizard — Step 1: Language
  | 'wizard.step1.title'
  | 'wizard.step1.subtitle'
  | 'wizard.step1.languageLabel'
  | 'wizard.step1.option.en'
  | 'wizard.step1.option.id'
  // Setup Wizard — Step 2: Timezone
  | 'wizard.step2.title'
  | 'wizard.step2.subtitle'
  | 'wizard.step2.timezoneLabel'
  | 'wizard.step2.searchPlaceholder'
  | 'wizard.step2.noResults'
  // Setup Wizard — Step 3: Admin Account
  | 'wizard.step3.title.create'
  | 'wizard.step3.title.login'
  | 'wizard.step3.subtitle.create'
  | 'wizard.step3.subtitle.login'
  | 'wizard.step3.usernameLabel'
  | 'wizard.step3.passwordLabel'
  | 'wizard.step3.passwordHint'
  | 'wizard.step3.submit.create'
  | 'wizard.step3.submit.login'
  | 'wizard.step3.submitting'
  // Setup Wizard — Step 4: Agent Preset
  | 'wizard.step4.title'
  | 'wizard.step4.subtitle'
  | 'wizard.step4.note'
  // Setup Wizard — Step 5: Import
  | 'wizard.step5.title'
  | 'wizard.step5.subtitle'
  | 'wizard.step5.uploadLabel'
  | 'wizard.step5.pasteLabel'
  | 'wizard.step5.fileSelected'
  | 'wizard.step5.importButton'
  | 'wizard.step5.skipButton'
  | 'wizard.step5.importing'
  | 'wizard.step5.error.invalidFormat'
  | 'wizard.step5.error.invalidVersion'
  | 'wizard.step5.error.importFailed'
  // Setup Wizard — Step 6: Complete
  | 'wizard.step6.title'
  | 'wizard.step6.subtitle'
  | 'wizard.step6.completeButton'
  | 'wizard.step6.completing'
  // Setup Wizard — Common
  | 'wizard.back'
  | 'wizard.next'
  | 'wizard.skip'
  // Settings Page — System section
  | 'settings.system.title'
  | 'settings.system.languageLabel'
  | 'settings.system.languageDesc'
  | 'settings.system.timezoneLabel'
  | 'settings.system.timezoneDesc'
  | 'settings.system.saving'
  | 'settings.system.saved'
  | 'settings.system.error'
  // Settings Page — Agents section (existing keys from PRESET_LABELS/DESCRIPTIONS)
  | 'settings.agents.title'
  | 'settings.agents.preset.agent_inside'
  | 'settings.agents.preset.managed_outside'
  | 'settings.agents.preset.both'
  | 'settings.agents.preset.no_agent'
  | 'settings.agents.presetDesc.agent_inside'
  | 'settings.agents.presetDesc.managed_outside'
  | 'settings.agents.presetDesc.both'
  | 'settings.agents.presetDesc.no_agent'
  | 'settings.agents.residentHarnessLabel'
  | 'settings.agents.residentHarnessDesc'
  | 'settings.agents.externalMcpLabel'
  | 'settings.agents.externalMcpDesc'
  | 'settings.agents.note'
  | 'settings.agents.save'
  | 'settings.agents.saving'
  // Setup Page (first-boot admin creation, legacy)
  | 'setup.title'
  | 'setup.subtitle'
  | 'setup.usernameLabel'
  | 'setup.passwordLabel'
  | 'setup.passwordHint'
  | 'setup.submit'
  | 'setup.submitting'
  // Common
  | 'common.cancel'
  | 'common.save'
  | 'common.loading'
  | 'common.error'
  | 'common.skip';

/** Translation catalog: Record<Lang, Record<I18nKey, string>> */
const catalog: Record<Lang, Record<I18nKey, string>> = {
  en: {
    // App loading & general
    'app.loading': 'Loading…',
    // TopBar / logout
    'topbar.logout': 'Log out',
    // Navigation
    'nav.home': 'Home',
    'nav.storage': 'Storage',
    'nav.files': 'Files',
    'nav.activity': 'Activity',
    'nav.approvals': 'Approvals',
    'nav.agents': 'Agents',
    'nav.apps': 'App Library',
    'nav.backup': 'Backup',
    'nav.tv': 'TV Mode',
    'nav.settings': 'Settings',
    // Page titles
    'page.home': 'Home',
    'page.storage': 'Storage',
    'page.files': 'Files',
    'page.activity': 'Activity',
    'page.approvals': 'Approvals',
    'page.agents': 'Agents',
    'page.apps': 'App Library',
    'page.backup': 'Backup',
    'page.tv': 'TV Mode',
    'page.settings': 'Settings',
    // Wizard Step 1: Language
    'wizard.step1.title': 'Welcome to KelviOS',
    'wizard.step1.subtitle': 'Choose your preferred language.',
    'wizard.step1.languageLabel': 'Language',
    'wizard.step1.option.en': 'English',
    'wizard.step1.option.id': 'Indonesian',
    // Wizard Step 2: Timezone
    'wizard.step2.title': 'Time Zone',
    'wizard.step2.subtitle': 'Select your time zone for scheduled tasks and logs.',
    'wizard.step2.timezoneLabel': 'Time zone',
    'wizard.step2.searchPlaceholder': 'Search time zones…',
    'wizard.step2.noResults': 'No matching time zones',
    // Wizard Step 3: Admin Account
    'wizard.step3.title.create': 'Create Admin Account',
    'wizard.step3.title.login': 'Sign In',
    'wizard.step3.subtitle.create': 'Set up the first admin account to access KelviOS.',
    'wizard.step3.subtitle.login': 'An admin account already exists. Please sign in.',
    'wizard.step3.usernameLabel': 'Username',
    'wizard.step3.passwordLabel': 'Password',
    'wizard.step3.passwordHint': 'Minimum 12 characters',
    'wizard.step3.submit.create': 'Create Account',
    'wizard.step3.submit.login': 'Sign In',
    'wizard.step3.submitting': 'Working…',
    // Wizard Step 4: Agent Preset
    'wizard.step4.title': 'Agent Mode',
    'wizard.step4.subtitle': 'Choose how you want to run agents. You can change this anytime in Settings.',
    'wizard.step4.note': 'Default: <strong>Agent inside</strong> — the resident agent stays asleep until an LLM provider is configured.',
    // Wizard Step 5: Import
    'wizard.step5.title': 'Import Settings (Optional)',
    'wizard.step5.subtitle': 'Restore a previous backup or skip to finish setup.',
    'wizard.step5.uploadLabel': 'Upload JSON file',
    'wizard.step5.pasteLabel': 'Or paste JSON content',
    'wizard.step5.fileSelected': 'File selected: {name}',
    'wizard.step5.importButton': 'Import',
    'wizard.step5.skipButton': 'Skip',
    'wizard.step5.importing': 'Importing…',
    'wizard.step5.error.invalidFormat': 'Invalid file format. Expected a KelviOS settings export.',
    'wizard.step5.error.invalidVersion': 'Unsupported export version.',
    'wizard.step5.error.importFailed': 'Import failed: {message}',
    // Wizard Step 6: Complete
    'wizard.step6.title': 'Setup Complete',
    'wizard.step6.subtitle': 'Your KelviOS instance is ready.',
    'wizard.step6.completeButton': 'Continue to Dashboard',
    'wizard.step6.completing': 'Finishing…',
    // Wizard Common
    'wizard.back': 'Back',
    'wizard.next': 'Next',
    'wizard.skip': 'Skip',
    // Settings System
    'settings.system.title': 'System',
    'settings.system.languageLabel': 'Language',
    'settings.system.languageDesc': 'Interface language',
    'settings.system.timezoneLabel': 'Time zone',
    'settings.system.timezoneDesc': 'Time zone for schedules and logs',
    'settings.system.saving': 'Saving…',
    'settings.system.saved': 'Settings saved',
    'settings.system.error': 'Failed to save settings',
    // Settings Agents (from PRESET_LABELS/DESCRIPTIONS)
    'settings.agents.title': 'Agents & Automation',
    'settings.agents.preset.agent_inside': 'Agent inside',
    'settings.agents.preset.managed_outside': 'Managed from outside',
    'settings.agents.preset.both': 'Both',
    'settings.agents.preset.no_agent': 'No agent',
    'settings.agents.presetDesc.agent_inside': 'Resident harness ON · External MCP OFF',
    'settings.agents.presetDesc.managed_outside': 'Resident harness OFF · External MCP ON',
    'settings.agents.presetDesc.both': 'Resident harness ON · External MCP ON',
    'settings.agents.presetDesc.no_agent': 'Resident harness OFF · External MCP OFF',
    'settings.agents.residentHarnessLabel': 'Resident harness',
    'settings.agents.residentHarnessDesc': 'Run the built-in agent harness on this device',
    'settings.agents.externalMcpLabel': 'External MCP access',
    'settings.agents.externalMcpDesc': 'Allow external MCP clients to connect to this instance',
    'settings.agents.note': 'The resident agent stays asleep until an LLM provider is configured.',
    'settings.agents.save': 'Save',
    'settings.agents.saving': 'Saving…',
    // Setup Page (legacy first-boot)
    'setup.title': 'Welcome to KelviOS',
    'setup.subtitle': 'Create your admin account to get started',
    'setup.usernameLabel': 'Username',
    'setup.passwordLabel': 'Password',
    'setup.passwordHint': 'Minimum 12 characters',
    'setup.submit': 'Create Account',
    'setup.submitting': 'Creating…',
    // Common
    'common.cancel': 'Cancel',
    'common.save': 'Save',
    'common.loading': 'Loading…',
    'common.error': 'Error',
    'common.skip': 'Skip',
  },
  id: {
    // App loading & general
    'app.loading': 'Memuat…',
    // TopBar / logout
    'topbar.logout': 'Keluar',
    // Navigation
    'nav.home': 'Beranda',
    'nav.storage': 'Penyimpanan',
    'nav.files': 'Berkas',
    'nav.activity': 'Aktivitas',
    'nav.approvals': 'Persetujuan',
    'nav.agents': 'Agen',
    'nav.apps': 'Perpustakaan Aplikasi',
    'nav.backup': 'Cadangan',
    'nav.tv': 'Mode TV',
    'nav.settings': 'Pengaturan',
    // Page titles
    'page.home': 'Beranda',
    'page.storage': 'Penyimpanan',
    'page.files': 'Berkas',
    'page.activity': 'Aktivitas',
    'page.approvals': 'Persetujuan',
    'page.agents': 'Agen',
    'page.apps': 'Perpustakaan Aplikasi',
    'page.backup': 'Cadangan',
    'page.tv': 'Mode TV',
    'page.settings': 'Pengaturan',
    // Wizard Step 1: Language
    'wizard.step1.title': 'Selamat datang di KelviOS',
    'wizard.step1.subtitle': 'Pilih bahasa yang Anda sukai.',
    'wizard.step1.languageLabel': 'Bahasa',
    'wizard.step1.option.en': 'Inggris',
    'wizard.step1.option.id': 'Indonesia',
    // Wizard Step 2: Timezone
    'wizard.step2.title': 'Zona Waktu',
    'wizard.step2.subtitle': 'Pilih zona waktu untuk tugas terjadwal dan log.',
    'wizard.step2.timezoneLabel': 'Zona waktu',
    'wizard.step2.searchPlaceholder': 'Cari zona waktu…',
    'wizard.step2.noResults': 'Tidak ada zona waktu cocok',
    // Wizard Step 3: Admin Account
    'wizard.step3.title.create': 'Buat Akun Admin',
    'wizard.step3.title.login': 'Masuk',
    'wizard.step3.subtitle.create': 'Siapkan akun admin pertama untuk mengakses KelviOS.',
    'wizard.step3.subtitle.login': 'Akun admin sudah ada. Silakan masuk.',
    'wizard.step3.usernameLabel': 'Nama pengguna',
    'wizard.step3.passwordLabel': 'Kata sandi',
    'wizard.step3.passwordHint': 'Minimal 12 karakter',
    'wizard.step3.submit.create': 'Buat Akun',
    'wizard.step3.submit.login': 'Masuk',
    'wizard.step3.submitting': 'Memproses…',
    // Wizard Step 4: Agent Preset
    'wizard.step4.title': 'Mode Agen',
    'wizard.step4.subtitle': 'Pilih cara menjalankan agen. Bisa diubah kapan saja di Pengaturan.',
    'wizard.step4.note': 'Default: <strong>Agen di dalam</strong> — agen residennya tidur sampai provider LLM dikonfigurasi.',
    // Wizard Step 5: Import
    'wizard.step5.title': 'Impor Pengaturan (Opsional)',
    'wizard.step5.subtitle': 'Pulihkan cadangan sebelumnya atau lewati untuk menyelesaikan pengaturan.',
    'wizard.step5.uploadLabel': 'Unggah berkas JSON',
    'wizard.step5.pasteLabel': 'Atau tempel isi JSON',
    'wizard.step5.fileSelected': 'Berkas dipilih: {name}',
    'wizard.step5.importButton': 'Impor',
    'wizard.step5.skipButton': 'Lewati',
    'wizard.step5.importing': 'Mengimpor…',
    'wizard.step5.error.invalidFormat': 'Format berkas tidak valid. Diharapkan ekspor pengaturan KelviOS.',
    'wizard.step5.error.invalidVersion': 'Versi ekspor tidak didukung.',
    'wizard.step5.error.importFailed': 'Impor gagal: {message}',
    // Wizard Step 6: Complete
    'wizard.step6.title': 'Pengaturan Selesai',
    'wizard.step6.subtitle': 'Instans KelviOS Anda siap digunakan.',
    'wizard.step6.completeButton': 'Lanjut ke Dasbor',
    'wizard.step6.completing': 'Menyelesaikan…',
    // Wizard Common
    'wizard.back': 'Kembali',
    'wizard.next': 'Lanjut',
    'wizard.skip': 'Lewati',
    // Settings System
    'settings.system.title': 'Sistem',
    'settings.system.languageLabel': 'Bahasa',
    'settings.system.languageDesc': 'Bahasa antarmuka',
    'settings.system.timezoneLabel': 'Zona waktu',
    'settings.system.timezoneDesc': 'Zona waktu untuk jadwal dan log',
    'settings.system.saving': 'Menyimpan…',
    'settings.system.saved': 'Pengaturan tersimpan',
    'settings.system.error': 'Gagal menyimpan pengaturan',
    // Settings Agents
    'settings.agents.title': 'Agen & Otomasi',
    'settings.agents.preset.agent_inside': 'Agen di dalam',
    'settings.agents.preset.managed_outside': 'Dikelola dari luar',
    'settings.agents.preset.both': 'Keduanya',
    'settings.agents.preset.no_agent': 'Tanpa agen',
    'settings.agents.presetDesc.agent_inside': 'Harness residennya ON · MCP eksternal OFF',
    'settings.agents.presetDesc.managed_outside': 'Harness residennya OFF · MCP eksternal ON',
    'settings.agents.presetDesc.both': 'Harness residennya ON · MCP eksternal ON',
    'settings.agents.presetDesc.no_agent': 'Harness residennya OFF · MCP eksternal OFF',
    'settings.agents.residentHarnessLabel': 'Harness residennya',
    'settings.agents.residentHarnessDesc': 'Jalankan harness agen bawaan di perangkat ini',
    'settings.agents.externalMcpLabel': 'Akses MCP eksternal',
    'settings.agents.externalMcpDesc': 'Izinkan klien MCP eksternal terhubung ke instans ini',
    'settings.agents.note': 'Agen residennya tidur sampai provider LLM dikonfigurasi.',
    'settings.agents.save': 'Simpan',
    'settings.agents.saving': 'Menyimpan…',
    // Setup Page (legacy first-boot)
    'setup.title': 'Selamat datang di KelviOS',
    'setup.subtitle': 'Buat akun admin Anda untuk memulai',
    'setup.usernameLabel': 'Nama pengguna',
    'setup.passwordLabel': 'Kata sandi',
    'setup.passwordHint': 'Minimal 12 karakter',
    'setup.submit': 'Buat Akun',
    'setup.submitting': 'Membuat…',
    // Common
    'common.cancel': 'Batal',
    'common.save': 'Simpan',
    'common.loading': 'Memuat…',
    'common.error': 'Kesalahan',
    'common.skip': 'Lewati',
  },
};

/** Current active language (reactive via listener pattern). */
let currentLang: Lang = 'en';
const listeners = new Set<() => void>();

/** Get the current active language. */
export function getLang(): Lang {
  return currentLang;
}

/** Set the active language and notify listeners. */
export function setLang(lang: Lang): void {
  if (lang !== currentLang) {
    currentLang = lang;
    localStorage.setItem('kelvios_lang', lang);
    listeners.forEach((fn) => fn());
  }
}

/** Subscribe to language changes. Returns unsubscribe function. */
export function onLangChange(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/**
 * Translate a key with fallback chain: active lang → en → key.
 * Supports simple placeholder replacement: {name} → value.
 */
export function t(key: I18nKey, params?: Record<string, string>): string {
  let str = catalog[currentLang]?.[key] ?? catalog.en[key] ?? key;
  if (params) {
    Object.entries(params).forEach(([k, v]) => {
      str = str.replace(new RegExp(`\\{${k}\\}`, 'g'), v);
    });
  }
  return str;
}

/**
 * Initialize language from cache (localStorage) — call early, before auth.
 * Does NOT fetch from /api/settings/system (requires auth).
 */
export function initLangFromCache(): void {
  const cached = localStorage.getItem('kelvios_lang') as Lang | null;
  if (cached === 'en' || cached === 'id') {
    currentLang = cached;
  } else {
    // Fallback to browser preference
    const browserLang = navigator.language.slice(0, 2).toLowerCase();
    if (browserLang === 'id') currentLang = 'id';
  }
}

/**
 * Update language from authenticated GET /api/settings/system response.
 * Call after successful auth or when settings are saved.
 */
export function setLangFromSystemSettings(language: string): void {
  if (language === 'en' || language === 'id') {
    setLang(language);
  }
}

/**
 * Update language from wizard step 1 (pre-auth, first-boot).
 * Persists to localStorage immediately.
 */
export function setLangFromWizard(language: Lang): void {
  setLang(language);
}

/** Get all supported languages for UI selectors. */
export function getSupportedLangs(): { code: Lang; label: string }[] {
  return [
    { code: 'en', label: catalog.en['wizard.step1.option.en'] },
    { code: 'id', label: catalog.id['wizard.step1.option.id'] },
  ];
}