package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Cluster
// ---------------------------------------------------------------------------

// UpsertCluster inserts or refreshes a cluster. A cluster that reappears after
// having been soft-deleted is revived (deleted_at cleared).
func (s *SQLiteStore) UpsertCluster(ctx context.Context, c Cluster) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO clusters(id, name, metrics_enabled, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name       = excluded.name,
			deleted_at = NULL`,
		c.ID, c.Name, boolToInt(c.MetricsEnabled), c.CreatedAt,
	)
	return wrapErr("UpsertCluster", err)
}

// ListClusters returns every cluster still backed by a kubeconfig. Clusters
// whose kubeconfig has been removed are soft-deleted and excluded (BUG-20).
func (s *SQLiteStore) ListClusters(ctx context.Context) ([]Cluster, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, metrics_enabled, created_at FROM clusters
		WHERE deleted_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, wrapErr("ListClusters", err)
	}
	defer rows.Close()

	var out []Cluster
	for rows.Next() {
		var c Cluster
		var metricsInt int
		if err := rows.Scan(&c.ID, &c.Name, &metricsInt, &c.CreatedAt); err != nil {
			return nil, wrapErr("ListClusters scan", err)
		}
		c.MetricsEnabled = metricsInt != 0
		out = append(out, c)
	}
	return out, wrapErr("ListClusters rows", rows.Err())
}

// MarkClustersDeletedExcept soft-deletes every cluster whose ID is not in
// keepIDs. Called after the kubeconfig directory is loaded so that removing a
// kubeconfig removes the cluster from the UI. An empty keepIDs is treated as a
// no-op: loading zero clusters means something went wrong with the config
// directory, not that every cluster was decommissioned.
func (s *SQLiteStore) MarkClustersDeletedExcept(ctx context.Context, keepIDs []string) error {
	if len(keepIDs) == 0 {
		return nil
	}
	args := make([]any, 0, len(keepIDs)+1)
	args = append(args, time.Now())
	placeholders := make([]string, len(keepIDs))
	for i, id := range keepIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE clusters SET deleted_at = ?
		WHERE deleted_at IS NULL AND id NOT IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	return wrapErr("MarkClustersDeletedExcept", err)
}

func (s *SQLiteStore) SetClusterMetricsEnabled(ctx context.Context, clusterID string, enabled bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE clusters SET metrics_enabled = ? WHERE id = ?`,
		boolToInt(enabled), clusterID,
	)
	return wrapErr("SetClusterMetricsEnabled", err)
}

// ---------------------------------------------------------------------------
// CronJob
// ---------------------------------------------------------------------------

// cronJobCols is the canonical SELECT column list for cronjobs, shared by every
// query that scans a full CronJob row.
const cronJobCols = `id, cluster_id, namespace, name, schedule, time_zone, suspended,
	       cpu_request, cpu_limit, memory_request, memory_limit,
	       last_successful_time, updated_at, deleted_at`

