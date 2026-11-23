// The model status report of gpu-setup model-status, without widgets.

// readReport returns the entries of a version 1 model status report.
export function readReport(output) {
    const report = JSON.parse(output);
    if (report?.version !== 1 || !Array.isArray(report.models)) throw new Error('unsupported model status report');
    return report.models;
}

// mergeReport returns the status after a card rereads the report. Only the
// entry for workload changes, so other cards keep their own results (for
// example from a recheck). A status that was not loaded takes the whole
// report.
export function mergeReport(status, models, workload) {
    if (status?.state !== 'loaded') return {state: 'loaded', entries: new Map(models.map(entry => [entry.workload, entry]))};
    const own = models.find(entry => entry.workload === workload);
    if (own) status.entries.set(workload, own);
    else status.entries.delete(workload);
    return status;
}
