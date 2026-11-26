package xdns

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"time"
)

type CloudHealth struct {
	Operation  string `json:"operation"`
	Message    string `json:"message"`
	FailedAt   int64  `json:"failedAt"`
	RetryAt    int64  `json:"retryAt"`
	VerifiedAt int64  `json:"verifiedAt"`
}

func (s *repository) cloudHealth(ctx context.Context, g Group) (CloudHealth, error) {
	var h CloudHealth
	err := s.db.QueryRowContext(ctx, `SELECT operation,message,failed_at,retry_at,verified_at
		FROM xdns_cloud_health WHERE group_id=? AND record_key=?`, g.ID, recordKey(g)).
		Scan(&h.Operation, &h.Message, &h.FailedAt, &h.RetryAt, &h.VerifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return h, err
}

func (s *repository) setCloudHealth(ctx context.Context, g Group, h CloudHealth) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO xdns_cloud_health VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(group_id,record_key) DO UPDATE SET operation=excluded.operation,message=excluded.message,
		failed_at=excluded.failed_at,retry_at=excluded.retry_at,verified_at=excluded.verified_at`,
		g.ID, recordKey(g), h.Operation, h.Message, h.FailedAt, h.RetryAt, h.VerifiedAt)
	return err
}

type routePoint struct {
	at       int64
	target   string
	observed bool
}

const observationTTL = 2 * time.Minute

func (s *repository) observeRoute(ctx context.Context, g Group, when time.Time, target string) error {
	if g.ID == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO xdns_route_samples VALUES(?,?,?,?)
		ON CONFLICT(group_id,record_key,at) DO UPDATE SET target=excluded.target`, g.ID, recordKey(g), when.UnixMilli(), target); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM xdns_route_samples WHERE group_id=? AND record_key=? AND at<?`,
		g.ID, recordKey(g), when.Add(-8*24*time.Hour).UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *repository) routePoints(ctx context.Context, g Group, start, end int64) ([]routePoint, error) {
	var first sql.NullInt64
	key := recordKey(g)
	if err := s.db.QueryRowContext(ctx, `SELECT min(at) FROM xdns_route_samples WHERE group_id=? AND record_key=?`, g.ID, key).Scan(&first); err != nil {
		return nil, err
	}
	points := []routePoint{}
	if !first.Valid || first.Int64 > start {
		stop := end
		if first.Valid {
			stop = min(stop, first.Int64-1)
		}
		// Existing records can be reconstructed from confirmed switches until
		// the first sampled observation. An error cuts that inference short.
		rows, err := s.db.QueryContext(ctx, `SELECT at,CASE WHEN status='changed' THEN coalesce(json_extract(body,'$.after'),'') ELSE '' END
			FROM xdns_history WHERE group_id=? AND record_key=? AND status IN ('changed','failed','uncertain')
				AND at BETWEEN coalesce((SELECT max(at) FROM xdns_history WHERE group_id=? AND record_key=?
					AND status IN ('changed','failed','uncertain') AND at<=?),?) AND ?
			ORDER BY at,id`, g.ID, key, g.ID, key, start, start, stop)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p routePoint
			if err := rows.Scan(&p.at, &p.target); err != nil {
				rows.Close()
				return nil, err
			}
			points = append(points, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT at,target FROM xdns_route_samples WHERE group_id=? AND record_key=? AND at BETWEEN ? AND ? ORDER BY at`,
		g.ID, key, start-observationTTL.Milliseconds(), end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p := routePoint{observed: true}
		if err := rows.Scan(&p.at, &p.target); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

type RouteBucket struct {
	At      int64            `json:"at"`
	End     int64            `json:"end"`
	Targets map[string]int64 `json:"targets"` // milliseconds by target
}

type PingBucket struct {
	At  int64    `json:"at"`
	Avg *float64 `json:"avg"`
	Min *float64 `json:"min"`
	Max *float64 `json:"max"`
}