// UpsertCronJob inserts or refreshes a CronJob. A CronJob recreated under the
// same name is revived (deleted_at cleared) rather than staying hidden.
func (s *SQLiteStore) UpsertCronJob(ctx context.Context, cj CronJob) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cronjobs(
			id, cluster_id, namespace, name, schedule, time_zone, suspended,
			cpu_request, cpu_limit, memory_request, memory_limit,
			last_successful_time, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			schedule             = excluded.schedule,
			time_zone            = excluded.time_zone,
			suspended            = excluded.suspended,
			cpu_request          = excluded.cpu_request,
			cpu_limit            = excluded.cpu_limit,
			memory_request       = excluded.memory_request,
			memory_limit         = excluded.memory_limit,
			last_successful_time = excluded.last_successful_time,
			updated_at           = excluded.updated_at,
			deleted_at           = NULL`,
		cj.ID, cj.ClusterID, cj.Namespace, cj.Name, cj.Schedule, cj.TimeZone, boolToInt(cj.Suspended),
		cj.CPURequest, cj.CPULimit, cj.MemoryRequest, cj.MemoryLimit,
		cj.LastSuccessfulTime, cj.UpdatedAt,
	)
	return wrapErr("UpsertCronJob", err)
}

// ListCronJobs returns the CronJobs still present in the cluster. Soft-deleted
// ones are excluded so they stop appearing as ghost rows (BUG-20).
func (s *SQLiteStore) ListCronJobs(ctx context.Context, clusterID string) ([]CronJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cronJobCols+` FROM cronjobs
		 WHERE cluster_id = ? AND deleted_at IS NULL ORDER BY namespace, name`,
		clusterID,
	)
	if err != nil {
		return nil, wrapErr("ListCronJobs", err)
	}
	defer rows.Close()

	var out []CronJob
	for rows.Next() {
		cj, err := scanCronJob(rows)
		if err != nil {
			return nil, wrapErr("ListCronJobs scan", err)
		}
		out = append(out, *cj)
	}
	return out, wrapErr("ListCronJobs rows", rows.Err())
}

// GetCronJobByName looks up a single CronJob. Unlike ListCronJobs it does not
// filter out soft-deleted rows: an existing link to the run history of a
// CronJob that has since been deleted keeps working. Callers that render a
// live view should check DeletedAt.
func (s *SQLiteStore) GetCronJobByName(ctx context.Context, clusterID, namespace, name string) (*CronJob, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cronJobCols+` FROM cronjobs
		 WHERE cluster_id = ? AND namespace = ? AND name = ?`,
		clusterID, namespace, name,
	)
	cj, err := scanCronJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return cj, wrapErr("GetCronJobByName", err)
}

// MarkCronJobDeleted soft-deletes a CronJob that is no longer present in the
// cluster. Idempotent: re-marking an already-deleted row keeps its original
// deletion timestamp so the retention purge window does not slide.
func (s *SQLiteStore) MarkCronJobDeleted(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE cronjobs SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		time.Now(), id,
	)
	return wrapErr("MarkCronJobDeleted", err)
}

// ---------------------------------------------------------------------------
// JobRun
// ---------------------------------------------------------------------------

// jobRunCols is the canonical SELECT column list for job_runs, shared by all
// queries that need to scan a full JobRun row.
const jobRunCols = `id, cronjob_id, pod_name, node_name, container_image,
	       trigger, started_at, finished_at, status, exit_code,
	       retry_count, log_size_bytes, duration_ms,
	       avg_cpu_millicores, max_cpu_millicores,
	       avg_memory_bytes, max_memory_bytes`

func (s *SQLiteStore) UpsertJobRun(ctx context.Context, r JobRun) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_runs(id, cronjob_id, pod_name, trigger, started_at, status)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		    cronjob_id = excluded.cronjob_id,
		    pod_name   = excluded.pod_name,
		    trigger    = excluded.trigger,
		    started_at = excluded.started_at,
		    status     = excluded.status`,
		r.ID, r.CronJobID, r.PodName, r.Trigger, r.StartedAt, r.Status,
	)
	return wrapErr("UpsertJobRun", err)
}

func (s *SQLiteStore) GetJobRun(ctx context.Context, id string) (*JobRun, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobRunCols+` FROM job_runs WHERE id = ?`, id)
	r, err := scanJobRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, wrapErr("GetJobRun", err)
}

func (s *SQLiteStore) ListJobRuns(ctx context.Context, cronjobID string) ([]JobRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobRunCols+` FROM job_runs WHERE cronjob_id = ? ORDER BY started_at DESC`,
		cronjobID,
	)
	if err != nil {
		return nil, wrapErr("ListJobRuns", err)
	}
	defer rows.Close()

	var out []JobRun
	for rows.Next() {
		r, err := scanJobRun(rows)
		if err != nil {
			return nil, wrapErr("ListJobRuns scan", err)
		}
		out = append(out, *r)
	}
	return out, wrapErr("ListJobRuns rows", rows.Err())
}

func (s *SQLiteStore) GetLastJobRun(ctx context.Context, cronjobID string) (*JobRun, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobRunCols+` FROM job_runs WHERE cronjob_id = ? ORDER BY started_at DESC LIMIT 1`,
		cronjobID,
	)
	r, err := scanJobRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, wrapErr("GetLastJobRun", err)
}

