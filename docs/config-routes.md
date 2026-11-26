# Config Routes

Config Routes uses existing monitor heartbeats to route a Cloudflare A or CNAME record. The module keeps its settings, switch history, and DNS observations under the Neo Uptime data directory in `xdns/`.

## Cloudflare errors

A group reports the failing operation, the latest error, the last time Cloudflare confirmed the DNS record, and the earliest retry time. A successful lookup clears a lookup error. An unconfirmed write remains visible until Cloudflare verifies the update or a later update succeeds. A timeout can occur after Cloudflare has applied a write. The module reads the record again before it considers another write.

In the group editor, enable notifications, select active Neo notification channels, and enable **Cloudflare error alert**. Repeated failures are coalesced, and each configured channel gets its own delivery attempt. Alerts are paused in Dry run. The issue banner links directly to affected groups.

## Routing insights

Open **Insights** in a group to see its last 24 hourly slots or last seven days. Select a bar to see that hour's route durations or latency. The weekly view also shows individual days in the panel time zone.

- DNS shares are percentages of recorded time. The coverage card shows how much of the window has DNS data. Neutral gaps remain unknown. Dry-run decisions are excluded.
- Recent data comes from successful Cloudflare observations. Older data is reconstructed from confirmed changes in history. Manual changes are visible at the next successful lookup. The data describes the authoritative Cloudflare record and does not measure client DNS caches.
- Hourly latency uses the selected monitor's existing Neo aggregates. Empty hours remain empty. Select an hour to see its mean, minimum, and maximum.

Back up the entire Neo Uptime data directory before upgrading an existing installation. Keep the `xdns` subdirectory and its encryption key together.
