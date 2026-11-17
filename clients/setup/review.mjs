// Bind activation to one validated snapshot and retire late validation replies.
export class ReviewedConfiguration {
    generation = 0;
    request = null;

    invalidate() {
        this.generation++;
        this.request = null;
    }

    begin(request) {
        return {generation: this.generation, request};
    }

    accept(candidate) {
        if (candidate.generation !== this.generation)
            return false;
        this.request = candidate.request;
        return true;
    }

    confirmed() {
        if (this.request === null)
            throw new Error('Review the current configuration before applying.');
        const request = JSON.parse(this.request);
        request.confirmQuiesced = true;
        return JSON.stringify(request);
    }
}

// Mirror of control.WorkloadProfile.HasLimits: an empty, null or all-zero
// limits object is absent, and the backend decoder rejects it.
export function hasLimits(profile) {
    const limits = profile.nativeModel?.owned?.limits;
    return limits !== null && typeof limits === 'object' && Object.values(limits).some(Boolean);
}

// Mirror of control.WorkloadProfile.HasServingOptions: an empty or null
// serving object is absent, and the backend decoder rejects it.
function hasServingOptions(profile) {
    const serving = profile.nativeModel?.owned?.serving;
    return serving !== null && typeof serving === 'object' && Object.values(serving).some(Boolean);
}

// Mirror of control.WorkloadProfile.HasArtifactProtection: an empty or null
// artifacts object is absent, and the backend decoder rejects it.
function hasArtifactProtection(profile) {
    const artifacts = profile.nativeModel?.owned?.artifacts;
    return artifacts !== null && typeof artifacts === 'object' && Object.values(artifacts).some(Boolean);
}

// Mirror of draftsVersion in internal/setup/drafts.go: 4 with artifact
// protection, 3 with serving options, 2 with resource limits, 1 otherwise.
export function draftsVersion(drafts) {
    if (drafts.some(draft => draft.binding?.owned?.artifacts))
        return 4;
    if (drafts.some(draft => draft.binding?.owned?.serving))
        return 3;
    return drafts.some(draft => draft.binding?.owned?.limits) ? 2 : 1;
}

// Mirror of control.LowestCatalogVersion in internal/control/catalog.go: the
// lowest catalog version that the profiles need. The backend recomputes it
// before preview and apply.
export function lowestCatalogVersion(profiles) {
    if (profiles.some(hasArtifactProtection))
        return 7;
    if (profiles.some(hasServingOptions))
        return 6;
    if (profiles.some(hasLimits))
        return 5;
    if (profiles.some(profile => profile.trainer))
        return 4;
    return profiles.some(profile => profile.nativeModel?.owned) ? 2 : 1;
}
