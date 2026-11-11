// The model status report of gpu-setup model-status, without widgets.

// readReport gives the entries of a model status report of version 1.
export function readReport(output) {
    const report = JSON.parse(output);
    if (report?.version !== 1 || !Array.isArray(report.models)) throw new Error('unsupported model status report');
    return report.models;
}

// mergeReport gives the status after a card read the report again. Only the
// entry of workload changes, so other cards keep their own results, for
// example of a recheck. A status that was not loaded takes the whole
// report.
export function mergeReport(status, models, workload) {
    if (status?.state !== 'loaded') return {state: 'loaded', entries: new Map(models.map(entry => [entry.workload, entry]))};
    const own = models.find(entry => entry.workload === workload);
    if (own) status.entries.set(workload, own);
    else status.entries.delete(workload);
    return status;
}
