import { useLang } from '../lib/i18n'

export type Member = { monitorId: number; target: string; priority: number }
export type EventKind = 'down' | 'slow' | 'recover' | 'route' | 'error' | 'all_down'
export type Group = {
  id: number
  revision: number
  name: string
  zone: string
  recordName: string
  domain: string
  kind: 'CNAME' | 'A'
  ttl: number
  proxied: boolean
  enabled: boolean
  downSeconds: number
  slowMs: number
  slowSeconds: number
  recoverSeconds: number
  cooldownSeconds: number
  keepLast: boolean
  members: Member[]
  notify: boolean
  channels: number[]
  events: EventKind[]
}
export type Health = Member & {
  name: string
  state: 'up' | 'down' | 'slow' | 'unknown'
  ping: number | null
  since: number
  at: number
}
export type View = Group & {
  health: Health[]
  current: string
  simulated: string
  lastError: string
  cloud: { operation: string; message: string; failedAt: number; retryAt: number; verifiedAt: number }
  errorAlerts: boolean
  lastEvaluation: number
  lastSwitch: number
  reason: string
}
export type HistoryEvent = {
  id: number
  at: number
  reason: EventKind
  status: 'changed' | 'pending' | 'simulated' | 'failed' | 'held' | 'uncertain'
  before: string
  after: string
  fromMonitor: string
  toMonitor: string
  message: string
}
export type Preferences = { dryRun: boolean; tokenConfigured: boolean }
export type Index = Preferences & { groups: View[] }
export type Localize = (en: string, fa: string) => string

export function useLocalize(): Localize {
  const language = useLang((s) => s.lang)
  return (en, fa) => language === 'fa' ? fa : en
}

export const eventKinds: EventKind[] = ['down', 'slow', 'recover', 'route', 'error', 'all_down']

export function eventLabel(t: Localize, event: string) {
  switch (event) {
    case 'down': return t('Down', 'قطعی')
    case 'slow': return t('High latency', 'کندی')
    case 'recover': return t('Preferred route restored', 'بازگشت به مسیر ترجیحی')
    case 'route': return t('Route selected', 'انتخاب مسیر')
    case 'error': return t('Cloudflare error', 'خطای کلادفلر')
    case 'all_down': return t('All members down', 'قطعی تمام اعضا')
    case 'cooldown': return t('Cooldown', 'انتظار بین تغییرها')
    case 'disabled': return t('Disabled', 'غیرفعال')
    default: return ''
  }
}

export function groupFrom(view?: View): Group {
  const initial: Group = {
    id: 0, revision: 0, name: '', zone: '', recordName: '', domain: '', kind: 'CNAME',
    ttl: 60, proxied: false, enabled: false, downSeconds: 30, slowMs: 0,
    slowSeconds: 45, recoverSeconds: 90, cooldownSeconds: 120, keepLast: true,
    members: [{ monitorId: 0, target: '', priority: 1 }, { monitorId: 0, target: '', priority: 2 }],
    notify: false, channels: [], events: ['down', 'slow', 'recover', 'error'],
  }
  if (!view) return initial
  const { cloud: _cloud, errorAlerts: _alerts, health: _health, current: _current, simulated: _simulated, lastError: _error,
    lastEvaluation: _evaluation, lastSwitch: _switch, reason: _reason, ...group } = view
  return group
}

export function recordName(group: Group) {
  return group.recordName === '@' ? group.domain : [group.recordName, group.domain].filter(Boolean).join('.')
}

export function errorText(error: unknown) {
  return error instanceof Error ? error.message : String(error)
}
