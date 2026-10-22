/**
 * Workers through an `eqlink work` child this process starts and owns.
 *
 *     await new NodeGateway({ entroq: "localhost:37706" }).run(
 *       new Worker(["in"], { work: async (w) => M.of(M.deleting(w.task)) }),
 *     );
 *
 * One child serves every session in the process, over HTTP on loopback. It
 * runs in a process group of its own, so a signal reaches it only when the
 * gateway passes it on, and it exits on its own when this process does.
 */
import { spawn, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";

import { failure, Gateway, GatewayError, HTTPTransport, SessionGone, type Transport, type Worker } from "./gateway";

/** What a gateway's exit code says, when it exited without a word. */
export function exitError(code: number | null): Error {
  if (code === 0) {
    return new SessionGone("gateway exited");
  }
  const cls = code === 75 ? "transient" : code === 78 ? "caller" : "gateway";
  return new GatewayError(cls, `gateway exited ${code}`);
}

/** One `eqlink work --addr` child, shared by every session in this process. */
export class HTTPChild {
  private proc?: ChildProcess;
  private url?: string;
  private starting?: Promise<string>;

  constructor(private readonly argv: string[]) {}

  /** The child's URL, starting it if it is not running. */
  async connect(): Promise<string> {
    if (this.proc && this.proc.exitCode === null && this.proc.signalCode === null && this.url) {
      return this.url;
    }
    this.starting ??= this.start().finally(() => {
      this.starting = undefined;
    });
    return this.starting;
  }

  private start(): Promise<string> {
    const [cmd, ...args] = this.argv;
    const proc = spawn(cmd, [...args, "--addr", "127.0.0.1:0", "--parent-pid", String(process.pid)], {
      stdio: ["ignore", "pipe", "inherit"],
      detached: true,
    });
    return new Promise<string>((resolve, reject) => {
      const lines = createInterface({ input: proc.stdout! });
      let settled = false;
      const settle = (err?: Error, url?: string) => {
        if (settled) {
          return;
        }
        settled = true;
        lines.close();
        if (err) {
          reject(err);
        } else {
          this.proc = proc;
          this.url = url;
          resolve(url!);
        }
      };
      // The one line it writes once listening, or the error that stopped it.
      lines.once("line", (line) => {
        let announce: any;
        try {
          announce = JSON.parse(line);
        } catch {
          settle(new GatewayError("gateway", `gateway announced ${JSON.stringify(line)}`));
          return;
        }
        if (announce.type === "error") {
          settle(failure(announce));
          return;
        }
        settle(undefined, `http://${announce.addr}/`);
      });
      proc.once("error", (e) => settle(new GatewayError("caller", `start ${cmd}: ${e.message}`)));
      proc.once("exit", (code) => settle(exitError(code)));
    });
  }

  /** Pass signal on to the child, if it is running. */
  signal(sig: NodeJS.Signals): void {
    if (this.proc && this.proc.exitCode === null && this.proc.signalCode === null) {
      this.proc.kill(sig);
    }
  }

  /** Stop the child; called once every session has ended. */
  async close(): Promise<void> {
    const proc = this.proc;
    if (!proc || proc.exitCode !== null || proc.signalCode !== null) {
      return;
    }
    const exited = new Promise((resolve) => proc.once("exit", resolve));
    proc.kill("SIGTERM");
    await exited;
  }
}

export interface NodeGatewayOptions {
  /** The EntroQ service the gateway connects to. */
  entroq?: string;
  /** The eqlink binary. */
  eqlink?: string;
  /** The operator's flags for `eqlink work`, such as ["--lease", "10s"]. */
  args?: string[];
  /**
   * Handle SIGINT and SIGTERM while running: the first drains, a second hangs
   * up at once and exits. True by default; false leaves signals to the caller,
   * who stops with stop().
   */
  handleSignals?: boolean;
  log?: (line: string) => void;
}

/**
 * Runs workers through an `eqlink work` child, each as a session of its own,
 * and stops them together.
 */
export class NodeGateway extends Gateway {
  readonly child: HTTPChild;
  private readonly handleSignals: boolean;

  constructor(opts: NodeGatewayOptions = {}) {
    const child = new HTTPChild([
      opts.eqlink ?? "eqlink",
      "--entroq",
      opts.entroq ?? "localhost:37706",
      "work",
      ...(opts.args ?? []),
    ]);
    super({ connect: async (): Promise<Transport> => new HTTPTransport(await child.connect()), log: opts.log });
    this.child = child;
    this.handleSignals = opts.handleSignals ?? true;
  }

  /**
   * Run until every worker has stopped: by ctx.stop, by stop() or a signal, or
   * by a failure a new session cannot fix, which is then thrown.
   */
  async run(...workers: Worker[]): Promise<void> {
    const onSignal = (sig: NodeJS.Signals) => {
      if (this.stopping.signal.aborted) {
        // A second signal: the gateway hangs up, leaving tasks to their leases.
        this.child.signal(sig);
        process.exit(130);
      }
      this.log(`[worker] ${sig}: draining`);
      this.stop(sig);
    };
    if (this.handleSignals) {
      process.on("SIGINT", onSignal);
      process.on("SIGTERM", onSignal);
    }
    try {
      await super.run(...workers);
    } finally {
      if (this.handleSignals) {
        process.off("SIGINT", onSignal);
        process.off("SIGTERM", onSignal);
      }
      await this.child.close();
    }
  }

  /**
   * Drain: every task in hand finishes, and no session claims again. The
   * gateway does it, told by sig, and answers each session with quit.
   */
  stop(sig: NodeJS.Signals = "SIGTERM"): void {
    super.stop();
    this.child.signal(sig);
  }
}
