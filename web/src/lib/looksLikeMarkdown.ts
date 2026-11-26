// Whether pasted plain text is markdown the person means to have applied: a
// block of it (fence, heading, list, task, quote, table, rule) or inline
// markup. Prose, a lone URL and snake_case are not.
const BLOCK = [
  /^ {0,3}(?:`{3,}|~{3,})/m,
  /^ {0,3}#{1,6}[ \t]+\S/m,
  /^ {0,3}(?:[-*+]|\d{1,9}[.)])[ \t]+\S/m,
  /^ {0,3}>[ \t]?\S/m,
  /^ {0,3}(?:-{3,}|\*{3,}|_{3,})[ \t]*$/m,
  // A table: a row of cells over a row of dashes.
  /^.*\|.*\n[ \t]*\|?[ \t]*:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)+\|?[ \t]*$/m,
]
const INLINE = [
  // Not inside a word: "x**2 + y**2" is arithmetic.
  /(?<![\p{L}\p{N}_*])\*\*[^*\s](?:[^*\n]*[^*\s])?\*\*(?![\p{L}\p{N}_*])/u,
  /`[^`\n]+`/,
  /\[[^\]\n]+\]\([^)\s]+\)/,
  /~~[^~\s](?:[^~\n]*[^~\s])?~~/,
  /\[\[[^\]\n]+\]\]/,
]

export function looksLikeMarkdown(text: string): boolean {
  return BLOCK.some((re) => re.test(text)) || INLINE.some((re) => re.test(text))
}
