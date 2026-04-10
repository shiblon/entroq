import { Client as PGClient } from 'pg';
import { 
  Task, 
  TaskData, 
  ModifyRequest, 
  ModifyResponse,
  QueuesResponse,
  TasksResponse,
  EntroQClientInterface,
  ClaimResponse
} from '../types';
import { generateClaimantId } from '../client';
import * as crypto from 'crypto';

export const SCHEMA_VERSION = "0.10.0";

/**
 * pgChannelName returns the PostgreSQL notification channel name for a queue.
 * Mirrors pgChannelName in backend/eqpg/pgnotify.go and channel_name() in schema.sql.
 */
export function pgChannelName(queue: string): string {
    const sanitized = queue.replace(/[^a-zA-Z0-9]/g, '_');
    if (sanitized.length + 2 <= 63) {
        return `q_${sanitized}`;
    }
    const hash = crypto.createHash('md5').update(queue).digest('hex');
    // q_(25)_(8-hex-hash)_(26) = 2+25+1+8+1+26 = 63 bytes.
    return `q_${sanitized.slice(0, 25)}_${hash.slice(0, 8)}_${sanitized.slice(-26)}`;
}

function rowToTask(row: any): Task {
    return {
        id: row.id,
        version: row.version,
        queue: row.queue,
        atMs: new Date(row.at).getTime().toString(),
        claimantId: row.claimant || '',
        value: row.value,
        createdMs: row.created ? new Date(row.created).getTime().toString() : '0',
        modifiedMs: new Date(row.modified).getTime().toString(),
        claims: row.claims,
        attempt: row.attempt,
        err: row.err || '',
    };
}

/**
 * EntroQPG is a direct PostgreSQL client for EntroQ.
 * Satisfies EntroQClientInterface, allowing it to be used with EntroQWorker.
 */
export class EntroQPG implements EntroQClientInterface {
    private client: PGClient;
    private claimantId: string;

    constructor(config: any, claimantId?: string) {
        this.client = new PGClient(config);
        this.claimantId = claimantId || generateClaimantId();
    }

    async connect() {
        await this.client.connect();
        await this.checkSchemaVersion();
    }

    async disconnect() {
        await this.client.end();
    }

    private async checkSchemaVersion() {
        try {
            const res = await this.client.query(
                "SELECT value FROM entroq.meta WHERE key = 'schema_version'"
            );
            if (res.rowCount === 0) {
                throw new Error("schema_version not found in entroq.meta");
            }
            const stored = res.rows[0].value;
            if (stored !== SCHEMA_VERSION) {
                throw new Error(`schema version mismatch: database has ${stored}, code expects ${SCHEMA_VERSION}`);
            }
        } catch (err: any) {
            if (err.code === '42P01') { // undefined_table
                throw new Error("entroq.meta table not found; initialize the database with schema.sql");
            }
            throw err;
        }
    }

    async time(): Promise<string> {
        const res = await this.client.query('SELECT now() as t');
        return new Date(res.rows[0].t).getTime().toString();
    }

    async queues(prefix: string = '', exact: string[] = [], limit: number = 0): Promise<QueuesResponse> {
        const res = await this.client.query(
            'SELECT * FROM entroq.queues($1, $2, $3)',
            [prefix, exact, limit]
        );
        return {
            queues: res.rows.map(r => ({
                name: r.name,
                numTasks: parseInt(r.num_tasks),
                numClaimed: 0,
                numAvailable: 0,
                maxClaims: 0
            }))
        };
    }

    async tasks(queue: string = '', limit: number = 0, omitValues: boolean = false): Promise<TasksResponse> {
        const res = await this.client.query(
            'SELECT * FROM entroq.tasks($1, $2, $3)',
            [queue, limit, omitValues]
        );
        return {
            tasks: res.rows.map(rowToTask)
        };
    }

    async tryClaim(queues: string | string[], durationMs: number = 30000): Promise<Task | undefined> {
        const queueList = Array.isArray(queues) ? queues : [queues];
        const res = await this.client.query(
            'SELECT * FROM entroq.try_claim($1, $2, $3::interval)',
            [queueList, this.claimantId, `${durationMs} milliseconds`]
        );
        return res.rows.length > 0 ? rowToTask(res.rows[0]) : undefined;
    }

    /**
     * claim blocks until a task is available using a combination of tryClaim 
     * and LISTEN/NOTIFY.
     */
    async claim(queues: string[], durationMs: number = 30000, pollMs: number = 5000): Promise<ClaimResponse> {
        const channels = queues.map(pgChannelName);
        
        // Ensure we are listening to all requested channels.
        for (const chan of channels) {
            await this.client.query(`LISTEN "${chan}"`);
        }

        try {
            while (true) {
                // 1. Try an immediate claim.
                const task = await this.tryClaim(queues, durationMs);
                if (task) return { task };

                // 2. If nothing, wait for a notification OR the poll timeout.
                await new Promise<void>((resolve) => {
                    const timer = setTimeout(resolve, pollMs);
                    
                    // The 'notification' event is fired by the pg client whenever a NOTIFY arrives.
                    const onNotify = (msg: any) => {
                        if (channels.includes(msg.channel)) {
                            this.client.removeListener('notification', onNotify);
                            clearTimeout(timer);
                            resolve();
                        }
                    };

                    this.client.once('notification', onNotify);
                });
            }
        } finally {
            // Cleanup: stop listening.
            for (const chan of channels) {
                await this.client.query(`UNLISTEN "${chan}"`);
            }
        }
    }

    async modify(request: Omit<ModifyRequest, "claimantId">): Promise<ModifyResponse> {
        const res = await this.client.query(
            'SELECT * FROM entroq.modify($1, $2::jsonb, $3::jsonb, $4::jsonb, $5::jsonb)',
            [
                this.claimantId,
                JSON.stringify(request.depends || []),
                JSON.stringify(request.deletes || []),
                JSON.stringify(request.inserts || []),
                JSON.stringify(request.changes || [])
            ]
        );

        const inserted: Task[] = [];
        const changed: Task[] = [];

        for (const row of res.rows) {
            const t = rowToTask(row);
            if (row.kind === 'inserted') inserted.push(t);
            else if (row.kind === 'changed') changed.push(t);
        }

        return { inserted, changed };
    }
}
