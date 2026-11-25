export async function copyText(text: string): Promise<void> {
  if (window.isSecureContext && navigator.clipboard) return navigator.clipboard.writeText(text)
  const el = document.createElement('textarea')
  el.value = text
  el.setAttribute('readonly', '')
  el.style.cssText = 'position:fixed;opacity:0;pointer-events:none'
  document.body.appendChild(el)
  el.select()
  const ok = document.execCommand('copy')
  el.remove()
  if (!ok) throw new Error('Copying is blocked here; select the text and copy it yourself.')
}