func (s *SQLiteStore) GetRunStats7d(ctx context.Context, cronjobID string) (*RunStats, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			-- COALESCE: SUM() over zero rows is NULL (COUNT(*) is 0), and these
			-- two land in non-nullable ints. A CronJob that has run before but
			-- not within the window matches no rows here, so without COALESCE
			-- the scan fails and takes the whole page render down with it.
			COALESCE(SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'failed'    THEN 1 ELSE 0 END), 0),
			CAST(AVG(duration_ms) AS INTEGER),
			MAX(duration_ms),
			CAST(AVG(avg_cpu_millicores) AS INTEGER),
			CAST(AVG(avg_memory_bytes)   AS INTEGER)
		FROM job_runs
		WHERE cronjob_id = ?
		  AND started_at > datetime('now', '-7 days')`,
		cronjobID,
	)

	var stats RunStats
	var avgDur, maxDur, avgCPU, avgMem sql.NullInt64
	if err := row.Scan(
		&stats.Total, &stats.Succeeded, &stats.Failed,
		&avgDur, &maxDur, &avgCPU, &avgMem,
	); err != nil {
		return nil, wrapErr("GetRunStats7d", err)
	}
	if avgDur.Valid {
		stats.AvgDurationMs = &avgDur.Int64
	}
	if maxDur.Valid {
		stats.MaxDurationMs = &maxDur.Int64
	}
	if avgCPU.Valid {
		stats.AvgCPUMillicores = &avgCPU.Int64
	}
	if avgMem.Valid {
		stats.AvgMemoryBytes = &avgMem.Int64
	}
	return &stats, nil
}

func (s *SQLiteStore) GetRecentDurations(ctx context.Context, cronjobID string, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT duration_ms FROM job_runs
		WHERE cronjob_id = ? AND duration_ms IS NOT NULL AND status IN ('succeeded','failed')
		ORDER BY started_at DESC LIMIT ?`,
		cronjobID, limit,
	)
	if err != nil {
		return nil, wrapErr("GetRecentDurations", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			return nil, wrapErr("GetRecentDurations scan", err)
		}
		out = append(out, d)
	}
	return out, wrapErr("GetRecentDurations rows", rows.Err())
}

func (s *SQLiteStore) GetDailyRunStats(ctx context.Context, cronjobID string, days int) ([]DailyRunStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT substr(started_at, 1, 10), COUNT(*),
		       SUM(CASE WHEN status='succeeded' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN status='running'   THEN 1 ELSE 0 END)
		FROM job_runs
		WHERE cronjob_id = ? AND started_at > datetime('now', ?)
		GROUP BY substr(started_at, 1, 10)
		ORDER BY substr(started_at, 1, 10)`,
		cronjobID, fmt.Sprintf("-%d days", days),
	)
	if err != nil {
		return nil, wrapErr("GetDailyRunStats", err)
	}
	defer rows.Close()

	var out []DailyRunStat
	for rows.Next() {
		var d DailyRunStat
		if err := rows.Scan(&d.Day, &d.Total, &d.Succeeded, &d.Running); err != nil {
			return nil, wrapErr("GetDailyRunStats scan", err)
		}
		out = append(out, d)
	}
	return out, wrapErr("GetDailyRunStats rows", rows.Err())
}

// ---------------------------------------------------------------------------
// Cluster-wide aggregates (PERF-2)
//
// Cluster and namespace pages need last run + 7-day stats + recent durations
// for every CronJob they display, and the HTMX poll re-renders all of it every
// 10 s per open tab.
//
// The read below is still one indexed query per CronJob per question, because
// that measured faster than every batched alternative: a single window-function
// query (ROW_NUMBER() OVER (PARTITION BY cronjob_id ORDER BY started_at DESC))
// cannot use an index for the partition ordering, so it sorts the cluster's
// whole run history on every render — 600 ms vs 38 ms on 500 CronJobs × 500
// runs. What actually made this cheap is migration 000006's composite index
// (cronjob_id, started_at DESC), which turns each of these reads into an
// ordered index scan instead of an index lookup plus a temp B-tree sort.
//
// Gathering them behind one call still matters: handlers no longer reach into
// the store per row, and a future backend can batch differently without
// touching the render path.
// ---------------------------------------------------------------------------

// GetCronJobSummaries returns the row-summary data for every CronJob in the
// cluster that has at least one run, keyed by CronJob ID. CronJobs with no runs
// are absent from the map; callers must treat a missing entry as "no data yet".
func (s *SQLiteStore) GetCronJobSummaries(ctx context.Context, clusterID string, durationLimit int) (map[string]*CronJobSummary, error) {
	ids, err := s.liveCronJobIDs(ctx, clusterID)
	if err != nil {
		return nil, err
	}

	out := make(map[string]*CronJobSummary, len(ids))
	for _, id := range ids {
		lastRun, err := s.GetLastJobRun(ctx, id)
		if err != nil {
			return nil, err
		}
		if lastRun == nil {
			// No runs recorded yet — leave the CronJob out of the map entirely so
			// callers render "no data" rather than zeroed statistics.
			continue
		}
		stats, err := s.GetRunStats7d(ctx, id)
		if err != nil {
			return nil, err
		}
		durations, err := s.GetRecentDurations(ctx, id, durationLimit)
		if err != nil {
			return nil, err
		}
		out[id] = &CronJobSummary{LastRun: lastRun, Stats7d: stats, Durations: durations}
	}
	return out, nil
}

// liveCronJobIDs returns the IDs of the cluster's non-deleted CronJobs.
func (s *SQLiteStore) liveCronJobIDs(ctx context.Context, clusterID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM cronjobs WHERE cluster_id = ? AND deleted_at IS NULL ORDER BY namespace, name`,
		clusterID,
	)
	if err != nil {
		return nil, wrapErr("liveCronJobIDs", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, wrapErr("liveCronJobIDs scan", err)
		}
		out = append(out, id)
	}
	return out, wrapErr("liveCronJobIDs rows", rows.Err())
}

