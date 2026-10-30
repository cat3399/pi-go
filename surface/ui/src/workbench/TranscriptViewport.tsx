import { Component, type ReactNode, type RefObject } from "react";
import type { ScrollPosition } from "./application-data";

interface TranscriptViewportProps {
  viewportRef: RefObject<HTMLDivElement | null>;
  start?: number;
  end?: number;
  loading: boolean;
  capture(): ScrollPosition | null;
  restore(position: ScrollPosition | null): void;
  children: ReactNode;
}

// Snapshot the reader immediately before React changes the DOM, not when a
// network request starts. The same keyed content can then stay at the same
// viewport offset across prepends, middle replacements and opposite-edge trims.
export class TranscriptViewport extends Component<TranscriptViewportProps> {
  getSnapshotBeforeUpdate(): ScrollPosition | null {
    return this.props.capture();
  }

  componentDidUpdate(_previous: TranscriptViewportProps, _state: unknown, position: ScrollPosition | null): void {
    this.props.restore(position);
  }

  render() {
    const { viewportRef, start, end, loading, children } = this.props;
    return (
      <div ref={viewportRef} className="pi-transcript pi-overlay-scroll-viewport" aria-live="polite"
        data-history-start={start} data-history-end={end} data-history-loading={loading || undefined}>
        <div className="pi-transcript-inner">{children}</div>
      </div>
    );
  }
}