func routeBuckets(points []routePoint, start, end int64) []RouteBucket {
	hour := time.Hour.Milliseconds()
	buckets := make([]RouteBucket, 0, (end-start+hour-1)/hour)
	for at := start; at < end; at += hour {
		buckets = append(buckets, RouteBucket{At: at, End: min(at+hour, end), Targets: map[string]int64{}})
	}
	fillRouteBuckets(points, buckets)
	return buckets
}

func dayBuckets(points []routePoint, start, end time.Time, loc *time.Location) []RouteBucket {
	local := start.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	buckets := []RouteBucket{}
	for day.Before(end) {
		next := day.AddDate(0, 0, 1)
		buckets = append(buckets, RouteBucket{At: max(day.UnixMilli(), start.UnixMilli()), End: min(next.UnixMilli(), end.UnixMilli()), Targets: map[string]int64{}})
		day = next
	}
	fillRouteBuckets(points, buckets)
	return buckets
}

func fillRouteBuckets(points []routePoint, buckets []RouteBucket) {
	if len(buckets) == 0 {
		return
	}
	start, end := buckets[0].At, buckets[len(buckets)-1].End
	slot := 0
	for i, p := range points {
		if p.target == "" {
			continue
		}
		a, b := max(p.at, start), end
		if i+1 < len(points) {
			b = min(b, points[i+1].at)
		}
		if p.observed {
			b = min(b, p.at+observationTTL.Milliseconds())
		}
		for a < b {
			for buckets[slot].End <= a {
				slot++
			}
			until := min(b, buckets[slot].End)
			buckets[slot].Targets[p.target] += until - a
			a = until
		}
	}
}

func (s *Service) insights(w http.ResponseWriter, r *http.Request) {
	id := parseID(r.PathValue("id"))
	hours := parseID(r.URL.Query().Get("hours"))
	if id <= 0 || (hours != 24 && hours != 168) {
		problem(w, 400, "use a valid group and a 24 or 168 hour window")
		return
	}
	ctx := r.Context()
	g, err := s.repo.group(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, 404, "group not found")
		return
	}
	if err != nil {
		internal(w, err)
		return
	}
	loc := time.UTC
	if zone := r.URL.Query().Get("tz"); zone != "" && len(zone) <= 80 {
		if zoneLoc, err := time.LoadLocation(zone); err == nil {
			loc = zoneLoc
		}
	}
	now := time.Now()
	start := now.Truncate(time.Hour).Add(time.Duration(1-hours) * time.Hour)
	points, err := s.repo.routePoints(ctx, g, start.UnixMilli(), now.UnixMilli())
	if err != nil {
		internal(w, err)
		return
	}
	var changes, failures int
	err = s.repo.db.QueryRowContext(ctx, `SELECT count(CASE WHEN status='changed' THEN 1 END),
		count(CASE WHEN status IN ('failed','uncertain') THEN 1 END) FROM xdns_history
		WHERE group_id=? AND record_key=? AND at>=? AND at<=?`, g.ID, recordKey(g), start.UnixMilli(), now.UnixMilli()).Scan(&changes, &failures)
	if err != nil {
		internal(w, err)
		return
	}
	ping := map[int64][]PingBucket{}
	if s.src.Ping != nil {
		ids := make([]int64, 0, len(g.Members))
		for _, m := range g.Members {
			ids = append(ids, m.MonitorID)
		}
		ping, err = s.src.Ping(ctx, ids, start)
		if err != nil {
			internal(w, err)
			return
		}
	}
	reply(w, 200, map[string]any{"buckets": routeBuckets(points, start.UnixMilli(), now.UnixMilli()), "ping": ping,
		"changes": changes, "failures": failures, "days": dayBuckets(points, start, now, loc)})
}

func errorAlertsEnabled(g Group) bool {
	return g.Notify && len(g.Channels) > 0 && slices.Contains(g.Events, Failure)
}
