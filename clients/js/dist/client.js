"use strict";
Object.defineProperty(exports, "__esModule", { value: true });
exports.EntroQClient = void 0;
class EntroQClient {
    constructor(options) {
        this.baseUrl = options.baseUrl.replace(/\/$/, "");
        this.claimantId = options.claimantId || this.generateClaimantId();
        this.headers = options.headers || {};
    }
    generateClaimantId() {
        // Basic UUID-like generation for claimant ID if not provided.
        return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, (c) => {
            const r = (Math.random() * 16) | 0;
            const v = c === "x" ? r : (r & 0x3) | 0x8;
            return v.toString(16);
        });
    }
    async request(path, method = "GET", body) {
        const url = `${this.baseUrl}${path}`;
        const response = await fetch(url, {
            method,
            headers: {
                "Content-Type": "application/json",
                ...this.headers,
            },
            body: body ? JSON.stringify(body) : undefined,
        });
        if (!response.ok) {
            const text = await response.text();
            let errorMsg;
            try {
                const errJson = JSON.parse(text);
                errorMsg = errJson.message || text;
            }
            catch {
                errorMsg = text || response.statusText;
            }
            throw new Error(`EntroQ request failed (${response.status}): ${errorMsg}`);
        }
        return response.json();
    }
    /**
     * tryClaim attempts to claim a task from one of the specified queues immediately.
     */
    async tryClaim(queues, durationMs = 30000) {
        const body = {
            claimantId: this.claimantId,
            queues,
            durationMs: durationMs.toString(),
            pollMs: "0",
        };
        return this.request("/api/v0/claim", "POST", body);
    }
    /**
     * claim attempts to claim a task, blocking or polling as necessary.
     */
    async claim(queues, durationMs = 30000, pollMs = 5000) {
        const body = {
            claimantId: this.claimantId,
            queues,
            durationMs: durationMs.toString(),
            pollMs: pollMs.toString(),
        };
        return this.request("/api/v0/claim/wait", "POST", body);
    }
    /**
     * modify atomically updates, inserts, deletes, or depends on tasks.
     */
    async modify(request) {
        const body = {
            claimantId: this.claimantId,
            ...request,
        };
        return this.request("/api/v0/modify", "POST", body);
    }
    /**
     * tasks lists tasks in a particular queue.
     */
    async tasks(request) {
        const { queue, ...rest } = request;
        const query = new URLSearchParams(rest).toString();
        const path = `/api/v0/queues/${encodeURIComponent(queue)}/tasks${query ? "?" + query : ""}`;
        return this.request(path, "GET");
    }
    /**
     * queues lists statistics for multiple queues.
     */
    async queues(request = {}) {
        const query = new URLSearchParams();
        if (request.matchPrefix)
            request.matchPrefix.forEach((p) => query.append("matchPrefix", p));
        if (request.matchExact)
            request.matchExact.forEach((e) => query.append("matchExact", e));
        if (request.limit)
            query.append("limit", request.limit.toString());
        const qs = query.toString();
        const path = `/api/v0/queues${qs ? "?" + qs : ""}`;
        return this.request(path, "GET");
    }
    /**
     * queueStats is a shortcut to get statistics specifically for the stats endpoint.
     */
    async queueStats(request = {}) {
        const query = new URLSearchParams();
        if (request.matchPrefix)
            request.matchPrefix.forEach((p) => query.append("matchPrefix", p));
        if (request.matchExact)
            request.matchExact.forEach((e) => query.append("matchExact", e));
        if (request.limit)
            query.append("limit", request.limit.toString());
        const qs = query.toString();
        const path = `/api/v0/queues/stats${qs ? "?" + qs : ""}`;
        return this.request(path, "GET");
    }
    /**
     * time returns the current server time in milliseconds since the epoch.
     */
    async time() {
        const resp = await this.request("/api/v0/time", "GET");
        return resp.timeMs;
    }
    /**
     * streamTasks returns an async iterator that yields tasks as they are received from the server.
     * This uses HTTP Chunked Transfer Encoding to provide real-time updates.
     */
    async *streamTasks(request) {
        const { queue, ...rest } = request;
        const query = new URLSearchParams(rest).toString();
        const url = `${this.baseUrl}/api/v0/queues/${encodeURIComponent(queue)}/tasks/stream${query ? "?" + query : ""}`;
        const response = await fetch(url, {
            method: "GET",
            headers: {
                ...this.headers,
            },
        });
        if (!response.ok) {
            throw new Error(`EntroQ stream failed (${response.status})`);
        }
        if (!response.body) {
            return;
        }
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        try {
            while (true) {
                const { done, value } = await reader.read();
                if (done)
                    break;
                buffer += decoder.decode(value, { stream: true });
                // Simple streaming JSON parser for the TasksResponse sequence.
                let start = 0;
                let depth = 0;
                for (let i = 0; i < buffer.length; i++) {
                    if (buffer[i] === '{')
                        depth++;
                    else if (buffer[i] === '}') {
                        depth--;
                        if (depth === 0) {
                            const jsonStr = buffer.substring(start, i + 1);
                            try {
                                const obj = JSON.parse(jsonStr);
                                if (obj.tasks && Array.isArray(obj.tasks)) {
                                    for (const t of obj.tasks) {
                                        yield t;
                                    }
                                }
                            }
                            catch (e) {
                                // Ignore partial JSON or structural errors within a chunk.
                            }
                            start = i + 1;
                        }
                    }
                }
                buffer = buffer.substring(start);
            }
        }
        finally {
            reader.releaseLock();
        }
    }
    /**
     * getClaimantId returns the current claimant ID being used by this client.
     */
    getClaimantId() {
        return this.claimantId;
    }
}
exports.EntroQClient = EntroQClient;
