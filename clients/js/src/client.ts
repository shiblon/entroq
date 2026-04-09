import {
  Task,
  ClaimRequest,
  ClaimResponse,
  ModifyRequest,
  ModifyResponse,
  TasksRequest,
  TasksResponse,
  QueuesRequest,
  QueuesResponse,
  TimeResponse,
} from "./types";

export interface ClientOptions {
  baseUrl: string;
  claimantId?: string;
  headers?: Record<string, string>;
}

export class EntroQClient {
  private baseUrl: string;
  private claimantId: string;
  private headers: Record<string, string>;

  constructor(options: ClientOptions) {
    this.baseUrl = options.baseUrl.replace(/\/$/, "");
    this.claimantId = options.claimantId || this.generateClaimantId();
    this.headers = options.headers || {};
  }

  private generateClaimantId(): string {
    // Generate a 16-character hex string (8 random bytes).
    // We use a manual Math.random() implementation to ensure absolute
    // portability across all JS environments without requiring Node-specific
    // modules or modern web crypto globals.
    return "xxxxxxxxxxxxxxxx".replace(/x/g, () =>
      ((Math.random() * 16) | 0).toString(16)
    );
  }

  private async request<T>(
    path: string,
    method: string = "GET",
    body?: any
  ): Promise<T> {
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
      let errorMsg: string;
      try {
        const errJson = JSON.parse(text);
        errorMsg = errJson.message || text;
      } catch {
        errorMsg = text || response.statusText;
      }
      throw new Error(`EntroQ request failed (${response.status}): ${errorMsg}`);
    }

    return response.json() as Promise<T>;
  }

  /**
   * tryClaim attempts to claim a task from one of the specified queues immediately.
   */
  async tryClaim(
    queues: string[],
    durationMs: number = 30000
  ): Promise<ClaimResponse> {
    const body: ClaimRequest = {
      claimantId: this.claimantId,
      queues,
      durationMs: durationMs.toString(),
      pollMs: "0",
    };
    return this.request<ClaimResponse>("/api/v0/claim", "POST", body);
  }

  /**
   * claim attempts to claim a task, blocking or polling as necessary.
   */
  async claim(
    queues: string[],
    durationMs: number = 30000,
    pollMs: number = 5000
  ): Promise<ClaimResponse> {
    const body: ClaimRequest = {
      claimantId: this.claimantId,
      queues,
      durationMs: durationMs.toString(),
      pollMs: pollMs.toString(),
    };
    return this.request<ClaimResponse>("/api/v0/claim/wait", "POST", body);
  }

  /**
   * modify atomically updates, inserts, deletes, or depends on tasks.
   */
  async modify(request: Omit<ModifyRequest, "claimantId">): Promise<ModifyResponse> {
    const body: ModifyRequest = {
      claimantId: this.claimantId,
      ...request,
    };
    return this.request<ModifyResponse>("/api/v0/modify", "POST", body);
  }

  /**
   * tasks lists tasks in a particular queue.
   */
  async tasks(request: Omit<TasksRequest, "claimantId">): Promise<TasksResponse> {
    const query = new URLSearchParams(request as any).toString();
    const path = `/api/v0/tasks${query ? "?" + query : ""}`;
    const resp = await this.request<any>(path, "GET");
    return {
      tasks: resp.tasks || [],
    };
  }

  /**
   * queues lists statistics for multiple queues.
   */
  async queues(request: QueuesRequest = {}): Promise<QueuesResponse> {
    const query = new URLSearchParams();
    if (request.matchPrefix)
      request.matchPrefix.forEach((p) => query.append("matchPrefix", p));
    if (request.matchExact)
      request.matchExact.forEach((e) => query.append("matchExact", e));
    if (request.limit) query.append("limit", request.limit.toString());

    const qs = query.toString();
    const path = `/api/v0/queues${qs ? "?" + qs : ""}`;
    const resp = await this.request<any>(path, "GET");
    return {
      queues: resp.queues || [],
    };
  }

  /**
   * queueStats is a shortcut to get statistics specifically for the stats endpoint.
   */
  async queueStats(request: QueuesRequest = {}): Promise<QueuesResponse> {
    const query = new URLSearchParams();
    if (request.matchPrefix)
      request.matchPrefix.forEach((p) => query.append("matchPrefix", p));
    if (request.matchExact)
      request.matchExact.forEach((e) => query.append("matchExact", e));
    if (request.limit) query.append("limit", request.limit.toString());

    const qs = query.toString();
    const path = `/api/v0/queues/stats${qs ? "?" + qs : ""}`;
    const resp = await this.request<any>(path, "GET");
    return {
      queues: resp.queues || [],
    };
  }

  /**
   * time returns the current server time in milliseconds since the epoch.
   */
  async time(): Promise<string> {
    const resp = await this.request<TimeResponse>("/api/v0/time", "GET");
    return resp.timeMs;
  }

  /**
   * streamTasks returns an async iterator that yields tasks as they are received from the server.
   * This uses HTTP Chunked Transfer Encoding to provide real-time updates.
   */
  async *streamTasks(request: Omit<TasksRequest, "claimantId">): AsyncIterable<Task> {
    const url = `${this.baseUrl}/api.EntroQ/StreamTasks`;

    const jsonBody = JSON.stringify({
      ...request,
    });
    const jsonBytes = new TextEncoder().encode(jsonBody);
    
    // Connect envelope: 1-byte flags + 4-byte big-endian length + data
    const envelope = new Uint8Array(5 + jsonBytes.length);
    envelope[0] = 0; // flags
    new DataView(envelope.buffer).setUint32(1, jsonBytes.length, false);
    envelope.set(jsonBytes, 5);

    const response = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/connect+json",
        "Accept": "application/connect+json",
        "Connect-Protocol-Version": "1",
        ...this.headers,
      },
      body: envelope,
    });

    if (!response.ok) {
        throw new Error(`EntroQ stream failed (${response.status})`);
    }

    if (!response.body) {
        return;
    }

    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = new Uint8Array(0);

    const appendToBuffer = (newBytes: Uint8Array) => {
        const combined = new Uint8Array(buffer.length + newBytes.length);
        combined.set(buffer);
        combined.set(newBytes, buffer.length);
        buffer = combined;
    };

    try {
      while (true) {
        const { done, value } = await reader.read();
        if (value) appendToBuffer(value);
        
        while (buffer.length >= 5) {
          const flags = buffer[0];
          const len = new DataView(buffer.buffer, buffer.byteOffset, 5).getUint32(1, false);
          
          if (buffer.length < 5 + len) break;

          const data = buffer.slice(5, 5 + len);
          buffer = buffer.slice(5 + len);

          if ((flags & 0x02) !== 0) {
            // End of stream trailer
            return;
          }

          const jsonStr = decoder.decode(data);
          try {
            const obj = JSON.parse(jsonStr);
            if (obj.tasks && Array.isArray(obj.tasks)) {
              for (const t of obj.tasks) {
                yield t;
              }
            }
          } catch (e) {
            // Ignore parse errors
          }
        }

        if (done) break;
      }
    } finally {
      reader.releaseLock();
    }
  }

  /**
   * getClaimantId returns the current claimant ID being used by this client.
   */
  getClaimantId(): string {
    return this.claimantId;
  }
}
