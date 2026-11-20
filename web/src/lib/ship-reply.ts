// Admin → AI: the template wp ship gives an agent for its last message
// (WP-390), so wrap-ups say what shipped and stop.

import { api } from './api'

export interface ShipReply {
  reply: string
  default: string
  /** An admin worded it. */
  custom: boolean
}

export const saveShipReply = (reply: string) => api.put<ShipReply>('/admin/ship-reply', { reply })

/** The template as an agent sees it, for an example pull request. */
export const renderShipReply = (tmpl: string) =>
  tmpl
    .replaceAll('{task}', 'WP-123')
    .replaceAll('{pr}', '45')
    .replaceAll('{url}', 'https://git.example.com/acme/app/pulls/45')
