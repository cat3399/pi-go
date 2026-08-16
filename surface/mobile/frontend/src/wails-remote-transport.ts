import { Call, Events } from "@wailsio/runtime";
import {
  applyEventData,
  type EventObserver,
  type EventSubscription,
  type RemoteApplicationTransport,
  type RemoteRequestInit,
  type RemoteTransportResponse,
} from "@cat3399/pi-workbench";

const bridge = "main.RemoteBridge";
const remoteEventName = "pi:mobile-remote-event";
const remoteErrorName = "pi:mobile-remote-error";

interface RemoteStreamOpened {
  streamId: number;
}

interface RemoteStreamEvent {
  streamId: number;
  data: string;
}

interface RemoteStreamError {
  streamId: number;
  revision: number;
  message: string;
  terminal: boolean;
}

function call<T>(method: string, ...args: unknown[]): Promise<T> {
  return Call.ByName(`${bridge}.${method}`, ...args) as Promise<T>;
}

export class WailsRemoteTransport implements RemoteApplicationTransport {
  private readonly subscriptions = new Set<() => void>();

  request(
    endpoint: string,
    path: string,
    token: string,
    init: RemoteRequestInit = {},
  ): Promise<RemoteTransportResponse> {
    return call(
      "Request",
      init.method ?? "GET",
      endpoint,
      path,
      token,
      init.body ?? "",
    );
  }

  subscribe(
    endpoint: string,
    token: string,
    after: number,
    observer: EventObserver,
  ): EventSubscription {
    let closed = false;
    let connected = false;
    let streamId = 0;
    let resolveReady: () => void = () => undefined;
    let rejectReady: (error: unknown) => void = () => undefined;
    const ready = new Promise<void>((resolve, reject) => {
      resolveReady = resolve;
      rejectReady = reject;
    });

    const offEvent = Events.On(remoteEventName, (event) => {
      const payload = event.data as RemoteStreamEvent;
      if (closed || payload.streamId !== streamId) return;
      applyEventData(payload.data, observer, () => {
        connected = true;
        resolveReady();
      });
    });
    const offError = Events.On(remoteErrorName, (event) => {
      const payload = event.data as RemoteStreamError;
      if (closed || payload.streamId !== streamId) return;
      if (!connected) {
        rejectReady(new Error(payload.message || "远程事件连接失败"));
        close();
      } else if (payload.terminal) {
        observer.onReset(payload.revision, payload.message || "远程事件连接已关闭");
      }
    });

    const close = () => {
      if (closed) return;
      closed = true;
      offEvent();
      offError();
      this.subscriptions.delete(close);
      if (streamId) void call("CloseEventStream", streamId);
    };
    this.subscriptions.add(close);

    void call<RemoteStreamOpened>("OpenEventStream", endpoint, token, after)
      .then(async (opened) => {
        if (closed) {
          await call("CloseEventStream", opened.streamId);
          return;
        }
        streamId = opened.streamId;
        await call("ResumeEventStream", streamId);
      })
      .catch((error) => {
        rejectReady(error);
        close();
      });

    return { ready, close };
  }

  close(): void {
    for (const close of [...this.subscriptions]) close();
  }
}
