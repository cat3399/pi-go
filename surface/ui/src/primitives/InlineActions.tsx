import { forwardRef, type ButtonHTMLAttributes, type HTMLAttributes, type ReactNode } from "react";

interface InlineActionsProps extends HTMLAttributes<HTMLDivElement> {
  visible?: boolean;
}

interface IconActionProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "aria-label"> {
  label: string;
  children: ReactNode;
}

export function InlineActions({ visible = false, className = "", ...props }: InlineActionsProps) {
  return (
    <div
      {...props}
      className={`pi-inline-actions ${visible ? "is-visible" : ""} ${className}`.trim()}
    />
  );
}

export const IconAction = forwardRef<HTMLButtonElement, IconActionProps>(function IconAction({
  label,
  className = "",
  title,
  type = "button",
  ...props
}, ref) {
  return (
    <button
      {...props}
      ref={ref}
      className={`pi-icon-action ${className}`.trim()}
      type={type}
      aria-label={label}
      title={title ?? label}
    />
  );
});
