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
