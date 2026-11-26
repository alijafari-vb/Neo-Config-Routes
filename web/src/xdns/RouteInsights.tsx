import { useEffect, useMemo, useRef, useState } from 'react'
import { ChartNoAxesColumn, Clock, Timer, TriangleAlert } from 'lucide-react'
import { Button, Select, Spinner } from '../components/ui'
import { api } from '../lib/api'
import { fmtDate, fmtTime } from '../lib/format'
import { useLang } from '../lib/i18n'
import { useApp } from '../lib/store'
import { errorText, useLocalize, type Group } from './types'

type Slot = { at: number; end: number; targets: Record<string, number> }
type Ping = { at: number; avg: number | null; min: number | null; max: number | null }
type Insights = { buckets: Slot[]; days: Slot[]; ping: Record<number, Ping[]>; changes: number; failures: number }

const palette = ['#84cc16', '#38bdf8', '#f59e0b', '#a78bfa', '#fb7185', '#34d399', '#818cf8', '#f97316']
const totalOf = (targets: Record<string, number>) => Object.values(targets).reduce((total, value) => total + value, 0)

export default function RouteInsights({ group }: { group: Group }) {
  const t = useLocalize()
  const lang = useLang((s) => s.lang)
  const monitors = useApp((s) => s.monitors)
  const zone = useApp((s) => s.settings.timezone)
  const timezone = zone && zone !== 'auto' ? zone : Intl.DateTimeFormat().resolvedOptions().timeZone
  const timeline = useRef<HTMLDivElement>(null)
  const [hours, setHours] = useState<24 | 168>(24)
  const [member, setMember] = useState(group.members[0]?.monitorId ?? 0)
  const [section, setSection] = useState<'dns' | 'ping'>('dns')
  const [focus, setFocus] = useState<number>()
  const [data, setData] = useState<Insights>()
  const [error, setError] = useState('')
  const [reload, refresh] = useState(0)
  useEffect(() => {
    let disposed = false
    const load = async () => {
      try {
        const result = await api.get<Insights>(`/api/xdns/groups/${group.id}/insights?hours=${hours}&tz=${encodeURIComponent(timezone)}`)
        if (!disposed) { setData(result); setError('') }
      } catch (e) {
        if (!disposed) setError(errorText(e))
      }
    }
    void load()
    const timer = window.setInterval(() => { if (!document.hidden) void load() }, 60000)
    return () => { disposed = true; window.clearInterval(timer) }
  }, [group.id, group.revision, hours, reload, timezone])
  const ready = !!data
  useEffect(() => {
    const area = timeline.current
    if (ready && area) area.scrollLeft = area.scrollWidth
  }, [ready, hours])

  const format = (value: number, fraction = 0) => new Intl.NumberFormat(lang, { maximumFractionDigits: fraction }).format(value)
  const duration = (ms: number) => {
    const mins = Math.round(ms / 60000)
    if (mins < 1 && ms > 0) return t('<1 min', 'کمتر از ۱ دقیقه')
    if (mins < 60) return t(`${format(mins)} min`, `${format(mins)} دقیقه`)
    const h = Math.floor(mins / 60), m = mins % 60
    return t(`${format(h)}h ${format(m)}m`, `${format(h)} ساعت و ${format(m)} دقیقه`)
  }
  const buckets = data?.buckets ?? []
  const series = useMemo(() => {
    const totals: Record<string, number> = {}
    for (const slot of data?.buckets ?? []) {
      for (const [target, value] of Object.entries(slot.targets)) totals[target] = (totals[target] ?? 0) + value
    }
    const order = [...new Set([...group.members.map((m) => m.target), ...Object.keys(totals).sort()])]
    return order.map((target, i) => ({ target, color: palette[i % palette.length], ms: totals[target] ?? 0 }))
  }, [data, group.members])
  const known = series.reduce((sum, item) => sum + item.ms, 0)
  const windowMs = buckets.reduce((sum, slot) => sum + slot.end - slot.at, 0)
  const coverage = windowMs ? known / windowMs : 0
  const ping = new Map((data?.ping[member] ?? []).map((p) => [p.at, p]))
  const peak = Math.max(1, ...Array.from(ping.values(), (p) => p.max ?? p.avg ?? 0))
  const selected = buckets.find((s) => s.at === focus) ?? buckets.at(-1)
  const selectedPing = selected ? ping.get(selected.at) : undefined

  function pickHours(value: 24 | 168) {
    if (value === hours) return
    setData(undefined)
    setFocus(undefined)
    setHours(value)
  }

  const tab = (value: 'dns' | 'ping', label: string) => (
    <Button size="sm" variant={section === value ? 'soft' : 'ghost'} aria-pressed={section === value} onClick={() => setSection(value)}>{label}</Button>
  )

  return (
    <section className="space-y-4 border-t border-line bg-surface-2/40 p-4 sm:p-5" aria-label={t('Routing insights', 'آمار مسیریابی')}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h3 className="flex items-center gap-2 font-semibold"><ChartNoAxesColumn className="size-4 text-accent-ink" aria-hidden />{t('Routing insights', 'آمار مسیریابی')}</h3>
          <p className="mt-1 text-xs text-muted">{t('Hourly DNS residency and monitor latency', 'زمان حضور روی مقصدها و پینگ مانیتورها به تفکیک ساعت')}</p>
        </div>
        <div className="flex gap-1 rounded-xl border border-line bg-surface p-1">
          <Button size="sm" aria-pressed={hours === 24} variant={hours === 24 ? 'soft' : 'ghost'} onClick={() => pickHours(24)}>{t('24 hours', '۲۴ ساعت')}</Button>
          <Button size="sm" aria-pressed={hours === 168} variant={hours === 168 ? 'soft' : 'ghost'} onClick={() => pickHours(168)}>{t('7 days', '۷ روز')}</Button>
        </div>
      </div>
      {error && <div className="flex flex-wrap items-center gap-3 rounded-xl border border-down/30 p-3 text-sm text-down" role="alert">
        {error}<Button size="sm" onClick={() => refresh((n) => n + 1)}>{t('Retry', 'تلاش دوباره')}</Button>
      </div>}
      {!data && !error && <div className="grid place-items-center p-8"><Spinner /></div>}
      {data && <>
        <div className="grid grid-cols-3 gap-2">
          {[
            { icon: Clock, label: t('DNS coverage', 'پوشش دادهٔ DNS'), value: `${format(coverage * 100, 1)}%` },
            { icon: Timer, label: t('DNS changes', 'تغییر مقصد'), value: format(data.changes) },
            { icon: TriangleAlert, label: t('API errors', 'خطای API'), value: format(data.failures) },
          ].map(({ icon: Icon, label, value }) => <div key={label} className="min-w-0 rounded-xl border border-line bg-surface px-3 py-2.5">
            <Icon className="mb-1 size-4 text-muted" aria-hidden /><div className="tnum text-lg font-semibold">{value}</div>
            <div className="text-[11px] text-muted sm:text-xs">{label}</div>
          </div>)}
        </div>
        <div className="rounded-xl border border-line bg-surface p-3 sm:p-4">
          <div className="mb-3 flex flex-wrap items-center gap-2">
            {tab('dns', t('DNS timeline', 'نمودار مقصدها'))}{tab('ping', t('Latency', 'پینگ'))}
            {section === 'ping' && <Select className="ms-auto max-w-52" aria-label={t('Monitor', 'مانیتور')} value={member} onChange={(e) => setMember(Number(e.target.value))}>
              {group.members.map((m) => <option key={m.monitorId} value={m.monitorId}>{monitors[m.monitorId]?.name || m.target}</option>)}
            </Select>}
          </div>
          {section === 'ping' && <div className="mb-2 flex justify-between text-[11px] text-muted"><span>{t('Hourly average', 'میانگین ساعتی')}</span><bdi>{format(peak, 1)} ms</bdi></div>}
          <div dir="ltr" ref={timeline} className="overflow-x-auto rounded-sm">
            <div className={`flex h-28 items-stretch gap-px ${hours === 168 ? 'min-w-[58rem]' : 'min-w-[18rem]'}`}>
            {buckets.map((slot) => {
              const v = ping.get(slot.at)?.avg
              const label = `${fmtDate(slot.at)} · ${fmtTime(slot.at)}`
              const knownSlot = totalOf(slot.targets)
              const detail = section === 'dns'
                ? (knownSlot ? series.filter(({ target }) => slot.targets[target]).map(({ target }) => `${target} · ${duration(slot.targets[target])}`).join(', ') : t('Unknown', 'نامشخص'))
                : (v == null ? t('No ping', 'بدون پینگ') : `${format(v, 1)} ms`)
              return <button type="button" key={slot.at} title={`${label} — ${detail}`} aria-label={`${label} — ${detail}`}
                aria-pressed={selected?.at === slot.at} onClick={() => setFocus(slot.at)}
                className={`relative flex min-w-0 flex-1 flex-col-reverse overflow-hidden rounded-sm bg-surface-3/75 outline-none hover:brightness-125 focus-visible:ring-2 focus-visible:ring-accent ${selected?.at === slot.at ? 'ring-1 ring-fg' : ''}`}>
                {section === 'dns' ? series.filter(({ target }) => slot.targets[target]).map(({ target, color }) =>
                  <span key={target} style={{ height: `${slot.targets[target] / (slot.end - slot.at) * 100}%`, backgroundColor: color }} />)
                  : v != null && <span className="bg-sky-400/85" style={{ height: `${Math.max(1, v / peak * 100)}%` }} />}
              </button>
            })}
            </div>
          </div>
          {buckets.length > 0 && <div dir="ltr" className="mt-2 flex justify-between text-[11px] text-muted">
            <span>{hours === 24 ? fmtTime(buckets[0].at) : fmtDate(buckets[0].at)}</span>
            <span>{hours === 24 ? fmtTime(buckets.at(-1)!.end) : fmtDate(buckets.at(-1)!.end)}</span>
          </div>}
          {selected && <div className="mt-3 space-y-1 rounded-lg bg-surface-2 px-3 py-2 text-xs text-muted">
            <p className="font-medium text-fg"><bdi>{fmtDate(selected.at)} · {fmtTime(selected.at)}–{fmtTime(selected.end)}</bdi></p>
            {section === 'dns' ? <>
              {series.filter(({ target }) => selected.targets[target]).map(({ target, color }) => <div className="flex items-center gap-2" key={target}>
                <span className="size-2 shrink-0 rounded-full" style={{ backgroundColor: color }} />
                <bdi className="min-w-0 grow break-all font-mono">{target}</bdi><span>{duration(selected.targets[target])}</span>
              </div>)}
              {selected.end - selected.at > totalOf(selected.targets) && <p>{t('Unknown', 'نامشخص')} · {duration(selected.end - selected.at - totalOf(selected.targets))}</p>}
            </> : selectedPing?.avg == null ? <p>{t('No ping measured in this hour.', 'در این ساعت پینگی ثبت نشده است.')}</p> :
              <p>{t('Average', 'میانگین')} <bdi>{format(selectedPing.avg, 1)} ms</bdi> · {t('Min / max', 'کمینه / بیشینه')} <bdi>{selectedPing.min == null ? '—' : format(selectedPing.min, 1)} / {selectedPing.max == null ? '—' : format(selectedPing.max, 1)} ms</bdi></p>}
          </div>}
        </div>
        {section === 'dns' && <>
          <div className="grid gap-2 sm:grid-cols-2">
            {series.map(({ target, color, ms }) => <div key={target} className="rounded-xl border border-line bg-surface px-3 py-2.5 text-sm">
              <div className="flex flex-wrap items-center gap-2"><span className="size-2.5 shrink-0 rounded-full" style={{ backgroundColor: color }} />
                <bdi className="min-w-0 break-all font-mono text-xs">{target}</bdi><span className="ms-auto font-semibold tnum">{known ? `${format(ms / known * 100, 1)}%` : '—'}</span></div>
              <div className="mt-1 text-xs text-muted">{duration(ms)}</div>
            </div>)}
          </div>
          {hours === 168 && <div className="space-y-2 rounded-xl border border-line bg-surface p-3 sm:p-4">
            <h4 className="text-sm font-semibold">{t('Daily routing', 'زمان مسیریابی به تفکیک روز')}</h4>
            {data.days.map((day) => <div key={day.at} className="grid grid-cols-[6.5rem_1fr] items-start gap-x-3 gap-y-1 py-1 text-xs">
              <span className="text-muted">{fmtDate(day.at)}</span>
              <div className="space-y-1.5">
                <div dir="ltr" className="flex h-2 overflow-hidden rounded-full bg-surface-3">
                  {series.filter(({ target }) => day.targets[target]).map(({ target, color }) => <span key={target} title={`${target} · ${duration(day.targets[target])}`} style={{ width: `${day.targets[target] / (day.end - day.at) * 100}%`, backgroundColor: color }} />)}
                </div>
                <div className="flex flex-wrap gap-x-3 gap-y-1 text-[11px] text-muted">
                  {series.filter(({ target }) => day.targets[target]).map(({ target, color }) => <span key={target} className="inline-flex items-center gap-1">
                    <span className="size-1.5 rounded-full" style={{ backgroundColor: color }} /><bdi>{target}</bdi> <span>{duration(day.targets[target])}</span>
                  </span>)}
                  {!totalOf(day.targets) && <span>{t('No DNS data', 'بدون دادهٔ DNS')}</span>}
                </div>
              </div>
            </div>)}
          </div>}
          <p className="text-xs leading-relaxed text-muted">{t('Percentages use recorded DNS time. Gaps are unknown. Earlier periods use confirmed switch history. Changes made externally are visible at the next successful check.', 'درصدها از زمان ثبت‌شدهٔ DNS حساب می‌شوند. فاصله‌های بدون داده نامشخص‌اند. داده‌های قدیمی از تاریخچهٔ تغییرهای تأییدشده می‌آیند. تغییرهای دستی در اولین بررسی موفق دیده می‌شوند.')}</p>
        </>}
      </>}
    </section>
  )
}
