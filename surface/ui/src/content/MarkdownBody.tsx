import { MouseEvent, useMemo, useState } from "react";
import { Check, Copy } from "lucide-react";
import ReactMarkdown, { type Components } from "react-markdown";
import "katex/dist/katex.min.css";
import {
  markdownRehypePlugins,
  markdownRemarkPlugins,
  normalizeDisplayMath,
} from "../markdown";

interface MarkdownBodyProps {
  children: string;
  isStreaming?: boolean;
}

function CodeBlock({ code, language }: { code: string; language: string }) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    await navigator.clipboard.writeText(code);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  };

  return (
    <div className="pi-markdown-code">
      <div className="pi-markdown-code-header">
        <span>{language === "bash" ? "Bash" : language === "shell" ? "Shell" : language === "text" || !language ? "纯文本" : language}</span>
        <button type="button" aria-label={copied ? "已复制" : "复制"} title={copied ? "已复制" : "复制"} onClick={() => void copy()}>
          {copied ? <Check size={14} /> : <Copy size={14} />}
        </button>
      </div>
      <pre><code>{code}</code></pre>
    </div>
  );
}

export function MarkdownBody({ children, isStreaming }: MarkdownBodyProps) {
  const normalized = useMemo(() => normalizeDisplayMath(children), [children]);
  const components = useMemo<Components>(() => ({
    code({ className, children: codeChildren, ...props }) {
      const language = className?.replace("language-", "").toLowerCase() ?? "";
      const raw = String(codeChildren);
      const block = className?.includes("language-") || raw.includes("\n");
      if (block) return <CodeBlock code={raw.replace(/\n$/, "")} language={language} />;
      return <code className="pi-markdown-inline-code" {...props}>{codeChildren}</code>;
    },
    pre({ children: preChildren }) {
      return <>{preChildren}</>;
    },
    a({ href, children: linkChildren, ...props }) {
      delete props.node;
      const handleClick = (event: MouseEvent<HTMLAnchorElement>) => {
        if (!href || isStreaming) event.preventDefault();
      };
      return (
        <a
          href={href}
          {...props}
          target="_blank"
          rel="noopener noreferrer"
          onClick={handleClick}
        >
          {linkChildren}
        </a>
      );
    },
    img({ src, alt, ...props }) {
      delete props.node;
      return <img src={src} alt={alt ?? ""} loading="lazy" {...props} />;
    },
    table({ children: tableChildren }) {
      return <div className="pi-markdown-table"><table>{tableChildren}</table></div>;
    },
  }), [isStreaming]);

  return (
    <div className="pi-markdown">
      <ReactMarkdown
        remarkPlugins={markdownRemarkPlugins}
        rehypePlugins={markdownRehypePlugins}
        components={components}
      >
        {normalized}
      </ReactMarkdown>
    </div>
  );
}
