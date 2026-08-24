import {
  type KeyboardEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { Check, ChevronDown } from "lucide-react";
import { AnchoredPopover } from "./AnchoredPopover";
import { useInputModality } from "./useInputModality";

export interface SelectMenuOption {
  value: string;
  label: string;
}

interface SelectMenuProps {
  ariaLabel: string;
  value: string;
  options: SelectMenuOption[];
  placeholder?: string;
  disabled?: boolean;
  variant?: "model" | "compact" | "project";
  showChevron?: boolean;
  leadingIcon?: ReactNode;
  onChange(value: string): void;
}

export function SelectMenu({
  ariaLabel,
  value,
  options,
  placeholder = "请选择",
  disabled = false,
  variant = "compact",
  showChevron = true,
  leadingIcon,
  onChange,
}: SelectMenuProps) {
  const [open, setOpen] = useState(false);
  const [activeIndex, setActiveIndex] = useState(0);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const optionRefs = useRef<Array<HTMLButtonElement | null>>([]);
  const {
    modality,
    modalityRef,
    markKeyboard,
    markPointer,
    suppressPointerFocus,
  } = useInputModality();
  const menuId = useId();
  const selectedIndex = options.findIndex((option) => option.value === value);
  const selected = selectedIndex >= 0 ? options[selectedIndex] : undefined;

  const focusMenu = useCallback(() => {
    requestAnimationFrame(() => menuRef.current?.focus({ preventScroll: true }));
  }, []);

  const openMenu = (preferredIndex?: number, moveFocus = false) => {
    if (disabled || options.length === 0) return;
    const nextIndex = preferredIndex ?? (selectedIndex >= 0 ? selectedIndex : 0);
    setActiveIndex(Math.max(0, Math.min(nextIndex, options.length - 1)));
    setOpen(true);
    if (open && moveFocus) focusMenu();
  };

  const closeMenu = useCallback((restoreFocus = false) => {
    setOpen(false);
    if (restoreFocus) requestAnimationFrame(() => triggerRef.current?.focus());
  }, []);

  const choose = (index: number) => {
    const option = options[index];
    if (!option) return;
    if (option.value !== value) onChange(option.value);
    closeMenu(modalityRef.current === "keyboard");
  };

  useLayoutEffect(() => {
    if (!open || modality !== "keyboard") return;
    const frame = requestAnimationFrame(() => menuRef.current?.focus({ preventScroll: true }));
    return () => cancelAnimationFrame(frame);
  }, [modality, open]);

  useEffect(() => {
    if (!open) return;
    optionRefs.current[activeIndex]?.scrollIntoView({ block: "nearest" });
  }, [activeIndex, open]);

  useEffect(() => {
    if (disabled || options.length === 0) closeMenu();
  }, [closeMenu, disabled, options.length]);

  const onTriggerKeyDown = (event: KeyboardEvent<HTMLButtonElement>) => {
    markKeyboard();
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        openMenu(selectedIndex >= 0 ? selectedIndex : 0, true);
        break;
      case "ArrowUp":
        event.preventDefault();
        openMenu(selectedIndex >= 0 ? selectedIndex : options.length - 1, true);
        break;
      case "Home":
        event.preventDefault();
        openMenu(0, true);
        break;
      case "End":
        event.preventDefault();
        openMenu(options.length - 1, true);
        break;
      case "Enter":
      case " ":
        event.preventDefault();
        if (open) closeMenu(); else openMenu(undefined, true);
        break;
      case "Tab":
        if (open) closeMenu();
        break;
    }
  };

  const onMenuKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    markKeyboard();
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        setActiveIndex((index) => (index + 1) % options.length);
        break;
      case "ArrowUp":
        event.preventDefault();
        setActiveIndex((index) => (index - 1 + options.length) % options.length);
        break;
      case "Home":
        event.preventDefault();
        setActiveIndex(0);
        break;
      case "End":
        event.preventDefault();
        setActiveIndex(options.length - 1);
        break;
      case "Enter":
      case " ":
        event.preventDefault();
        choose(activeIndex);
        break;
      case "Escape":
        event.preventDefault();
        closeMenu(true);
        break;
      case "Tab":
        closeMenu();
        break;
    }
  };

  return (
    <>
      <button
        ref={triggerRef}
        className={`pi-select-trigger is-${variant} ${open ? "is-open" : ""}`}
        type="button"
        aria-label={ariaLabel}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        title={selected?.label ?? placeholder}
        disabled={disabled || options.length === 0}
        onPointerDown={markPointer}
        onClick={() => {
          if (open) closeMenu(); else openMenu();
        }}
        onKeyDown={onTriggerKeyDown}
      >
        {leadingIcon && <span className="pi-select-trigger-icon">{leadingIcon}</span>}
        <span className="pi-select-trigger-label">{selected?.label ?? placeholder}</span>
        {showChevron && <ChevronDown className="pi-select-chevron" size={13} strokeWidth={1.7} />}
      </button>
      <AnchoredPopover
        ref={menuRef}
        anchorRef={triggerRef}
        open={open}
        id={menuId}
        className={`pi-select-popover is-${variant}`}
        role="listbox"
        tabIndex={-1}
        data-focus-modality={modality}
        aria-label={ariaLabel}
        aria-activedescendant={`${menuId}-option-${activeIndex}`}
        minWidth="anchor"
        onDismiss={() => closeMenu()}
        onPointerDownCapture={markPointer}
        onMouseDownCapture={suppressPointerFocus}
        onKeyDown={onMenuKeyDown}
      >
        {options.map((option, index) => {
          const selectedOption = option.value === value;
          return (
            <button
              ref={(element) => { optionRefs.current[index] = element; }}
              id={`${menuId}-option-${index}`}
              className={`${index === activeIndex ? "is-active" : ""} ${selectedOption ? "is-selected" : ""}`}
              type="button"
              role="option"
              tabIndex={-1}
              aria-selected={selectedOption}
              key={option.value}
              onMouseMove={() => setActiveIndex(index)}
              onClick={() => choose(index)}
            >
              <span>{option.label}</span>
              {selectedOption && <Check size={14} strokeWidth={1.8} />}
            </button>
          );
        })}
      </AnchoredPopover>
    </>
  );
}
