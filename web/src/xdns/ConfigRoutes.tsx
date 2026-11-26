import { useCallback, useEffect, useRef, useState } from 'react'
import { Navigate } from 'react-router-dom'
import { Activity, BellOff, ChartNoAxesColumn, Cloud, History, Pencil, Plus, Route, Trash2, TriangleAlert } from 'lucide-react'
import { Badge, Button, Checkbox, Field, Input, Modal, Spinner, confirm, toast } from '../components/ui'
import { api } from '../lib/api'
import { fmtDate, fmtTimeSec } from '../lib/format'
import { useApp } from '../lib/store'
import GroupEditor from './GroupEditor'
import RouteInsights from './RouteInsights'
import { errorText, eventLabel, groupFrom, recordName, useLocalize, type Group, type HistoryEvent, type Index, type Preferences, type View } from './types'

const base = '/api/xdns'

function CloudSettings({ initial, onClose, onSaved }: { initial: Preferences; onClose: () => void; onSaved: () => void }) {
  const t = useLocalize()
  const [token, setToken] = useState('')
  const [dryRun, setDryRun] = useState(initial.dryRun)
  const [remove, setRemove] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function save() {
    setBusy(true)
    setError('')
    if (initial.dryRun && !dryRun && !await confirm({
      title: t('Enable live DNS changes?', 'تغییر واقعی DNS فعال شود؟'),
      body: t('Enabled groups can update their Cloudflare record as soon as the next evaluation runs.', 'گروه‌های فعال می‌توانند از ارزیابی بعدی رکوردهای کلادفلر را تغییر دهند.'),
      confirmLabel: t('Enable live changes', 'فعال‌سازی تغییر واقعی'),
    })) {
      setBusy(false)
      return
    }
    try {
      const input: { dryRun: boolean; token?: string } = { dryRun }
      if (remove || token.trim()) input.token = remove ? '' : token.trim()
      await api.put(`${base}/settings`, input)
      onSaved()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open onClose={() => { if (!busy) onClose() }} title={t('Cloudflare settings', 'تنظیمات کلادفلر')}
      footer={<><Button onClick={onClose} disabled={busy}>{t('Cancel', 'انصراف')}</Button>
        <Button form="xdns-settings" type="submit" variant="primary" loading={busy}>{t('Save settings', 'ذخیرهٔ تنظیمات')}</Button></>}>
      <form id="xdns-settings" className="space-y-4" onSubmit={(e) => { e.preventDefault(); void save() }}>
        <Field htmlFor="xdns-token" label={t('Cloudflare API token', 'توکن API کلادفلر')}
          help={t('Use DNS Edit on the zones managed by these groups.', 'برای Zoneهای تحت مدیریت، دسترسی DNS Edit بدهید.')}>
          <Input id="xdns-token" autoComplete="new-password" dir="ltr" type="password" maxLength={128}
            placeholder={initial.tokenConfigured ? t('Stored · enter a replacement', 'ذخیره شده · برای جایگزینی توکن جدید وارد کنید') : 'Cloudflare API token'}
            value={token} disabled={remove} onChange={(e) => setToken(e.target.value)} />
        </Field>
        {initial.tokenConfigured && <Checkbox checked={remove} onChange={(value) => { setRemove(value); if (value) setDryRun(true) }}
          label={t('Remove stored token', 'حذف توکن ذخیره‌شده')} />}
        <div className="rounded-xl border border-line bg-surface-2/60 p-3">
          <Checkbox checked={dryRun} onChange={(value) => { setDryRun(value); if (!value) setRemove(false) }}
            label={t('Dry run', 'اجرای آزمایشی')} />
          <p className="mt-1 text-xs text-muted">{t('Preview routing decisions in history without applying DNS changes.', 'تصمیم‌های مسیریابی در تاریخچه ثبت می‌شوند و DNS تغییر نمی‌کند.')}</p>
        </div>
        {error && <p className="text-sm text-down" role="alert">{error}</p>}
      </form>
    </Modal>
  )
}

function SwitchHistory({ group, onClose }: { group: Group; onClose: () => void }) {
  const t = useLocalize()
  const [items, setItems] = useState<HistoryEvent[]>([])
  const [next, setNext] = useState(0)
  const [busy, setBusy] = useState(true)
  const [error, setError] = useState('')

  const load = useCallback(async (before = 0) => {
    setBusy(true)
    setError('')
    try {
      const data = await api.get<{ events: HistoryEvent[]; next: number }>(`${base}/groups/${group.id}/history?before=${before}`)
      setItems((prev) => before ? [...prev, ...data.events] : data.events)
      setNext(data.next)
    } catch (e) {
      setError(errorText(e))
    } finally {
      setBusy(false)
    }
  }, [group.id])
  useEffect(() => { void load() }, [load])

  const statuses = {
    changed: { tone: 'up', label: t('Changed', 'تغییر کرد') },
    pending: { tone: 'pending', label: t('Awaiting confirmation', 'در انتظار تأیید') },
    simulated: { tone: 'accent', label: t('Simulated', 'آزمایشی') },
    failed: { tone: 'down', label: t('Failed', 'ناموفق') },
    held: { tone: 'neutral', label: t('Held', 'حفظ شد') },
    uncertain: { tone: 'pending', label: t('Uncertain', 'نامشخص') },
  } as const

  return (
    <Modal open onClose={onClose} title={t('Switch history', 'تاریخچهٔ تغییر مسیر')} description={group.name} size="lg"
      footer={<><Button loading={busy} onClick={() => void load()}>{t('Refresh', 'به‌روزرسانی')}</Button>
        <Button onClick={onClose}>{t('Close', 'بستن')}</Button></>}>
      {busy && items.length === 0 && <Spinner />}
      {!busy && !error && items.length === 0 && <p className="text-sm text-muted">{t('No events recorded for this group.', 'هنوز رویدادی برای این گروه ثبت نشده است.')}</p>}
      {error && <p className="text-sm text-down" role="alert">{error}</p>}
      <ol className="space-y-3">
        {items.map((item) => (
          <li key={item.id} className="rounded-xl border border-line bg-surface-2/50 p-3">
            <div className="flex flex-wrap items-center gap-2">
              <Badge tone={statuses[item.status].tone}>{statuses[item.status].label}</Badge>
              <span className="text-sm font-medium">{eventLabel(t, item.reason)}</span>
              <span className="ms-auto text-xs text-muted">{fmtDate(item.at)} · {fmtTimeSec(item.at)}</span>
            </div>
            {(item.before || item.after) && <div dir="ltr" className="mt-2 flex flex-wrap gap-2 font-mono text-xs [overflow-wrap:anywhere]">
              <span>{item.before || '—'}</span>{item.after && <><span className="text-muted">→</span><span>{item.after}</span></>}
            </div>}
            {(item.fromMonitor || item.toMonitor) && <div className="mt-1 text-xs text-muted">
              <bdi>{item.fromMonitor}</bdi>{item.toMonitor && <> → <bdi>{item.toMonitor}</bdi></>}
            </div>}
            {item.message && <p className="mt-2 text-xs leading-relaxed text-muted [overflow-wrap:anywhere]">{item.message}</p>}
          </li>
        ))}
      </ol>
      {next > 0 && <Button className="mt-4" loading={busy} onClick={() => void load(next)}>{t('Older events', 'رویدادهای قدیمی‌تر')}</Button>}
    </Modal>
  )
}

function RouteCard({ view, edit, history, remove, dryRun }: { view: View; edit: () => void; history: () => void; remove: () => void; dryRun: boolean }) {
  const [insights, setInsights] = useState(false)
  const notifications = useApp((s) => s.notifications)
  const alerts = view.errorAlerts && view.channels.some((id) => notifications.some((n) => n.id === id && n.active)) && !dryRun
  const t = useLocalize()
  const monitors = useApp((s) => s.monitors)
  const health = new Map(view.health.map((h) => [h.monitorId, h]))
  const statuses = {
    up: { label: t('Up', 'سالم'), tone: 'up' },
    down: { label: t('Down', 'قطع'), tone: 'down' },
    slow: { label: t('Slow', 'کند'), tone: 'pending' },
    unknown: { label: t('Unknown', 'نامشخص'), tone: 'neutral' },
  } as const

  return (
    <article id={`route-${view.id}`} className={`scroll-mt-4 overflow-hidden rounded-2xl border bg-surface shadow-sm ${view.lastError ? 'border-down/45' : 'border-line'}`}>
      <div className="flex flex-wrap items-start justify-between gap-4 border-b border-line p-4 sm:p-5">
        <div className="min-w-0 space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <h2 className="break-words text-lg font-semibold">{view.name}</h2>
            <Badge tone={view.enabled ? 'up' : 'neutral'}>{view.enabled ? t('Enabled', 'فعال') : t('Disabled', 'غیرفعال')}</Badge>
            {view.lastError && <Badge tone="down"><TriangleAlert className="me-1 size-3" aria-hidden />{t('Cloudflare issue', 'خطای کلادفلر')}</Badge>}
          </div>
          <div className="flex flex-wrap items-center gap-2 text-sm text-muted">
            <Badge tone="outline" mono>{view.kind}</Badge>
            <bdi className="break-all font-mono">{recordName(view)}</bdi>
          </div>
        </div>
        <div className="flex flex-wrap gap-1.5">
          <Button size="sm" onClick={edit} icon={<Pencil className="size-3.5" />}>{t('Edit', 'ویرایش')}</Button>
          <Button size="sm" aria-expanded={insights} onClick={() => setInsights((on) => !on)} icon={<ChartNoAxesColumn className="size-3.5" />}>{t('Insights', 'آمار')}</Button>
          <Button size="sm" onClick={history} icon={<History className="size-3.5" />}>{t('History', 'تاریخچه')}</Button>
          <Button size="sm" variant="ghost" onClick={remove} aria-label={t('Delete group', 'حذف گروه')} icon={<Trash2 className="size-3.5" />} />
        </div>
      </div>
      <div className="flex flex-wrap items-center gap-x-5 gap-y-2 bg-surface-2/50 px-4 py-3 sm:px-5">
        <div className="min-w-0 text-sm text-muted">
          {t('Current target', 'مقصد فعلی')} <bdi className="ms-2 break-all font-mono text-fg">{view.current || '—'}</bdi>
        </div>
        {view.simulated && <div className="min-w-0 text-sm text-accent-ink">
          {t('Simulated', 'مقصد آزمایشی')} <bdi className="ms-2 break-all font-mono">{view.simulated}</bdi>
        </div>}
        {view.reason && view.reason !== 'disabled' && <Badge tone="outline">{eventLabel(t, view.reason)}</Badge>}
        {!!view.lastEvaluation && <span className="ms-auto text-xs text-muted">{t('Evaluated', 'آخرین ارزیابی')} {fmtTimeSec(view.lastEvaluation)}</span>}
      </div>
      {view.lastError && <aside role="status" className="border-t border-down/20 bg-down/8 px-4 py-3 text-sm sm:px-5">
        <div className="flex flex-wrap items-center gap-2 font-semibold text-down">
          <TriangleAlert className="size-4" aria-hidden />
          {view.cloud?.operation === 'update' ? t('DNS update failed', 'تغییر DNS ناموفق بود') : t('Cloudflare lookup failed', 'خواندن رکورد از کلادفلر ناموفق بود')}
        </div>
        <p className="mt-1 break-words text-sm text-fg"><bdi>{view.lastError}</bdi></p>
        <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
          {!!view.cloud?.failedAt && <span>{t('Last error', 'آخرین خطا')} · {fmtTimeSec(view.cloud.failedAt)}</span>}
          {!!view.cloud?.verifiedAt && <span>{t('DNS last verified', 'آخرین تأیید DNS')} · {fmtTimeSec(view.cloud.verifiedAt)}</span>}
          {view.cloud?.retryAt > Date.now() && <span>{t('Retry after', 'زمان تلاش بعدی')} · {fmtTimeSec(view.cloud.retryAt)}</span>}
          {alerts && <span>{t('Error alerts enabled', 'اعلان خطا فعال است')}</span>}
        </div>
        {!alerts && <Button className="mt-2" size="sm" onClick={edit} icon={<BellOff className="size-3.5" />}>
          {dryRun ? t('Alerts paused in Dry run', 'اعلان‌ها در اجرای آزمایشی متوقف‌اند') : t('Configure error alerts', 'تنظیم اعلان خطا')}
        </Button>}
      </aside>}
      <div className="overflow-x-auto">
        <table className="w-full min-w-[30rem] text-start text-sm">
          <thead className="border-y border-line text-xs text-muted">
            <tr>
              {[t('Monitor', 'مانیتور'), t('Target', 'مقصد'), t('Priority', 'اولویت'), t('Health', 'سلامت'), t('Ping', 'پینگ')].map((label) =>
                <th key={label} scope="col" className="px-4 py-2 text-start font-medium sm:px-5">{label}</th>)}
            </tr>
          </thead>
          <tbody className="divide-y divide-line">
            {view.members.map((m) => {
              const h = health.get(m.monitorId)
              const state = statuses[h?.state ?? 'unknown']
              return (
                <tr key={m.monitorId} className={view.current === m.target ? 'bg-accent-soft/35' : undefined}>
                  <td className="px-4 py-2.5 font-medium sm:px-5">{h?.name || monitors[m.monitorId]?.name || `#${m.monitorId}`}</td>
                  <td className="px-4 py-2.5 font-mono text-xs sm:px-5"><bdi>{m.target}</bdi></td>
                  <td className="px-4 py-2.5 tnum text-muted sm:px-5">{m.priority}</td>
                  <td className="px-4 py-2.5 sm:px-5"><Badge tone={state.tone}>{state.label}</Badge></td>
                  <td className="px-4 py-2.5 tnum text-muted sm:px-5"><bdi>{h?.ping == null ? '—' : `${Math.round(h.ping)} ms`}</bdi></td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
      {insights && <RouteInsights key={view.revision} group={view} />}
    </article>
  )
}

function RoutesPage() {
  const t = useLocalize()
  const [index, setIndex] = useState<Index>()
  const [editor, setEditor] = useState<Group>()
  const [history, setHistory] = useState<Group>()
  const [settings, setSettings] = useState(false)
  const [evaluating, setEvaluating] = useState(false)
  const [error, setError] = useState('')
  const latest = useRef(0)

  const refresh = useCallback(async () => {
    const request = ++latest.current
    try {
      const page = await api.get<Index>(base)
      if (request !== latest.current) return
      setIndex(page)
      setError('')
    } catch (e) {
      if (request === latest.current) setError(errorText(e))
    }
  }, [])
  useEffect(() => {
    void refresh()
    const timer = window.setInterval(() => { if (!document.hidden) void refresh() }, 5000)
    return () => { window.clearInterval(timer); latest.current++ }
  }, [refresh])

  async function evaluate() {
    setEvaluating(true)
    try {
      await api.post(`${base}/evaluate`)
      toast.success(t('Evaluation queued', 'ارزیابی در صف قرار گرفت'))
      void refresh()
    } catch (e) {
      toast.error(errorText(e))
    } finally {
      setEvaluating(false)
    }
  }

  async function remove(group: Group) {
    if (!await confirm({
      title: t('Delete route group?', 'گروه مسیریابی حذف شود؟'),
      body: t('This deletes the group and its history. The existing DNS record stays in Cloudflare.', 'گروه و تاریخچهٔ آن حذف می‌شوند. رکورد DNS در کلادفلر باقی می‌ماند.'),
      confirmLabel: t('Delete group', 'حذف گروه'),
      danger: true,
    })) return
    try {
      await api.del(`${base}/groups/${group.id}?revision=${group.revision}`)
      void refresh()
    } catch (e) {
      toast.error(errorText(e))
    }
  }

  const issues = index?.groups.filter((group) => group.lastError) ?? []
  return (
    <div className="mx-auto max-w-6xl space-y-5 px-4 py-5 sm:px-6 sm:py-7">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="space-y-1">
          <h1 className="flex items-center gap-2.5 text-2xl font-semibold tracking-tight sm:text-3xl">
            <Route className="size-6 text-accent-ink" aria-hidden />{t('Config routes', 'مسیرهای کانفیگ')}
          </h1>
          <p className="text-sm text-muted">{t('Route a public DNS record using the health of existing monitors.', 'مقصد رکورد عمومی را بر اساس سلامت مانیتورهای موجود تعیین کنید.')}</p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button disabled={!index} onClick={() => setSettings(true)} icon={<Cloud className="size-4" />}>Cloudflare</Button>
          <Button disabled={!index?.groups.length} loading={evaluating} onClick={() => void evaluate()} icon={<Activity className="size-4" />}>
            {t('Evaluate now', 'ارزیابی اکنون')}
          </Button>
          <Button disabled={!index} variant="primary" onClick={() => setEditor(groupFrom())} icon={<Plus className="size-4" />}>
            {t('New group', 'گروه جدید')}
          </Button>
        </div>
      </header>
      {index && <div className="flex flex-wrap items-center gap-2">
        <Badge tone={index.dryRun ? 'accent' : 'up'}>{index.dryRun ? t('Dry run', 'اجرای آزمایشی') : t('Live DNS', 'تغییر واقعی DNS')}</Badge>
        <Badge tone={index.tokenConfigured ? 'outline' : 'pending'}>{index.tokenConfigured ? t('Token configured', 'توکن ثبت شده') : t('Cloudflare token required', 'توکن کلادفلر لازم است')}</Badge>
      </div>}
      {issues.length > 0 && <div className="flex flex-wrap items-center gap-2 rounded-xl border border-down/35 bg-down/8 p-3 text-sm text-down">
        <TriangleAlert className="size-4" aria-hidden />
        <strong>{t('Cloudflare issues', 'خطاهای کلادفلر')}</strong>
        {issues.map((group) => <a key={group.id} className="rounded-md bg-down/10 px-2 py-1 text-xs font-medium underline underline-offset-2" href={`#route-${group.id}`}>{group.name}</a>)}
      </div>}
      {error && <p role="alert" className="rounded-xl bg-down/10 p-3 text-sm text-down">{error}</p>}
      {!index && !error && <Spinner />}
      {index?.groups.length === 0 && (
        <div className="grid min-h-52 place-content-center rounded-2xl border border-dashed border-line bg-surface/60 p-6 text-center">
          <p className="font-semibold">{t('No route groups yet', 'هنوز گروه مسیریابی ندارید')}</p>
          <p className="mx-auto mt-1 max-w-sm text-sm text-muted">{t('Create a group and connect at least two monitors to an existing Cloudflare record.', 'یک گروه بسازید و دست‌کم دو مانیتور را به یک رکورد موجود کلادفلر وصل کنید.')}</p>
        </div>
      )}
      <div className="space-y-4">
        {index?.groups.map((view) => <RouteCard key={view.id} view={view} dryRun={index.dryRun}
          edit={() => setEditor(groupFrom(view))} history={() => setHistory(view)} remove={() => void remove(view)} />)}
      </div>
      {editor && <GroupEditor key={editor.id} initial={editor} onClose={() => setEditor(undefined)}
        onSaved={() => { setEditor(undefined); void refresh() }} />}
      {history && <SwitchHistory key={history.id} group={history} onClose={() => setHistory(undefined)} />}
      {settings && index && <CloudSettings initial={index} onClose={() => setSettings(false)}
        onSaved={() => { setSettings(false); void refresh() }} />}
    </div>
  )
}

export default function ConfigRoutes() {
  const role = useApp((s) => s.role)
  return role === 'admin' ? <RoutesPage /> : <Navigate to="/configs" replace />
}
