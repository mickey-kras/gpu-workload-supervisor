// Only remote action step references may vary, and only at immutable full SHAs.
// Inputs, scripts, local actions, step order and every other control stay exact.
function normalizeActionSteps(steps) {
  if (!Array.isArray(steps)) return steps;
  return steps.map(step => {
    if (!step || typeof step !== 'object') return step;
    const normalized = { ...step };
    if (Object.hasOwn(step, 'uses')) {
      const match = typeof step.uses === 'string' && step.uses.match(/^([\w.-]+(?:\/[\w.-]+)+)@[a-f0-9]{40}$/);
      // Wrap invalid references too: a literal placeholder or YAML mapping must
      // never collide with the representation of a valid immutable pin.
      normalized.uses = match ? { action: match[1], immutable: true } :
        { reference: step.uses, immutable: false };
    }
    return normalized;
  });
}

function normalizeWorkflowActionPins(workflow) {
  if (!workflow?.jobs) return workflow;
  return { ...workflow, jobs: Object.fromEntries(Object.entries(workflow.jobs).map(([id, job]) =>
    [id, Object.hasOwn(job, 'steps') ? { ...job, steps: normalizeActionSteps(job.steps) } : job])) };
}

function canonical(value) {
  if (Array.isArray(value)) return value.map(canonical);
  if (!value || typeof value !== 'object') return value;
  return Object.fromEntries(Object.entries(value).sort().map(([key, item]) => [key, canonical(item)]));
}

function sameActionSteps(actual, expected) {
  return JSON.stringify(canonical(normalizeActionSteps(actual))) ===
    JSON.stringify(canonical(normalizeActionSteps(expected)));
}

module.exports = { canonical, normalizeActionSteps, normalizeWorkflowActionPins, sameActionSteps };
