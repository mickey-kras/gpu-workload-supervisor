export class ResponseBuffer {
    constructor(limit = 65536) {
        this.limit = limit;
        this.bytes = [];
        this.length = 0;
    }
    append(bytes) {
        if (this.length + bytes.length > this.limit)
            throw new Error('Response too large');
        this.bytes.push(bytes);
        this.length += bytes.length;
    }
    finish() {
        const all = new Uint8Array(this.length);
        let offset = 0;
        for (const bytes of this.bytes) {
            all.set(bytes, offset);
            offset += bytes.length;
        }
        return new TextDecoder('utf-8', { fatal: true }).decode(all);
    }
}