// CountRunningRuns returns the number of currently-running runs per CronJob ID.
// Cheaper than GetRunningRuns when only the counts are needed: the aggregate
// stays in SQLite instead of materialising every running row.
func (s *SQLiteStore) CountRunningRuns(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT cronjob_id, COUNT(*) FROM job_runs WHERE status = 'running' GROUP BY cronjob_id`)
	if err != nil {
		return nil, wrapErr("CountRunningRuns", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, wrapErr("CountRunningRuns scan", err)
		}
		out[id] = n
	}
	return out, wrapErr("CountRunningRuns rows", rows.Err())
}

// ListJobRunsPaged returns one page of runs, newest first. A zero `before`
// starts at the newest run; otherwise only runs strictly older than it are
// returned.
//
// The comparison is a plain TEXT comparison, deliberately: the driver stores a
// time.Time as Go's own time.Time.String() layout ("2006-01-02 15:04:05.999999999
// -0700 MST"), which is not ISO 8601 — SQLite's datetime() returns NULL for it,
// so the datetime(started_at) < datetime(?) this query used to run was NULL for
// every row and every cursored page came back empty. Passing `before` as a
// time.Time makes the driver serialise it in exactly the layout the column
// holds, and that layout orders lexicographically, which is the same property
// the ORDER BY on this column already depends on.
func (s *SQLiteStore) ListJobRunsPaged(ctx context.Context, cronjobID string, before time.Time, limit int) ([]JobRun, error) {
	var (
		r   *sql.Rows
		err error
	)
	if before.IsZero() {
		r, err = s.db.QueryContext(ctx,
			`SELECT `+jobRunCols+` FROM job_runs WHERE cronjob_id = ? ORDER BY started_at DESC LIMIT ?`,
			cronjobID, limit,
		)
	} else {
		r, err = s.db.QueryContext(ctx,
			`SELECT `+jobRunCols+` FROM job_runs WHERE cronjob_id = ? AND started_at < ? ORDER BY started_at DESC LIMIT ?`,
			cronjobID, before, limit,
		)
	}
	if err != nil {
		return nil, wrapErr("ListJobRunsPaged", err)
	}
	defer r.Close()
	var out []JobRun
	for r.Next() {
		run, err := scanJobRun(r)
		if err != nil {
			return nil, wrapErr("ListJobRunsPaged scan", err)
		}
		out = append(out, *run)
	}
	return out, wrapErr("ListJobRunsPaged rows", r.Err())
}

func (s *SQLiteStore) ListJobRunsByDay(ctx context.Context, cronjobID, day string) ([]JobRun, error) {
	r, err := s.db.QueryContext(ctx,
		`SELECT `+jobRunCols+` FROM job_runs WHERE cronjob_id = ? AND substr(started_at, 1, 10) = ? ORDER BY started_at DESC`,
		cronjobID, day,
	)
	if err != nil {
		return nil, wrapErr("ListJobRunsByDay", err)
	}
	defer r.Close()
	var out []JobRun
	for r.Next() {
		run, err := scanJobRun(r)
		if err != nil {
			return nil, wrapErr("ListJobRunsByDay scan", err)
		}
		out = append(out, *run)
	}
	return out, wrapErr("ListJobRunsByDay rows", r.Err())
}

func (s *SQLiteStore) UpdateJobRunStatus(ctx context.Context, id, status string, finishedAt *time.Time, exitCode, retryCount int) error {
	// Compute duration in Go: the driver's timestamp format is not parseable by
	// SQLite's julianday()/strftime(), so the arithmetic cannot be done in SQL.
	// COALESCE keeps any existing value when finishedAt is nil.
	durationMs := durationFromStart(ctx, s, id, finishedAt)
	_, err := s.db.ExecContext(ctx, `
		UPDATE job_runs
		SET status = ?, finished_at = ?, exit_code = ?, retry_count = ?,
		    duration_ms = COALESCE(?, duration_ms)
		WHERE id = ?`,
		status, finishedAt, exitCode, retryCount, durationMs, id,
	)
	return wrapErr("UpdateJobRunStatus", err)
}

// durationFromStart returns the elapsed milliseconds between the run's stored
// started_at and finishedAt, or nil when finishedAt is nil or started_at can't
// be read. Never negative.
func durationFromStart(ctx context.Context, s *SQLiteStore, id string, finishedAt *time.Time) *int64 {
	if finishedAt == nil {
		return nil
	}
	var startedAt time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT started_at FROM job_runs WHERE id = ?`, id).Scan(&startedAt); err != nil {
		return nil
	}
	ms := finishedAt.Sub(startedAt).Milliseconds()
	if ms < 0 {
		ms = 0
	}
	return &ms
}

