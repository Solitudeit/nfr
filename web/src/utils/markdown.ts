function escapeHTML(raw: string): string {
  return raw
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;')
}

function renderInline(raw: string): string {
  const codeTokens: string[] = []
  let text = escapeHTML(raw).replace(/`([^`]+)`/g, (_, code: string) => {
    const idx = codeTokens.push(`<code>${code}</code>`) - 1
    return `\u0000CODE${idx}\u0000`
  })

  // Markdown links: [label](https://...)
  text = text.replace(
    /\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g,
    '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>'
  )

  // Bare http(s) URLs not already inside an attribute
  text = text.replace(
    /(^|[\s(>：:])((https?:\/\/[^\s<)]+))/g,
    '$1<a href="$2" target="_blank" rel="noopener noreferrer">$2</a>'
  )

  // Bold **text**
  text = text.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')

  // Restore inline code spans
  text = text.replace(/\u0000CODE(\d+)\u0000/g, (_, idx: string) => codeTokens[Number(idx)] || '')
  return text
}

/**
 * Renders GitHub Release Markdown notes into clean, strictly XSS-safe HTML
 * (headings, summary blockquotes, bullet lists, inline code, bold, and links).
 */
export function renderReleaseNotesMarkdown(md: string): string {
  const lines = md.replace(/\r\n?/g, '\n').split('\n')
  const out: string[] = []
  let inList = false
  let inQuote = false

  const closeList = () => {
    if (inList) {
      out.push('</ul>')
      inList = false
    }
  }
  const closeQuote = () => {
    if (inQuote) {
      out.push('</blockquote>')
      inQuote = false
    }
  }

  for (const rawLine of lines) {
    const trimmed = rawLine.trim()
    if (!trimmed) {
      closeList()
      closeQuote()
      continue
    }

    // Blockquote summary (> ...)
    if (trimmed.startsWith('>')) {
      closeList()
      const content = trimmed.replace(/^>\s?/, '')
      if (!inQuote) {
        out.push('<blockquote>')
        inQuote = true
      }
      out.push(`<p>${renderInline(content)}</p>`)
      continue
    }
    closeQuote()

    // Headings (# / ## / ### / ####)
    const headingMatch = trimmed.match(/^#{1,4}\s+(.+)$/)
    if (headingMatch) {
      closeList()
      out.push(`<h4>${renderInline(headingMatch[1])}</h4>`)
      continue
    }

    // Bullet list items (- item / * item, including nested indented sub-items)
    const listMatch = rawLine.match(/^(\s*)[-*]\s+(.+)$/)
    if (listMatch) {
      if (!inList) {
        out.push('<ul>')
        inList = true
      }
      const isNested = listMatch[1].length >= 2
      out.push(`<li${isNested ? ' class="sub-item"' : ''}>${renderInline(listMatch[2])}</li>`)
      continue
    }
    closeList()

    // Compact footer styling for GitHub's "Full Changelog" line
    const changelogMatch = trimmed.match(/^\*\*Full Changelog\*\*:\s*(https?:\/\/\S+)$/i)
    if (changelogMatch) {
      const url = escapeHTML(changelogMatch[1])
      out.push(
        `<p class="md-changelog"><a href="${url}" target="_blank" rel="noopener noreferrer">查看完整提交对比 (Full Changelog) ↗</a></p>`
      )
      continue
    }

    out.push(`<p>${renderInline(trimmed)}</p>`)
  }

  closeList()
  closeQuote()
  return out.join('')
}
