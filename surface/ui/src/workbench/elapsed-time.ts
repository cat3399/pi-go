// A running label samples wall time only once. Browser and server clocks can
// differ, and subsequent browser clock corrections must not rewind the timer.
// The completed duration still comes from the two durable server timestamps.
export class ElapsedClock {
  private origin: { at: number; elapsed: number } | null = null;
  private latest: number | null = null;

  get lastElapsed(): number | null { return this.latest; }

  read(startedAt: number, monotonicNow: number, wallNow: number): number {
    this.origin ??= { at: monotonicNow, elapsed: Math.max(0, wallNow - startedAt) };
    this.latest = this.origin.elapsed + Math.max(0, monotonicNow - this.origin.at);
    return this.latest;
  }
}

export function elapsedBetween(startedAt: number | null, endedAt: number | null): number | null {
  return startedAt !== null && endedAt !== null && endedAt >= startedAt ? endedAt - startedAt : null;
}