func (s *SQLiteStore) UpdateJobRunNode(ctx context.Context, id, nodeName, containerImage string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE job_runs SET node_name = ?, container_image = ? WHERE id = ?`,
		nodeName, containerImage, id,
	)
	return wrapErr("UpdateJobRunNode", err)
}

func (s *SQLiteStore) GetRunningRuns(ctx context.Context) ([]JobRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobRunCols+` FROM job_runs WHERE status = 'running'`,
	)
	if err != nil {
		return nil, wrapErr("GetRunningRuns", err)
	}
	defer rows.Close()

	var out []JobRun
	for rows.Next() {
		r, err := scanJobRun(rows)
		if err != nil {
			return nil, wrapErr("GetRunningRuns scan", err)
		}
		out = append(out, *r)
	}
	return out, wrapErr("GetRunningRuns rows", rows.Err())
}

func (s *SQLiteStore) MarkRunFailed(ctx context.Context, id string) error {
	now := time.Now()
	durationMs := durationFromStart(ctx, s, id, &now)
	_, err := s.db.ExecContext(ctx, `
		UPDATE job_runs
		SET status = 'failed', exit_code = -1, finished_at = ?,
		    duration_ms = COALESCE(?, duration_ms)
		WHERE id = ?`, now, durationMs, id,
	)
	return wrapErr("MarkRunFailed", err)
}

func (s *SQLiteStore) AddLogSize(ctx context.Context, id string, bytes int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE job_runs SET log_size_bytes = log_size_bytes + ? WHERE id = ?`,
		bytes, id,
	)
	return wrapErr("AddLogSize", err)
}

func (s *SQLiteStore) FinalizeResourceUsage(ctx context.Context, runID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE job_runs SET
			avg_cpu_millicores = (SELECT CAST(AVG(cpu_millicores) AS INTEGER) FROM resource_samples WHERE run_id = ?),
			max_cpu_millicores = (SELECT MAX(cpu_millicores)                  FROM resource_samples WHERE run_id = ?),
			avg_memory_bytes   = (SELECT CAST(AVG(memory_bytes) AS INTEGER)   FROM resource_samples WHERE run_id = ?),
			max_memory_bytes   = (SELECT MAX(memory_bytes)                    FROM resource_samples WHERE run_id = ?)
		WHERE id = ?`,
		runID, runID, runID, runID, runID,
	)
	return wrapErr("FinalizeResourceUsage", err)
}

// ---------------------------------------------------------------------------
// Log lines
// ---------------------------------------------------------------------------

func (s *SQLiteStore) BatchInsertLogLines(ctx context.Context, runID string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapErr("BatchInsertLogLines begin", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO log_lines(run_id, line) VALUES (?,?)`)
	if err != nil {
		return wrapErr("BatchInsertLogLines prepare", err)
	}
	defer stmt.Close()

	for _, line := range lines {
		if _, err = stmt.ExecContext(ctx, runID, line); err != nil {
			return wrapErr("BatchInsertLogLines exec", err)
		}
	}

	return wrapErr("BatchInsertLogLines commit", tx.Commit())
}

