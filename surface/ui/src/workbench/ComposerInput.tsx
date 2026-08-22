import {
  forwardRef,
  type ButtonHTMLAttributes,
  type ReactNode,
  type TextareaHTMLAttributes,
} from "react";
import { ArrowUp, LoaderCircle, X } from "lucide-react";

interface ComposerInputProps extends Omit<TextareaHTMLAttributes<HTMLTextAreaElement>, "className"> {
  className?: string;
  textareaClassName?: string;
  leading?: ReactNode;
  toolbarLeft?: ReactNode;
  toolbarRight: ReactNode;
}

export const ComposerInput = forwardRef<HTMLTextAreaElement, ComposerInputProps>(function ComposerInput({
  className = "",
  textareaClassName,
  leading,
  toolbarLeft,
  toolbarRight,
  ...textareaProps
}, ref) {
  return (
    <div className={`pi-composer ${className}`.trim()}>
      {leading}
      <textarea ref={ref} className={textareaClassName} {...textareaProps} />
      <div className="pi-composer-toolbar">
        <div className="pi-composer-left">{toolbarLeft}</div>
        <div className="pi-composer-meta">{toolbarRight}</div>
      </div>
    </div>
  );
});

interface ComposerSendButtonProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children"> {
  submitting?: boolean;
}

export function ComposerSendButton({
  className = "",
  submitting = false,
  ...buttonProps
}: ComposerSendButtonProps) {
  return (
    <button {...buttonProps} className={`pi-send-button ${className}`.trim()} type="button">
      {submitting
        ? <LoaderCircle className="pi-submit-loading" size={17} />
        : <ArrowUp size={18} strokeWidth={2.1} />}
    </button>
  );
}

interface ComposerCancelButtonProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children"> {
  label?: string;
}

export function ComposerCancelButton({
  className = "",
  label = "取消",
  ...buttonProps
}: ComposerCancelButtonProps) {
  return (
    <button {...buttonProps} className={`pi-composer-cancel-button ${className}`.trim()} type="button">
      <X size={14} strokeWidth={1.9} />
      <span>{label}</span>
    </button>
  );
}
