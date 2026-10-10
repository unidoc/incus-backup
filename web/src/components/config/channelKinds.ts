import type { ChannelKind } from '@/api/sections/config'
import type { CfgIconName } from './icons'

export const KIND_META: Record<ChannelKind, { label: string; short: string; icon: CfgIconName }> = {
  smtp: { label: 'E-mail', short: 'SMTP', icon: 'mail' },
  webhook: { label: 'Webhook', short: 'JSON, Slack, Discord', icon: 'webhook' },
  ntfy: { label: 'ntfy', short: 'push to phone', icon: 'bell-ring' },
  gotify: { label: 'Gotify', short: 'push to phone', icon: 'message-square' },
}
