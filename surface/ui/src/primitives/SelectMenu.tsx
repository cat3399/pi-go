import {
  type CSSProperties,
  type KeyboardEvent,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";
import { Check, ChevronDown } from "lucide-react";

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
  variant?: "model" | "compact";
  showChevron?: boolean;
  onChange(value: string): void;
}

interface MenuPosition extends CSSProperties {
  maxHeight: number;
  minWidth: number;
}

const VIEWPORT_GUTTER = 8;
const MENU_GAP = 6;

export function SelectMenu({
  ariaLabel,
  value,
  options,
  placeholder = "请选择",
  disabled = false,
  variant = "compact",
  showChevron = true,
  onChange,
}: SelectMenuProps) {
  const [open, setOpen] = useState(false);
  const [activeIndex, setActiveIndex] = useState(0);
  const [position, setPosition] = useState<MenuPosition | null>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const optionRefs = useRef<Array<HTMLButtonElement | null>>([]);
  const menuId = useId();
  const selectedIndex = options.findIndex((option) => option.value === value);
  const selected = selectedIndex >= 0 ? options[selectedIndex] : undefined;

  const updatePosition = useCallback(() => {
    const trigger = triggerRef.current;
    if (!trigger) return;

    const rect = trigger.getBoundingClientRect();
    const spaceAbove = rect.top - VIEWPORT_GUTTER;
    const spaceBelow = window.innerHeight - rect.bottom - VIEWPORT_GUTTER;
    const placeAbove = spaceAbove >= 148 || spaceAbove >= spaceBelow;
    const available = Math.max(96, (placeAbove ? spaceAbove : spaceBelow) - MENU_GAP);
    const common = {
      right: Math.max(VIEWPORT_GUTTER, window.innerWidth - rect.right),
      maxHeight: Math.min(320, available),
      minWidth: rect.width,
    };

    setPosition(placeAbove
      ? { ...common, bottom: window.innerHeight - rect.top + MENU_GAP }
      : { ...common, top: rect.bottom + MENU_GAP });
  }, []);

  const openMenu = (preferredIndex?: number) => {
    if (disabled || options.length === 0) return;
    const nextIndex = preferredIndex ?? (selectedIndex >= 0 ? selectedIndex : 0);
    setActiveIndex(Math.max(0, Math.min(nextIndex, options.length - 1)));
    setOpen(true);
  };

  const closeMenu = useCallback((restoreFocus = false) => {
    setOpen(false);
    setPosition(null);
    if (restoreFocus) requestAnimationFrame(() => triggerRef.current?.focus());
  }, []);

  const choose = (index: number) => {
    const option = options[index];
    if (!option) return;
    if (option.value !== value) onChange(option.value);
    closeMenu(true);
  };

  useLayoutEffect(() => {
    if (!open) return;
    updatePosition();
    const frame = requestAnimationFrame(() => menuRef.current?.focus({ preventScroll: true }));
    return () => cancelAnimationFrame(frame);
  }, [open, updatePosition]);

  useEffect(() => {
    if (!open) return;

    const closeOnOutsideClick = (event: MouseEvent) => {
      const target = event.target as Node;
      if (!triggerRef.current?.contains(target) && !menuRef.current?.contains(target)) closeMenu();
    };
    const reposition = () => updatePosition();
    document.addEventListener("mousedown", closeOnOutsideClick);
    window.addEventListener("resize", reposition);
    window.addEventListener("scroll", reposition, true);
    return () => {
      document.removeEventListener("mousedown", closeOnOutsideClick);
      window.removeEventListener("resize", reposition);
      window.removeEventListener("scroll", reposition, true);
    };
  }, [closeMenu, open, updatePosition]);

  useEffect(() => {
    if (!open) return;
    optionRefs.current[activeIndex]?.scrollIntoView({ block: "nearest" });
  }, [activeIndex, open]);

  useEffect(() => {
    if (disabled || options.length === 0) closeMenu();
  }, [closeMenu, disabled, options.length]);

  const onTriggerKeyDown = (event: KeyboardEvent<HTMLButtonElement>) => {
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        openMenu(selectedIndex >= 0 ? selectedIndex : 0);
        break;
      case "ArrowUp":
        event.preventDefault();
        openMenu(selectedIndex >= 0 ? selectedIndex : options.length - 1);
        break;
      case "Home":
        event.preventDefault();
        openMenu(0);
        break;
      case "End":
        event.preventDefault();
        openMenu(options.length - 1);
        break;
      case "Enter":
      case " ":
        event.preventDefault();
        if (open) closeMenu(); else openMenu();
        break;
    }
  };

  const onMenuKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
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
        disabled={disabled || options.length === 0}
        onClick={() => {
          if (open) closeMenu(); else openMenu();
        }}
        onKeyDown={onTriggerKeyDown}
      >
        <span className="pi-select-trigger-label">{selected?.label ?? placeholder}</span>
        {showChevron && <ChevronDown className="pi-select-chevron" size={13} strokeWidth={1.7} />}
      </button>
      {open && position && createPortal(
        <div
          ref={menuRef}
          id={menuId}
          className={`pi-select-popover is-${variant}`}
          role="listbox"
          tabIndex={-1}
          aria-label={ariaLabel}
          aria-activedescendant={`${menuId}-option-${activeIndex}`}
          style={position}
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
        </div>,
        document.body,
      )}
    </>
  );
}