func (s *SQLiteStore) GetLogLines(ctx context.Context, runID string) ([]LogLine, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, ts, line FROM log_lines WHERE run_id = ? ORDER BY id`,
		runID,
	)
	if err != nil {
		return nil, wrapErr("GetLogLines", err)
	}
	defer rows.Close()

	var out []LogLine
	for rows.Next() {
		var l LogLine
		if err := rows.Scan(&l.ID, &l.RunID, &l.Ts, &l.Line); err != nil {
			return nil, wrapErr("GetLogLines scan", err)
		}
		out = append(out, l)
	}
	return out, wrapErr("GetLogLines rows", rows.Err())
}

func (s *SQLiteStore) GetLogLinesTail(ctx context.Context, runID string, limit int) ([]LogLine, error) {
	if limit <= 0 {
		return s.GetLogLines(ctx, runID)
	}
	// Fetch the last `limit` lines via a DESC subquery, then re-order ASC so the
	// caller always receives lines in chronological order.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, ts, line FROM (
			SELECT id, run_id, ts, line FROM log_lines WHERE run_id = ? ORDER BY id DESC LIMIT ?
		) ORDER BY id ASC`,
		runID, limit,
	)
	if err != nil {
		return nil, wrapErr("GetLogLinesTail", err)
	}
	defer rows.Close()

	var out []LogLine
	for rows.Next() {
		var l LogLine
		if err := rows.Scan(&l.ID, &l.RunID, &l.Ts, &l.Line); err != nil {
			return nil, wrapErr("GetLogLinesTail scan", err)
		}
		out = append(out, l)
	}
	return out, wrapErr("GetLogLinesTail rows", rows.Err())
}

// ---------------------------------------------------------------------------
// Resource samples
// ---------------------------------------------------------------------------

func (s *SQLiteStore) InsertResourceSample(ctx context.Context, runID string, cpuMillicores, memoryBytes int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO resource_samples(run_id, sampled_at, cpu_millicores, memory_bytes)
		VALUES (?, datetime('now'), ?, ?)`,
		runID, cpuMillicores, memoryBytes,
	)
	return wrapErr("InsertResourceSample", err)
}

func (s *SQLiteStore) GetResourceSamples(ctx context.Context, runID string) ([]ResourceSample, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, sampled_at, cpu_millicores, memory_bytes
		FROM resource_samples WHERE run_id = ? ORDER BY id`,
		runID,
	)
	if err != nil {
		return nil, wrapErr("GetResourceSamples", err)
	}
	defer rows.Close()

	var out []ResourceSample
	for rows.Next() {
		var rs ResourceSample
		if err := rows.Scan(&rs.ID, &rs.RunID, &rs.SampledAt, &rs.CPUMillicores, &rs.MemoryBytes); err != nil {
			return nil, wrapErr("GetResourceSamples scan", err)
		}
		out = append(out, rs)
	}
	return out, wrapErr("GetResourceSamples rows", rows.Err())
}

// ---------------------------------------------------------------------------
// Maintenance
// ---------------------------------------------------------------------------

func (s *SQLiteStore) DeleteOldData(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM job_runs WHERE started_at < ?`, before)
	return wrapErr("DeleteOldData", err)
}

func (s *SQLiteStore) DeleteOldLogLines(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM log_lines WHERE run_id IN (SELECT id FROM job_runs WHERE started_at < ?)`,
		before,
	)
	return wrapErr("DeleteOldLogLines", err)
}

