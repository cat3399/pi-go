export interface RefreshRequest {
  generation: number;
  sessionId: string | null;
  snapshot?: boolean;
  session?: boolean;
  turn?: boolean;
}

// Event bursts often contain turn_end, agent_settled and catalog changes for
// one durable update. Merge their reads, while retaining requests that arrive
// during an in-flight refresh so a newer change always gets another read.
export class RefreshScheduler {
  private readonly pending = new Map<string, RefreshRequest>();
  private timer: ReturnType<typeof setTimeout> | null = null;
  private running = false;
  private disposed = false;
  private readonly refresh: (requests: RefreshRequest[]) => Promise<void>;
  private readonly delayMs: number;

  constructor(
    refresh: (requests: RefreshRequest[]) => Promise<void>,
    delayMs = 20,
  ) {
    this.refresh = refresh;
    this.delayMs = delayMs;
  }

  start(): void {
    this.disposed = false;
    this.schedule();
  }

  request(request: RefreshRequest): void {
    if (this.disposed) return;
    const key = JSON.stringify([request.generation, request.sessionId]);
    const previous = this.pending.get(key);
    this.pending.set(key, {
      ...request,
      snapshot: request.snapshot || previous?.snapshot,
      session: request.session || previous?.session,
      turn: request.turn || previous?.turn,
    });
    this.schedule();
  }

  dispose(): void {
    this.disposed = true;
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
    this.pending.clear();
  }

  private schedule(): void {
    if (this.disposed || this.running || this.timer !== null || this.pending.size === 0) return;
    this.timer = setTimeout(() => { void this.flush(); }, this.delayMs);
  }

  private async flush(): Promise<void> {
    this.timer = null;
    if (this.disposed) return;
    const requests = [...this.pending.values()];
    this.pending.clear();
    this.running = true;
    try {
      await this.refresh(requests);
    } finally {
      this.running = false;
      this.schedule();
    }
  }
}
