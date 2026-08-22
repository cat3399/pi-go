"use client";

import {
  forwardRef,
  type ButtonHTMLAttributes,
  type ReactNode,
  type TextareaHTMLAttributes,
} from "react";

interface ChatTextInputProps extends Omit<TextareaHTMLAttributes<HTMLTextAreaElement>, "className"> {
  frameClassName?: string;
  textareaClassName?: string;
  action: ReactNode;
}

export const ChatTextInput = forwardRef<HTMLTextAreaElement, ChatTextInputProps>(function ChatTextInput({
  frameClassName = "",
  textareaClassName = "",
  action,
  ...textareaProps
}, ref) {
  return (
    <div className={`chat-text-input ${frameClassName}`.trim()}>
      <textarea ref={ref} className={textareaClassName} {...textareaProps} />
      {action}
    </div>
  );
});

interface ChatPrimaryActionProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children"> {
  active: boolean;
  label: string;
  pending?: boolean;
}

export function ChatPrimaryAction({
  active,
  label,
  pending = false,
  className = "",
  ...buttonProps
}: ChatPrimaryActionProps) {
  return (
    <button
      {...buttonProps}
      type="button"
      className={`chat-input-primary-action ${active ? "is-active" : ""} ${className}`.trim()}
    >
      {pending ? (
        <svg className="chat-input-action-spinner" width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true">
          <circle cx="8" cy="8" r="6" stroke="currentColor" strokeWidth="2" opacity="0.25" />
          <path d="M14 8a6 6 0 0 0-6-6" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
        </svg>
      ) : (
        <svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <line x1="2" y1="7" x2="11" y2="7" />
          <polyline points="7.5 3 12 7 7.5 11" />
        </svg>
      )}
      <span>{label}</span>
    </button>
  );
}

interface ChatSecondaryActionProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children"> {
  label: string;
}

export function ChatSecondaryAction({
  label,
  className = "",
  ...buttonProps
}: ChatSecondaryActionProps) {
  return (
    <button
      {...buttonProps}
      type="button"
      className={`chat-input-secondary-action ${className}`.trim()}
    >
      <svg width="13" height="13" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" aria-hidden="true">
        <line x1="3" y1="3" x2="11" y2="11" />
        <line x1="11" y1="3" x2="3" y2="11" />
      </svg>
      <span>{label}</span>
    </button>
  );
}