// PurgeDeletedCronJobs hard-deletes soft-deleted CronJobs that were removed
// before `before` and have no run rows left. The NOT EXISTS guard is what keeps
// history safe: cronjobs → job_runs cascades, so a CronJob is only dropped once
// DeleteOldData has already aged out all of its runs.
func (s *SQLiteStore) PurgeDeletedCronJobs(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM cronjobs
		WHERE deleted_at IS NOT NULL AND deleted_at < ?
		  AND NOT EXISTS (SELECT 1 FROM job_runs WHERE cronjob_id = cronjobs.id)`,
		before,
	)
	return wrapErr("PurgeDeletedCronJobs", err)
}

func (s *SQLiteStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Close closes the database handle. SQLite checkpoints the WAL into the main
// database file on the last connection close, so a clean shutdown leaves no
// -wal/-shm files behind for the next start to recover.
func (s *SQLiteStore) Close() error {
	return wrapErr("Close", s.db.Close())
}

// ---------------------------------------------------------------------------
// Internal scan helpers
// ---------------------------------------------------------------------------

// rowScanner abstracts *sql.Row and *sql.Rows so we can share scan logic.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCronJob(s rowScanner) (*CronJob, error) {
	var cj CronJob
	var suspended int
	var lastSuccess, deletedAt sql.NullTime
	err := s.Scan(
		&cj.ID, &cj.ClusterID, &cj.Namespace, &cj.Name, &cj.Schedule, &cj.TimeZone, &suspended,
		&cj.CPURequest, &cj.CPULimit, &cj.MemoryRequest, &cj.MemoryLimit,
		&lastSuccess, &cj.UpdatedAt, &deletedAt,
	)
	if err != nil {
		return nil, err
	}
	cj.Suspended = suspended != 0
	if lastSuccess.Valid {
		cj.LastSuccessfulTime = &lastSuccess.Time
	}
	if deletedAt.Valid {
		cj.DeletedAt = &deletedAt.Time
	}
	return &cj, nil
}

func scanJobRun(r rowScanner) (*JobRun, error) {
	var jr JobRun
	var nodeName, containerImage sql.NullString
	var finishedAt sql.NullTime
	var exitCode sql.NullInt64
	var durationMs sql.NullInt64
	var avgCPU, maxCPU, avgMem, maxMem sql.NullInt64

	err := r.Scan(
		&jr.ID, &jr.CronJobID, &jr.PodName,
		&nodeName, &containerImage,
		&jr.Trigger, &jr.StartedAt, &finishedAt,
		&jr.Status, &exitCode,
		&jr.RetryCount, &jr.LogSizeBytes, &durationMs,
		&avgCPU, &maxCPU, &avgMem, &maxMem,
	)
	if err != nil {
		return nil, err
	}
	if nodeName.Valid {
		jr.NodeName = &nodeName.String
	}
	if containerImage.Valid {
		jr.ContainerImage = &containerImage.String
	}
	if finishedAt.Valid {
		jr.FinishedAt = &finishedAt.Time
	}
	if exitCode.Valid {
		v := int(exitCode.Int64)
		jr.ExitCode = &v
	}
	if durationMs.Valid {
		jr.DurationMs = &durationMs.Int64
	} else if jr.FinishedAt != nil {
		// julianday() can't parse Go's RFC3339Nano format; compute in Go.
		ms := jr.FinishedAt.Sub(jr.StartedAt).Milliseconds()
		if ms < 0 {
			ms = 0
		}
		jr.DurationMs = &ms
	}
	if avgCPU.Valid {
		jr.AvgCPUMillicores = &avgCPU.Int64
	}
	if maxCPU.Valid {
		jr.MaxCPUMillicores = &maxCPU.Int64
	}
	if avgMem.Valid {
		jr.AvgMemoryBytes = &avgMem.Int64
	}
	if maxMem.Valid {
		jr.MaxMemoryBytes = &maxMem.Int64
	}
	return &jr, nil
}

// ---------------------------------------------------------------------------
// Fleet-wide aggregates (overview page)
//
// These read across every cluster at once, which no other query does — the rest
// of the app is scoped to a cluster or a single CronJob. Both join `cronjobs`
// so that runs belonging to a soft-deleted CronJob stay out of the totals: the
// run rows survive deletion on purpose (history is preserved), so filtering on
// job_runs alone would keep counting jobs that no longer exist.
// ---------------------------------------------------------------------------

// GetFleetStats aggregates inventory and run outcomes over the last `days`
// days. An empty clusterID spans every cluster (the overview); a non-empty one
// restricts the totals to that cluster (the cluster view), so both pages read
// the same numbers through the same query.
func (s *SQLiteStore) GetFleetStats(ctx context.Context, clusterID string, days int) (*FleetStats, error) {
	var f FleetStats

	// scope is appended to each WHERE clause; the placeholder is bound twice
	// per statement so an empty clusterID matches every row.
	const scope = ` AND (? = '' OR c.cluster_id = ?)`

	// Inventory is a point-in-time count and ignores the window. Clusters is 1
	// when scoped, since a scoped view describes exactly one cluster.
	err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM clusters WHERE deleted_at IS NULL AND (? = '' OR id = ?)),
			(SELECT COUNT(DISTINCT c.namespace) FROM cronjobs c WHERE c.deleted_at IS NULL`+scope+`),
			(SELECT COUNT(*) FROM cronjobs c WHERE c.deleted_at IS NULL`+scope+`),
			(SELECT COUNT(*) FROM cronjobs c WHERE c.deleted_at IS NULL AND c.suspended = 1`+scope+`)`,
		clusterID, clusterID, clusterID, clusterID, clusterID, clusterID, clusterID, clusterID,
	).Scan(&f.Clusters, &f.Namespaces, &f.CronJobs, &f.Suspended)
	if err != nil {
		return nil, wrapErr("GetFleetStats inventory", err)
	}

	// Run outcomes over the window. Running runs are counted separately and are
	// deliberately not bucketed as succeeded or failed — they have no outcome.
	err = s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN r.status = 'succeeded' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN r.status = 'failed'    THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN r.status = 'running'   THEN 1 ELSE 0 END), 0),
			COUNT(DISTINCT CASE WHEN r.status = 'failed' THEN r.cronjob_id END)
		FROM job_runs r
		JOIN cronjobs c ON c.id = r.cronjob_id
		WHERE c.deleted_at IS NULL AND r.started_at > datetime('now', ?)`+scope,
		fmt.Sprintf("-%d days", days), clusterID, clusterID,
	).Scan(&f.Runs, &f.Succeeded, &f.Failed, &f.Running, &f.FailingCronJob)
	if err != nil {
		return nil, wrapErr("GetFleetStats runs", err)
	}
	return &f, nil
}

// rankValueExpr maps a RankMetric to the SQL aggregate that produces its value.
// Ranking by peak rather than mean for CPU and memory is deliberate: what
// decides a CronJob's resource limits is the worst run, not the typical one.
// Duration ranks by mean, because a single slow outlier says less about where
// wall-clock time actually goes than the run-in, run-out average does.
var rankValueExpr = map[RankMetric]string{
	RankByCPU:      `CAST(MAX(r.max_cpu_millicores) AS INTEGER)`,
	RankByMemory:   `CAST(MAX(r.max_memory_bytes) AS INTEGER)`,
	RankByDuration: `CAST(AVG(r.duration_ms) AS INTEGER)`,
	RankByFailures: `COALESCE(SUM(CASE WHEN r.status = 'failed' THEN 1 ELSE 0 END), 0)`,
}

// GetTopCronJobs returns the `limit` CronJobs that rank highest on `metric`
// over the last `days` days. An empty clusterID ranks across every cluster;
// a non-empty one ranks within it. Entries whose value is NULL (no samples
// recorded) or zero are omitted rather than padding the list with rows that
// carry no signal.
func (s *SQLiteStore) GetTopCronJobs(ctx context.Context, clusterID string, metric RankMetric, days, limit int) ([]CronJobRank, error) {
	valueExpr, ok := rankValueExpr[metric]
	if !ok {
		return nil, wrapErr("GetTopCronJobs", fmt.Errorf("unknown rank metric %q", metric))
	}

	// valueExpr comes from the closed rankValueExpr map above, never from a
	// caller-supplied string, so this interpolation cannot carry injected SQL.
	// clusterID stays a bound parameter.
	query := fmt.Sprintf(`
		SELECT c.id, c.cluster_id, c.namespace, c.name,
		       COUNT(*) AS runs,
		       COALESCE(SUM(CASE WHEN r.status = 'failed' THEN 1 ELSE 0 END), 0) AS failed,
		       %s AS value
		FROM job_runs r
		JOIN cronjobs c ON c.id = r.cronjob_id
		WHERE c.deleted_at IS NULL AND r.started_at > datetime('now', ?)
		  AND (? = '' OR c.cluster_id = ?)
		GROUP BY c.id, c.cluster_id, c.namespace, c.name
		HAVING value IS NOT NULL AND value > 0
		ORDER BY value DESC
		LIMIT ?`, valueExpr)

	rows, err := s.db.QueryContext(ctx, query, fmt.Sprintf("-%d days", days), clusterID, clusterID, limit)
	if err != nil {
		return nil, wrapErr("GetTopCronJobs", err)
	}
	defer rows.Close()

	var out []CronJobRank
	for rows.Next() {
		var r CronJobRank
		if err := rows.Scan(&r.CronJobID, &r.ClusterID, &r.Namespace, &r.Name, &r.Runs, &r.Failed, &r.Value); err != nil {
			return nil, wrapErr("GetTopCronJobs scan", err)
		}
		out = append(out, r)
	}
	return out, wrapErr("GetTopCronJobs rows", rows.Err())
}

// ---------------------------------------------------------------------------
// Small utilities
// ---------------------------------------------------------------------------

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("storage.%s: %w", op, err)
}
