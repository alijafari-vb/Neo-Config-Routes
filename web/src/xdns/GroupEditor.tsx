import { useState } from 'react'
import { Plus, Trash2 } from 'lucide-react'
import { Button, Checkbox, Field, IconButton, Input, Modal, Select } from '../components/ui'
import { api } from '../lib/api'
import { useApp } from '../lib/store'
import { errorText, eventKinds, eventLabel, recordName, useLocalize, type Group, type Member } from './types'

export default function GroupEditor({ initial, onClose, onSaved }: {
  initial: Group
  onClose: () => void
  onSaved: () => void
}) {
  const t = useLocalize()
  const monitors = useApp((s) => s.monitors)
  const notifications = useApp((s) => s.notifications)
  const [group, setGroup] = useState(initial)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')
  const checks = Object.values(monitors).filter((m) => m.type !== 'group')
  const xray = checks.filter((m) => m.type === 'xray')
  const other = checks.filter((m) => m.type !== 'xray')

  const set = <K extends keyof Group>(key: K, value: Group[K]) => setGroup((g) => ({ ...g, [key]: value }))
  const patchMember = (index: number, patch: Partial<Member>) =>
    setGroup((g) => ({ ...g, members: g.members.map((m, i) => i === index ? { ...m, ...patch } : m) }))

  async function save() {
    setError('')
    setSubmitting(true)
    try {
      if (group.id) await api.put<Group>(`/api/xdns/groups/${group.id}`, group)
      else await api.post<Group>('/api/xdns/groups', group)
      onSaved()
    } catch (e) {
      setError(errorText(e))
    } finally {
      setSubmitting(false)
    }
  }

  const policyFields = [
    { key: 'downSeconds', label: t('Down (seconds)', 'مدت قطعی (ثانیه)') },
    { key: 'slowMs', label: t('Slow threshold (ms)', 'آستانهٔ کندی (میلی‌ثانیه)') },
    { key: 'slowSeconds', label: t('Slow (seconds)', 'مدت کندی (ثانیه)') },
    { key: 'recoverSeconds', label: t('Recovery (seconds)', 'مدت پایداری برگشت (ثانیه)') },
    { key: 'cooldownSeconds', label: t('Cooldown (seconds)', 'فاصلهٔ تغییرها (ثانیه)') },
  ] as const

  return (
    <Modal open onClose={() => { if (!submitting) onClose() }} size="xl"
      title={group.id ? t('Edit route group', 'ویرایش گروه مسیریابی') : t('New route group', 'گروه مسیریابی جدید')}
      footer={<><Button onClick={onClose} disabled={submitting}>{t('Cancel', 'انصراف')}</Button>
        <Button type="submit" form="xdns-group" loading={submitting} variant="primary">{t('Save group', 'ذخیرهٔ گروه')}</Button></>}>
      <form id="xdns-group" className="space-y-6" onSubmit={(event) => { event.preventDefault(); void save() }}>
        <section className="space-y-3">
          <Field label={t('Group name', 'نام گروه')} htmlFor="route-name">
            <Input id="route-name" data-autofocus value={group.name} required maxLength={80} onChange={(e) => set('name', e.target.value)} />
          </Field>
          <Field label="Cloudflare Zone ID" htmlFor="route-zone">
            <Input dir="ltr" id="route-zone" value={group.zone} required pattern="[0-9A-Fa-f]{32}" maxLength={32} onChange={(e) => set('zone', e.target.value)} />
          </Field>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
            <Field label={t('Record name', 'نام رکورد')} htmlFor="route-record" help={t('Use @ for the apex.', 'برای ریشهٔ دامنه @ وارد کنید.')}>
              <Input dir="ltr" id="route-record" required value={group.recordName} placeholder="tun" onChange={(e) => set('recordName', e.target.value)} />
            </Field>
            <Field label={t('Domain', 'دامنه')} htmlFor="route-domain">
              <Input dir="ltr" id="route-domain" required value={group.domain} placeholder="example.net" onChange={(e) => set('domain', e.target.value)} />
            </Field>
            <Field label={t('Record type', 'نوع رکورد')} htmlFor="route-kind">
              <Select id="route-kind" value={group.kind} onChange={(e) => set('kind', e.target.value as Group['kind'])}>
                <option value="CNAME">CNAME</option><option value="A">A</option>
              </Select>
            </Field>
          </div>
          {group.domain && group.recordName && <div className="text-sm text-muted"><bdi className="font-mono">{recordName(group)}</bdi></div>}
          <div className="grid gap-3 sm:grid-cols-2 sm:items-end">
            <Field label={t('TTL · seconds (1 = Auto)', 'TTL برحسب ثانیه (۱ = خودکار)')} htmlFor="route-ttl">
              <Input id="route-ttl" type="number" min={1} max={86400} required disabled={group.proxied}
                value={group.proxied ? 1 : group.ttl} onChange={(e) => set('ttl', e.target.valueAsNumber)} />
            </Field>
            <Checkbox className="min-h-10" checked={group.proxied} onChange={(v) => set('proxied', v)} label={t('Cloudflare proxy', 'پراکسی کلادفلر')} />
          </div>
          <p className="text-xs text-muted">{t('TTL and proxy settings apply with the next target change.', 'TTL و پراکسی همراه با تغییر بعدی مقصد اعمال می‌شوند.')}</p>
        </section>

        <section className="space-y-3 border-t border-line pt-5">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h3 className="font-semibold">{t('Route members', 'اعضای مسیر')}</h3>
            <Button type="button" size="sm" disabled={group.members.length >= 20} icon={<Plus className="size-4" />}
              onClick={() => set('members', [...group.members, { monitorId: 0, target: '', priority: group.members.length + 1 }])}>
              {t('Add member', 'افزودن عضو')}
            </Button>
          </div>
          <p className="text-xs text-muted">{t('Lower numbers win. For equal priorities, the lowest measured ping wins.', 'عدد کمتر اولویت بالاتری دارد. در اولویت برابر، پینگ کمتر برنده است.')}</p>
          {group.members.map((member, index) => (
            <div key={index} className="grid grid-cols-[1fr_auto] gap-2 rounded-xl bg-surface-2/65 p-3 sm:grid-cols-[minmax(0,1.1fr)_minmax(0,1fr)_minmax(5.5rem,0.4fr)_auto] sm:items-end">
              <Field className="col-span-2 sm:col-span-1" label={t('Monitor', 'مانیتور')} htmlFor={`route-monitor-${index}`}>
                <Select id={`route-monitor-${index}`} required value={member.monitorId || ''} onChange={(e) => patchMember(index, { monitorId: Number(e.target.value) })}>
                  <option value="">{t('Select monitor', 'انتخاب مانیتور')}</option>
                  {!!member.monitorId && !monitors[member.monitorId] &&
                    <option value={member.monitorId}>{t('Missing monitor', 'مانیتور حذف‌شده')} #{member.monitorId}</option>}
                  {xray.length > 0 && <optgroup label={t('Xray configs', 'کانفیگ‌های Xray')}>{xray.map((m) => <option key={m.id} value={m.id}>{m.name}</option>)}</optgroup>}
                  {other.length > 0 && <optgroup label={t('Other monitors', 'سایر مانیتورها')}>{other.map((m) => <option key={m.id} value={m.id}>{m.name}</option>)}</optgroup>}
                </Select>
              </Field>
              <Field className="col-span-2 sm:col-span-1" label={t('Target', 'مقصد')} htmlFor={`route-target-${index}`}>
                <Input dir="ltr" id={`route-target-${index}`} required value={member.target}
                  placeholder={group.kind === 'A' ? '192.0.2.10' : 'sia.example.net'} onChange={(e) => patchMember(index, { target: e.target.value })} />
              </Field>
              <Field label={t('Priority', 'اولویت')} htmlFor={`route-priority-${index}`}>
                <Input id={`route-priority-${index}`} type="number" min={1} max={10000} required value={member.priority}
                  onChange={(e) => patchMember(index, { priority: e.target.valueAsNumber })} />
              </Field>
              <IconButton type="button" className="self-end" label={t('Remove member', 'حذف عضو')} disabled={group.members.length <= 2}
                onClick={() => set('members', group.members.filter((_, i) => i !== index))}><Trash2 className="size-4" /></IconButton>
            </div>
          ))}
        </section>

        <section className="space-y-3 border-t border-line pt-5">
          <h3 className="font-semibold">{t('Switch policy', 'سیاست تغییر مسیر')}</h3>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
            {policyFields.map(({ key, label }) => <Field key={key} label={label} htmlFor={`route-${key}`}
              help={key === 'slowMs' ? t('0 disables slow detection.', '۰ یعنی سنجش کندی غیرفعال است.') : undefined}>
              <Input required type="number" min={0} max={key === 'slowMs' ? 600000 : 86400}
                id={`route-${key}`} value={group[key]} onChange={(e) => set(key, e.target.valueAsNumber)} />
            </Field>)}
          </div>
          <div className="flex flex-wrap gap-x-5 gap-y-3">
            <Checkbox checked={group.keepLast} onChange={(v) => set('keepLast', v)}
              label={t('Keep current target when all are down', 'حفظ مقصد فعلی هنگام قطعی تمام اعضا')} />
            <Checkbox checked={group.enabled} onChange={(v) => set('enabled', v)} label={t('Enable routing', 'فعال‌سازی مسیریابی')} />
          </div>
          {!group.keepLast && <p className="text-xs text-muted">
            {t('If every member is confirmed down, DNS returns to the first priority.', 'اگر قطعی تمام اعضا تأیید شود، DNS به اولویت اول برمی‌گردد.')}
          </p>}
        </section>

        <section className="space-y-3 border-t border-line pt-5">
          <Checkbox checked={group.notify} onChange={(v) => set('notify', v)} label={t('Send group notifications', 'ارسال اعلان برای این گروه')} />
          {group.notify && (
            <div className="grid gap-4 sm:grid-cols-2">
              <fieldset className="space-y-2">
                <legend className="mb-2 text-xs font-semibold text-muted">{t('Neo notification channels', 'کانال‌های اعلان Neo')}</legend>
                {notifications.length === 0 && <p className="text-sm text-muted">{t('Add a channel in Neo notification settings first.', 'ابتدا در تنظیمات اعلان Neo کانال بسازید.')}</p>}
                {group.channels.filter((id) => !notifications.some((n) => n.id === id)).map((id) =>
                  <Checkbox key={id} className="flex" checked label={t('Missing channel', 'کانال حذف‌شده') + ' #' + id}
                    onChange={() => set('channels', group.channels.filter((item) => item !== id))} />)}
                {notifications.map((n) => <Checkbox key={n.id} className="flex" checked={group.channels.includes(n.id)}
                  label={n.name + (n.active ? '' : t(' (disabled)', ' (غیرفعال)'))}
                  onChange={(v) => set('channels', v ? [...group.channels, n.id] : group.channels.filter((id) => id !== n.id))} />)}
              </fieldset>
              <fieldset className="space-y-2">
                <legend className="mb-2 text-xs font-semibold text-muted">{t('Events', 'رویدادها')}</legend>
                {eventKinds.filter((kind) => kind !== 'error').map((kind) => <Checkbox className="flex" key={kind} checked={group.events.includes(kind)} label={eventLabel(t, kind)}
                  onChange={(v) => set('events', v ? [...group.events, kind] : group.events.filter((k) => k !== kind))} />)}
                <div className="mt-3 rounded-lg border border-down/25 bg-down/5 p-2.5">
                  <Checkbox checked={group.events.includes('error')} label={t('Cloudflare error alert', 'اعلان خطای کلادفلر')}
                    onChange={(enabled) => set('events', enabled ? [...group.events, 'error'] : group.events.filter((kind) => kind !== 'error'))} />
                  <p className="mt-1 text-xs text-muted">{t('Send to the selected active channels after a lookup or update error in live mode.', 'در حالت واقعی، خطای خواندن یا تغییر رکورد به کانال‌های فعال انتخاب‌شده ارسال می‌شود.')}</p>
                </div>
              </fieldset>
            </div>
          )}
        </section>
        {error && <p role="alert" className="rounded-lg bg-down/10 p-3 text-sm text-down">{error}</p>}
      </form>
    </Modal>
  )
}
