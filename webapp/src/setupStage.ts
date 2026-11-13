/**
 * Setup stage to wizard step mapping.
 * Pure function — no side effects, no external dependencies.
 * Server is the source of truth for stage values.
 */

export type WizardStep = 1 | 2 | 3 | 4 | 5 | 6;

export type SetupStage = 'language' | 'timezone' | 'admin' | 'preset' | 'complete';

/**
 * Maps a server setup stage to a wizard step number.
 * - language → 1
 * - timezone → 2
 * - admin → 3
 * - preset → 4
 * - complete → 1 (fallback; handled by setup_complete branch in SetupWizard)
 * - any unknown string → 1 (safe fallback)
 * Never returns NaN.
 */
export function stageToStep(stage: string): WizardStep {
  switch (stage) {
    case 'language':
      return 1;
    case 'timezone':
      return 2;
    case 'admin':
      return 3;
    case 'preset':
      return 4;
    case 'complete':
      return 1; // fallback; setup_complete branch handles this in SetupWizard
    default:
      return 1; // safe fallback for any unknown stage value
  }
}