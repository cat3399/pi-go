/** Presentation layers only; session navigation stays with the Application client. */
export class BackNavigation {
  private layers: { handler: () => void; priority: number }[] = [];

  register(handler: () => void, priority: number): () => void {
    const layer = { handler, priority };
    this.layers.push(layer);
    return () => { this.layers = this.layers.filter((entry) => entry !== layer); };
  }

  handle(): boolean {
    const top = this.layers.reduce<typeof this.layers[number] | undefined>((selected, entry) => (
      !selected || entry.priority >= selected.priority ? entry : selected
    ), undefined);
    if (!top) return false;
    top.handler();
    return true;
  }
}

export const backNavigation = new BackNavigation();
export const handleBackNavigation = (): boolean => backNavigation.handle();
