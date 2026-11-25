import { renderMarkdownForEdit } from '@/lib/markdown'
import { htmlToMarkdown } from '@/lib/richtext'

// The editor's schema reads its own HTML shapes where renderMarkdownForEdit's
// differ. Tasks are the one place: marked writes `<li><input class="task"> text`
// in a list, Tiptap's taskList/taskItem is `ul[data-type=taskList] >
// li[data-type=taskItem][data-checked] > div`. A list that mixes task and plain
// items is split into runs of one kind going in and joined again coming out,
// so it saves as the one list it was.

const isTask = (li: Element) =>
  Array.from(li.children).some((c) => c.tagName === 'INPUT' && c.classList.contains('task'))

/** Markdown as HTML the editor's schema parses. */
export function toEditorHtml(md: string, opts: { blob?: boolean } = {}): string {
  const tpl = document.createElement('template')
  tpl.innerHTML = renderMarkdownForEdit(md, opts)
  for (const ul of Array.from(tpl.content.querySelectorAll('ul'))) {
    const items = Array.from(ul.children).filter((c) => c.tagName === 'LI')
    if (!items.some(isTask)) continue
    // Anything but items between them (not valid HTML, so not marked's) would
    // be lost in the split.
    if (items.length !== ul.children.length) continue
    const runs: HTMLUListElement[] = []
    for (const li of items) {
      const task = isTask(li)
      let run = runs[runs.length - 1]
      if (!run || (run.dataset.type === 'taskList') !== task) {
        run = document.createElement('ul')
        if (task) run.dataset.type = 'taskList'
        runs.push(run)
      }
      run.append(task ? taskItem(li) : li)
    }
    ul.replaceWith(...runs)
  }
  // A block of code ends in the newline the renderer adds; the editor's block
  // holds the code alone, and htmlToMarkdown puts the one newline back.
  for (const code of Array.from(tpl.content.querySelectorAll('pre > code'))) {
    const last = code.lastChild
    if (last && last.nodeType === Node.TEXT_NODE && (last as Text).data.endsWith('\n'))
      (last as Text).data = (last as Text).data.slice(0, -1)
  }
  return tpl.innerHTML
}

function taskItem(li: Element) {
  const box = Array.from(li.children).find((c) => c.tagName === 'INPUT') as HTMLInputElement
  const out = document.createElement('li')
  out.dataset.type = 'taskItem'
  out.dataset.checked = String(box.hasAttribute('checked'))
  box.remove()
  const body = document.createElement('div')
  body.append(...Array.from(li.childNodes))
  const first = body.firstChild
  if (first && first.nodeType === Node.TEXT_NODE)
    (first as Text).data = (first as Text).data.trimStart()
  out.append(body)
  return out
}

/** The markdown behind the editor's HTML. */
export function fromEditorHtml(html: string): string {
  const tpl = document.createElement('template')
  tpl.innerHTML = html
  const joined = new Set<Element>()
  for (const ul of Array.from(tpl.content.querySelectorAll('ul[data-type=taskList]'))) {
    for (const li of Array.from(ul.children)) {
      if (li.tagName !== 'LI') continue
      const box = document.createElement('input')
      box.type = 'checkbox'
      box.className = 'task'
      if (li.getAttribute('data-checked') === 'true') box.setAttribute('checked', '')
      const body = li.querySelector(':scope > div')
      const kids = body ? Array.from(body.childNodes) : []
      li.replaceChildren(box, ...kids)
      li.removeAttribute('data-type')
      li.removeAttribute('data-checked')
    }
    ul.removeAttribute('data-type')
    joined.add(ul)
  }
  // Task and plain items next to each other were one list.
  for (const ul of Array.from(tpl.content.querySelectorAll('ul'))) {
    let next = ul.nextElementSibling
    while (
      ul.isConnected &&
      next &&
      next.tagName === 'UL' &&
      ul.nextSibling === next &&
      (joined.has(ul) || joined.has(next))
    ) {
      ul.append(...Array.from(next.childNodes))
      if (joined.has(next)) joined.add(ul)
      next.remove()
      next = ul.nextElementSibling
    }
  }
  return htmlToMarkdown(tpl.innerHTML)
}
