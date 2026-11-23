// Plain text for the reason codes of the model checks. A reason code never
// holds a path or file content, and the text does not either.
const reasonTexts = {
    'code-or-pickle-file': 'The model folder has a code or pickle file.',
    'unlisted-file': 'The model folder has a file that the allowlist does not accept.',
    'nested-file': 'The model folder has a subfolder.',
    'adapter-redirect': 'A configuration file points to an adapter.',
    'model-redirect': 'A configuration file points to another model.',
    'file-path-redirect': 'A configuration file points to another file.',
    'remote-code-mapping': 'A configuration file maps remote code.',
    'invalid-json': 'A JSON file is not valid strict JSON.',
    'json-file-too-large': 'A JSON file is larger than the limit.',
    'misplaced-chat-template': 'A chat template is in a file that cannot hold it.',
    'files-changed': 'The model files changed during the check.',
    'unreadable': 'Setup could not read a model file.',
    'no-safetensors-weights': 'The model folder has no .safetensors weights.',
    'no-model-configuration': 'The model folder has no model configuration.',
    'link-or-nonregular-file': 'The model folder has a link or a special file. A Hugging Face cache folder holds links: download a copy with hf download --local-dir and select that folder.',
    'too-many-files': 'The model folder has more files than the limit.',
    'invalid-shard-index': 'The shard index of the weights is not valid.',
    'shard-index-too-large': 'The shard index of the weights is larger than the limit.',
    'no-chat-template': 'The model has no chat template.',
    'named-chat-templates': 'The model has named chat templates, which the check does not support.',
    'chat-template-too-large': 'The chat template is larger than the limit.',
    'chat-template-invalid': 'The chat template is not valid.',
    'chat-template-conflict': 'The model files hold different chat templates.',
};

// failureCauses map a fixed part of a gpu-setup error to its cause. The
// errors hold no path.
const failureCauses = [
    ['setup was busy with another change', 'Setup was busy with another change. Try again.'],
    ['deployment is in maintenance', 'Setup is in maintenance. Finish setup, then try again.'],
    ['installed release is not activated', 'The installed release is not activated. Finish setup, then try again.'],
    ['need an activated setup', 'Finish setup first.'],
    ['does not apply to the accepted workload', 'The workload changed during the action. Run the checks again.'],
    ['stopped at its time limit', 'The action stopped at its time limit.'],
];

// failureCause gives the cause of a failed gpu-setup action, or ''.
export const failureCause = error => failureCauses.find(([part]) => error?.message?.includes(part))?.[1] ?? '';

// reasonText gives the text of a reason code, or the code itself.
export const reasonText = reason => reasonTexts[reason] ?? `Reason code: ${reason}`;

// blockingReason gives the text of the first check that did not pass and
// has a reason, or ''.
export function blockingReason(entry) {
    const check = entry?.checks?.find(item => item.reason && item.outcome !== 'passed');
    return check ? reasonText(check.reason) : '';
}
