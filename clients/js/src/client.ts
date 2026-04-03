// Base stub for EntroQ JS client.
// To be implemented fully once the REST/RPC design is finalized.

export class EntroQClient {
    private endpoint: string;

    constructor(endpoint: string) {
        this.endpoint = endpoint;
    }

    async claim(queues: string[]): Promise<any> {
        throw new Error("Not implemented");
    }

    async modify(inserts: any[], changes: any[], deletes: any[], depends: any[]): Promise<any> {
        throw new Error("Not implemented");
    }
}
