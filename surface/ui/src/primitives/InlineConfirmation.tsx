import { useRef, type HTMLAttributes, type MouseEventHandler } from "react";
import { Check, X } from "lucide-react";
import { IconAction } from "./InlineActions";
import { useBackNavigation } from "./useBackNavigation";

interface InlineConfirmationProps extends HTMLAttributes<HTMLDivElement> {
  message: string;
  working?: boolean;
  onConfirm: MouseEventHandler<HTMLButtonElement>;
  onCancel: MouseEventHandler<HTMLButtonElement>;
}

export function InlineConfirmation({
  message,
  working = false,
  onConfirm,
  onCancel,
  className = "",
  title,
  ...props
}: InlineConfirmationProps) {
  const cancelRef = useRef<HTMLButtonElement>(null);
  useBackNavigation(true, () => { if (!working) cancelRef.current?.click(); }, 60);
  return (
    <div
      {...props}
      className={`pi-inline-confirmation ${className}`.trim()}
      title={title ?? message}
    >
      <span aria-live="polite">{message}</span>
      <IconAction
        className="is-confirm"
        label="确认删除"
        disabled={working}
        onClick={(event) => {
          event.stopPropagation();
          onConfirm(event);
        }}
      >
        <Check size={14} />
      </IconAction>
      <IconAction
        ref={cancelRef}
        label="取消删除"
        disabled={working}
        onClick={(event) => {
          event.stopPropagation();
          onCancel(event);
        }}
      >
        <X size={14} />
      </IconAction>
    </div>
  );
}
