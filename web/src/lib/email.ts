import { api } from './api'

export type SMTPSecurity = 'starttls' | 'tls' | 'none'

export interface EmailSettings {
  host: string
  port: number
  security: SMTPSecurity
  username: string
  has_password: boolean
  from: string
  configured: boolean
}

export interface EmailSettingsInput {
  host: string
  port: number
  security: SMTPSecurity
  username: string
  /** Empty keeps the password set, unless clear_password. */
  password: string
  clear_password?: boolean
  from: string
}

export const saveEmailSettings = (v: EmailSettingsInput) =>
  api.put<EmailSettings>('/admin/email', v)

export const sendTestEmail = (to: string) =>
  api.post<{ sent: boolean }>('/admin/email/test', { to })
