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
    'upstream-not-found': 'Hugging Face did not find this repository or revision.',
    'upstream-denied': 'Hugging Face refused access. Check your Hugging Face login.',
    'upstream-invalid': 'Hugging Face gave a file list that setup cannot read.',
    'upstream-unreachable': 'Setup could not reach Hugging Face.',
    'upstream-moved': 'Hugging Face moved this repository. Enter its current name.',
    'upstream-history-too-long': 'The branch history is too long to find this revision. Enter a commit that a branch or a tag points at.',
    'hub-token-unusable': 'Setup cannot use your Hugging Face login token. Check HF_TOKEN or the token file.',
    'revision-not-on-branch': 'No branch or tag of the repository has this revision.',
    'file-not-upstream': 'A model file is not in the repository at this revision.',
    'upstream-identity-mismatch': 'A model file differs from the repository at this revision.',
    'no-upstream-sha256': 'The repository gives no SHA-256 for a model file that the loader reads.',
    'no-chat-template': 'The model has no chat template.',
    'named-chat-templates': 'The model has named chat templates, which the check does not support.',
    'chat-template-too-large': 'The chat template is larger than the limit.',
    'chat-template-too-complex': 'The chat template is too complex for the check.',
    'chat-template-invalid': 'The chat template is not valid.',
    'chat-template-conflict': 'The model files hold different chat templates.',
    'chat-template-syntax': 'The chat template uses syntax that the check does not accept.',
    'chat-template-hidden-text': 'The chat template has hidden text.',
    'chat-template-statement': 'The chat template has a statement that the check does not accept.',
    'chat-template-unsafe-name': 'The chat template uses a name that the check does not accept.',
    'chat-template-unknown-source': 'The chat template reads a value of an unknown source.',
    'chat-template-too-many-literals': 'The chat template has more text to review than the limit.',
    'chat-template-review-too-large': 'The review list of the chat template is larger than the limit.',
    'chat-template-check-failed': 'The chat template check did not complete.',
    'chat-template-needs-review': 'The chat template has text to review and approve.',
    'chat-template-approval-failed': 'Setup could not read the template approval.',
};

// failureCauses map a fixed part of a gpu-setup error to its cause. The
// errors hold no path.
const failureCauses = [
    ['setup is busy with another change', 'Setup was busy with another change. Try again.'],
    ['deployment is in maintenance', 'Setup is in maintenance. Finish setup, then try again.'],
    ['installed release is not activated', 'The installed release is not activated. Finish setup, then try again.'],
    ['need an activated setup', 'Finish setup first.'],
    ['does not apply to the accepted workload', 'The workload changed during the action. Run the checks again.'],
    ['stopped at its time limit', 'The check stopped at its time limit.'],
    ['does not match the chat template review', 'The review list changed. Read it again.'],
    ['changed during the approval', 'The stored result changed. Read the review list again.'],
];

// failureCause gives the cause of a failed gpu-setup action, or ''.
export const failureCause = error => failureCauses.find(([part]) => error?.message?.includes(part))?.[1] ?? '';

// reasonText gives the text of a reason code, or the code itself.
export const reasonText = reason => reasonTexts[reason] ?? `Reason code: ${reason}`;

// blockingReason gives the text of the first check that did not pass and
// has a reason, or ''. The pending review has its own text on the card.
export function blockingReason(entry) {
    const check = entry?.checks?.find(item => item.reason && item.outcome !== 'passed' && item.reason !== 'chat-template-needs-review');
    return check ? reasonText(check.reason) : '';
}
