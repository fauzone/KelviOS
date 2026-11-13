#!/usr/bin/env node
/**
 * Setup Stage Contract Check
 * 
 * Verifies:
 * 1. stageToStep mapping matches expected values for all server stages + unknown
 * 2. Server stage literals in internal/httpserver/setup.go match expected set
 * 
 * No external dependencies — pure Node.js.
 */

import fs from 'fs';
import path from 'path';

// ============================================================
// 1. Load and test stageToStep from setupStage.ts
// ============================================================

const setupStagePath = path.resolve('webapp/src/setupStage.ts');
if (!fs.existsSync(setupStagePath)) {
  console.error('FAIL: webapp/src/setupStage.ts not found');
  process.exit(1);
}

const setupStageContent = fs.readFileSync(setupStagePath, 'utf-8');

// Extract the switch cases from stageToStep function
const switchMatch = setupStageContent.match(/switch \(stage\) \{([\s\S]*?)\n\}/);
if (!switchMatch) {
  console.error('FAIL: Could not find switch statement in stageToStep function');
  process.exit(1);
}

const switchBody = switchMatch[1];

// Parse case statements
const caseRegex = /case\s+['"]([^'"]+)['"]\s*:\s*return\s+(\d+)\s*;/g;
const caseMap = new Map();
let match;
while ((match = caseRegex.exec(switchBody)) !== null) {
  caseMap.set(match[1], parseInt(match[2], 10));
}

// Find default case
const defaultMatch = switchBody.match(/default\s*:\s*return\s+(\d+)\s*;/);
const defaultValue = defaultMatch ? parseInt(defaultMatch[1], 10) : 1;

// Implement stageToStep in JS
function stageToStep(stage) {
  if (caseMap.has(stage)) {
    return caseMap.get(stage);
  }
  return defaultValue;
}

// Expected mappings
const expectedMappings = [
  { stage: 'language', expected: 1 },
  { stage: 'timezone', expected: 2 },
  { stage: 'admin', expected: 3 },
  { stage: 'preset', expected: 4 },
  { stage: 'complete', expected: 1 }, // fallback
  { stage: 'unknown_stage_xyz', expected: 1 }, // fallback for unknown
];

console.log('=== Testing stageToStep mappings ===');
let allPassed = true;
for (const { stage, expected } of expectedMappings) {
  const result = stageToStep(stage);
  const passed = result === expected;
  console.log(`  stageToStep('${stage}') = ${result} ${passed ? '✓' : '✗ EXPECTED ' + expected}`);
  if (!passed) allPassed = false;
}

if (!allPassed) {
  console.error('\nFAIL: stageToStep mappings do not match expected values');
  process.exit(1);
}
console.log('All stageToStep mappings PASSED\n');

// ============================================================
// 2. Cross-check server stage literals in setup.go
// ============================================================

const setupGoPath = path.resolve('internal/httpserver/setup.go');
if (!fs.existsSync(setupGoPath)) {
  console.error('FAIL: internal/httpserver/setup.go not found');
  process.exit(1);
}

const setupGoContent = fs.readFileSync(setupGoPath, 'utf-8');

// Extract stage literals ONLY from computeSetupStage function
// Find the computeSetupStage function body first
const funcMatch = setupGoContent.match(/func \(s \*Server\) computeSetupStage\([^)]+\) string \{([\s\S]*?)\n\}/);
if (!funcMatch) {
  console.error('FAIL: Could not find computeSetupStage function in setup.go');
  process.exit(1);
}

const funcBody = funcMatch[1];

// Extract return statements with string literals from within this function
const stageLiterals = [];
const returnMatches = funcBody.matchAll(/return\s+"([^"]+)"/g);
for (const m of returnMatches) {
  stageLiterals.push(m[1]);
}

// Deduplicate while preserving order
const uniqueServerStages = [...new Set(stageLiterals)];

console.log('=== Server stage literals from setup.go (computeSetupStage) ===');
console.log('  Found:', uniqueServerStages.join(', '));

// Expected server stages (from computeSetupStage)
const expectedServerStages = ['complete', 'language', 'timezone', 'admin', 'preset'];

console.log('  Expected:', expectedServerStages.join(', '));

// Check if sets match (order doesn't matter for set equality)
const serverSet = new Set(uniqueServerStages);
const expectedSet = new Set(expectedServerStages);

const missing = expectedServerStages.filter(s => !serverSet.has(s));
const extra = uniqueServerStages.filter(s => !expectedSet.has(s));

if (missing.length > 0 || extra.length > 0) {
  console.error('\nFAIL: Server stage literals mismatch!');
  if (missing.length > 0) console.error('  Missing from server:', missing.join(', '));
  if (extra.length > 0) console.error('  Extra in server:', extra.join(', '));
  process.exit(1);
}

console.log('Server stage literals MATCH expected set ✓\n');

// ============================================================
// 3. Verify frontend expected stages match server stages
// ============================================================

// The frontend test expects these 5 server stages + 1 unknown
const frontendTestStages = ['language', 'timezone', 'admin', 'preset', 'complete'];
const frontendSet = new Set(frontendTestStages);

const frontendMissing = frontendTestStages.filter(s => !serverSet.has(s));
if (frontendMissing.length > 0) {
  console.error('FAIL: Frontend test stages not all present in server:', frontendMissing.join(', '));
  process.exit(1);
}

console.log('Frontend test stages all present in server ✓\n');

// ============================================================
// All checks passed
// ============================================================
console.log('=== ALL CONTRACT CHECKS PASSED ===');
process.exit(0);