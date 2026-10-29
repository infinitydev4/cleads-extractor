// Package pgstore is the PostgreSQL Store. PostgreSQL is both the system of
// record and the work queue (FOR UPDATE SKIP LOCKED), which keeps the Railway
// footprint to a single managed service while allowing any number of workers.
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cleads/business-extractor/internal/dedup"
	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/store"
	"github.com/cleads/business-extractor/migrations"
)

type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// Open connects, tunes the pool and applies migrations.
func Open(ctx context.Context, dsn string, maxConns int32, log *slog.Logger) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MinConns = min(2, cfg.MaxConns)
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.ConnConfig.RuntimeParams["application_name"] = "cleads-extractor"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool, log: log}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// migrate applies embedded migrations under an advisory lock so concurrent
// replicas starting together do not race.
func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	const lockID = 727274001
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockID) //nolint:errcheck
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		v, err := strconv.Atoi(strings.SplitN(f, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad name", f)
		}
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, v).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sqlText, err := migrations.FS.ReadFile(f)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sqlText)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", f, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		s.log.Info("migration applied", "file", f)
	}
	return nil
}

// ---------------------------------------------------------------- jobs

const jobCols = `id, spec, status, error, api_key_id, tasks_total, tasks_done, tasks_failed, raw_found, uniq,
	complete, crm_useful, enriched, created_at, started_at, finished_at`

func scanJob(row pgx.Row) (domain.Job, error) {
	var (
		j        domain.Job
		spec     []byte
		status   string
		started  *time.Time
		finished *time.Time
	)
	err := row.Scan(&j.ID, &spec, &status, &j.Error, &j.APIKeyID, &j.Counters.TasksTotal, &j.Counters.TasksDone,
		&j.Counters.TasksFailed, &j.Counters.RawFound, &j.Counters.Unique, &j.Counters.Complete,
		&j.Counters.CRMUseful, &j.Counters.Enriched, &j.CreatedAt, &started, &finished)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, store.ErrNotFound
	}
	if err != nil {
		return j, err
	}
	j.Status = domain.JobStatus(status)
	if started != nil {
		j.StartedAt = *started
	}
	if finished != nil {
		j.FinishedAt = *finished
	}
	if err := json.Unmarshal(spec, &j.Spec); err != nil {
		return j, fmt.Errorf("decode job spec: %w", err)
	}
	return j, nil
}

func (s *Store) CreateJob(ctx context.Context, job *domain.Job, tasks []domain.Task) error {
	spec, err := json.Marshal(job.Spec)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jobs (id, spec, status, api_key_id, priority, created_at, tasks_total, outstanding)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
			job.ID, spec, string(job.Status), job.APIKeyID, job.Spec.Priority, job.CreatedAt, len(tasks))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return store.ErrConflict
		}
		if err != nil {
			return err
		}
		job.Counters.TasksTotal = int64(len(tasks))
		return insertTasks(ctx, tx, job.ID, tasks)
	})
}

func insertTasks(ctx context.Context, tx pgx.Tx, jobID string, tasks []domain.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	now := time.Now()
	rows := make([][]any, len(tasks))
	for i, t := range tasks {
		p, err := json.Marshal(t.Payload)
		if err != nil {
			return err
		}
		ra := t.RunAfter
		if ra.IsZero() {
			ra = now
		}
		rows[i] = []any{jobID, string(t.Kind), t.Source, p, t.Priority, "pending", ra}
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"tasks"},
		[]string{"job_id", "kind", "source", "payload", "priority", "status", "run_after"}, pgx.CopyFromRows(rows))
	return err
}

func (s *Store) GetJob(ctx context.Context, id string) (domain.Job, error) {
	return scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = $1`, id))
}

func (s *Store) ListJobs(ctx context.Context, status string, limit int, before time.Time) ([]domain.Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if before.IsZero() {
		before = time.Now().Add(time.Hour)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+jobCols+` FROM jobs
		WHERE ($1 = '' OR status = $1) AND created_at < $2 ORDER BY created_at DESC LIMIT $3`, status, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) CancelJob(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1 FOR UPDATE`, id).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		if domain.JobStatus(status).Terminal() {
			return store.ErrConflict
		}
		tag, err := tx.Exec(ctx, `DELETE FROM tasks WHERE job_id = $1 AND status = 'pending'`, id)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE jobs SET status = 'cancelled', finished_at = now(), outstanding = outstanding - $2 WHERE id = $1`,
			id, tag.RowsAffected())
		return err
	})
}

func (s *Store) MarkJobRunning(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE jobs SET status = 'running', started_at = now() WHERE id = $1 AND status = 'queued'`, id)
	return err
}

func (s *Store) FailJob(ctx context.Context, id, reason string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE jobs SET status = 'failed', error = $2, finished_at = now()
			WHERE id = $1 AND status IN ('queued', 'running')`, id, reason); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM tasks WHERE job_id = $1 AND status = 'pending'`, id)
		return err
	})
}

func (s *Store) FinalizeJobs(ctx context.Context) ([]domain.Job, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE jobs SET
			status = CASE WHEN uniq = 0 AND tasks_failed > 0 THEN 'failed' ELSE 'completed' END,
			error = CASE WHEN uniq = 0 AND tasks_failed > 0 THEN 'no result and ' || tasks_failed || ' task(s) failed' ELSE error END,
			finished_at = now()
		WHERE status IN ('queued', 'running') AND outstanding <= 0`); err != nil {
		return nil, err
	}
	// Every terminal job is reported exactly once, whatever ended it
	// (completion, quota, cancellation, failure), across all replicas.
	rows, err := s.pool.Query(ctx, `UPDATE jobs SET notified = true
		WHERE NOT notified AND status IN ('completed', 'failed', 'cancelled')
		RETURNING `+jobCols)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- queue

func (s *Store) LeaseTasks(ctx context.Context, src string, n int, lease time.Duration) ([]domain.Task, error) {
	// Reclaim tasks whose worker died. Cheap thanks to the partial index.
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET status = 'pending', lease_until = NULL
		WHERE status = 'running' AND lease_until < now() AND source = $1`, src); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		WITH c AS (
			SELECT id FROM tasks
			WHERE source = $1 AND status = 'pending' AND run_after <= now()
			ORDER BY priority DESC, run_after, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE tasks t SET status = 'running', attempts = t.attempts + 1,
			lease_until = now() + $3 * interval '1 millisecond'
		FROM c WHERE t.id = c.id
		RETURNING t.id, t.job_id, t.kind, t.source, t.payload, t.priority, t.attempts, t.run_after, t.last_error`,
		src, n, lease.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Task, 0, n)
	for rows.Next() {
		var (
			t       domain.Task
			kind    string
			payload []byte
		)
		if err := rows.Scan(&t.ID, &t.JobID, &kind, &t.Source, &payload, &t.Priority, &t.Attempts, &t.RunAfter, &t.LastError); err != nil {
			return nil, err
		}
		t.Kind = domain.TaskKind(kind)
		t.Status = domain.TaskRunning
		if err := json.Unmarshal(payload, &t.Payload); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ExtendLease(ctx context.Context, ids []int64, lease time.Duration) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE tasks SET lease_until = now() + $2 * interval '1 millisecond'
		WHERE id = ANY($1) AND status = 'running'`, ids, lease.Milliseconds())
	return err
}

func (s *Store) CompleteTask(ctx context.Context, t *domain.Task, out store.TaskOutcome) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			status     string
			uniq       int64
			maxResults int64
		)
		err := tx.QueryRow(ctx, `SELECT status, uniq, COALESCE((spec->>'max_results')::bigint, 0) FROM jobs WHERE id = $1 FOR UPDATE`,
			t.JobID).Scan(&status, &uniq, &maxResults)
		if err != nil {
			return err
		}
		var (
			doneInc, failInc, outstandingDec int64
			tag                              pgconn.CommandTag
		)
		switch {
		case out.Done:
			tag, err = tx.Exec(ctx, `DELETE FROM tasks WHERE id = $1 AND status = 'running' AND attempts = $2`, t.ID, t.Attempts)
			doneInc, outstandingDec = 1, 1
		case !out.RetryAt.IsZero():
			keep := 0
			if out.KeepAttempt {
				keep = 1
			}
			tag, err = tx.Exec(ctx, `UPDATE tasks SET status = 'pending', lease_until = NULL, run_after = $2,
				last_error = $3, attempts = attempts - $4 WHERE id = $1 AND status = 'running' AND attempts = $5`,
				t.ID, out.RetryAt, out.Err, keep, t.Attempts)
		default:
			tag, err = tx.Exec(ctx, `UPDATE tasks SET status = 'failed', lease_until = NULL, last_error = $2
				WHERE id = $1 AND status = 'running' AND attempts = $3`, t.ID, out.Err, t.Attempts)
			failInc, outstandingDec = 1, 1
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Fencing: the lease expired and another worker re-leased the task
			// (attempts moved on). That worker owns the accounting now.
			return store.ErrConflict
		}
		newTasks := out.NewTasks
		terminal := domain.JobStatus(status).Terminal()
		if terminal {
			newTasks = nil
		}
		reached := !terminal && maxResults > 0 && uniq+out.Delta.Unique >= maxResults
		if reached {
			newTasks = nil
		}
		if err := insertTasks(ctx, tx, t.JobID, newTasks); err != nil {
			return err
		}
		d := out.Delta
		_, err = tx.Exec(ctx, `UPDATE jobs SET
				raw_found = raw_found + $2, uniq = uniq + $3, complete = complete + $4, crm_useful = crm_useful + $5,
				enriched = enriched + $6, tasks_done = tasks_done + $7, tasks_failed = tasks_failed + $8,
				tasks_total = tasks_total + $9, outstanding = outstanding + $9 - $10
			WHERE id = $1`,
			t.JobID, d.RawFound, d.Unique, d.Complete, d.CRMUseful, d.Enriched, doneInc, failInc, len(newTasks), outstandingDec)
		if err != nil {
			return err
		}
		if reached {
			tag, err := tx.Exec(ctx, `DELETE FROM tasks WHERE job_id = $1 AND status = 'pending'`, t.JobID)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE jobs SET status = 'completed', finished_at = now(), outstanding = outstanding - $2 WHERE id = $1`,
				t.JobID, tag.RowsAffected())
			return err
		}
		return nil
	})
}

// ---------------------------------------------------------------- businesses

type existing struct {
	id int64
	b  domain.Business
}

// UpsertBusinesses resolves, merges and links a batch in one transaction.
//
// Concurrency: advisory transaction locks on every blocking key (taken in a
// global order to avoid deadlocks) serialize writers that could create the
// same entity, so two workers can never insert the same business twice.
func (s *Store) UpsertBusinesses(ctx context.Context, jobID string, items []domain.Business, keys [][]string, maxInJob int) ([]store.UpsertItem, error) {
	if len(items) == 0 {
		return nil, nil
	}
	res := make([]store.UpsertItem, len(items))
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// 1. lock keys
		allKeys := make([]string, 0, len(items)*4)
		for _, ks := range keys {
			allKeys = append(allKeys, ks...)
		}
		hashes := make([]int64, 0, len(allKeys))
		for _, k := range allKeys {
			hashes = append(hashes, dedup.StableHash(k))
		}
		slices.Sort(hashes)
		hashes = slices.Compact(hashes)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(k) FROM (SELECT k FROM unnest($1::bigint[]) AS k ORDER BY k OFFSET 0) s`, hashes); err != nil {
			return fmt.Errorf("lock keys: %w", err)
		}

		// 2. resolve existing keys
		known := make(map[string]int64, len(allKeys))
		rows, err := tx.Query(ctx, `SELECT key, business_id FROM business_keys WHERE key = ANY($1)`, allKeys)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k string
			var id int64
			if err := rows.Scan(&k, &id); err != nil {
				rows.Close()
				return err
			}
			known[k] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// 3. group items into entities: existing ids or new local entities
		// (two new items sharing a key in the same batch are one entity).
		entityOf := make([]int, len(items)) // index into ents
		type entity struct {
			id    int64
			b     domain.Business
			keys  []string
			isNew bool
		}
		var ents []*entity
		byID := map[int64]int{}
		localKey := map[string]int{}
		var existingIDs []int64
		for i := range items {
			idx := -1
			for _, k := range keys[i] {
				if id, ok := known[k]; ok {
					if e, ok := byID[id]; ok {
						idx = e
					} else {
						ents = append(ents, &entity{id: id})
						idx = len(ents) - 1
						byID[id] = idx
						existingIDs = append(existingIDs, id)
					}
					break
				}
				if e, ok := localKey[k]; ok {
					idx = e
					break
				}
			}
			if idx < 0 {
				ents = append(ents, &entity{isNew: true})
				idx = len(ents) - 1
			}
			for _, k := range keys[i] {
				if _, ok := localKey[k]; !ok {
					localKey[k] = idx
				}
			}
			entityOf[i] = idx
			ents[idx].keys = append(ents[idx].keys, keys[i]...)
		}

		// 4. load existing records
		if len(existingIDs) > 0 {
			rows, err := tx.Query(ctx, `SELECT id, data FROM businesses WHERE id = ANY($1) FOR UPDATE`, existingIDs)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id int64
				var data []byte
				if err := rows.Scan(&id, &data); err != nil {
					rows.Close()
					return err
				}
				e := ents[byID[id]]
				if err := json.Unmarshal(data, &e.b); err != nil {
					rows.Close()
					return fmt.Errorf("decode business %d: %w", id, err)
				}
				e.b.ID = id
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}

		// 5. merge in Go
		now := time.Now().UTC()
		seenNew := make([]bool, len(ents))
		for i := range items {
			e := ents[entityOf[i]]
			if e.isNew && !seenNew[entityOf[i]] {
				seenNew[entityOf[i]] = true
				e.b = items[i]
				e.b.FirstSeenAt = now
				e.b.ComputeCompleteness()
				continue
			}
			e.b.Merge(&items[i])
		}

		// 6. allocate ids for new entities and COPY them
		var nNew int
		for _, e := range ents {
			if e.isNew {
				nNew++
			}
		}
		if nNew > 0 {
			rows, err := tx.Query(ctx, `SELECT nextval('businesses_id_seq') FROM generate_series(1, $1)`, nNew)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
			if err != nil {
				return err
			}
			copyRows := make([][]any, 0, nNew)
			k := 0
			for _, e := range ents {
				if !e.isNew {
					continue
				}
				e.id, e.b.ID = ids[k], ids[k]
				k++
				e.b.LastSeenAt = now
				if len(e.keys) > 0 {
					e.b.DedupKey = e.keys[0]
				}
				r, err := businessRow(&e.b)
				if err != nil {
					return err
				}
				copyRows = append(copyRows, append([]any{e.id}, r...))
			}
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"businesses"}, append([]string{"id"}, businessCols...), pgx.CopyFromRows(copyRows)); err != nil {
				return fmt.Errorf("copy businesses: %w", err)
			}
		}

		// 7. update merged existing records (pipelined)
		if len(existingIDs) > 0 {
			batch := &pgx.Batch{}
			for _, e := range ents {
				if e.isNew {
					continue
				}
				e.b.LastSeenAt = now
				r, err := businessRow(&e.b)
				if err != nil {
					return err
				}
				batch.Queue(`UPDATE businesses SET dedup_key=$2, name=$3, category=$4, city=$5, postal_code=$6, country=$7,
					phone=$8, website=$9, has_email=$10, useful=$11, completeness=$12, lat=$13, lng=$14, data=$15,
					enriched_at=$16, first_seen_at=$17, last_seen_at=$18 WHERE id=$1`, append([]any{e.id}, r...)...)
			}
			if err := tx.SendBatch(ctx, batch).Close(); err != nil {
				return fmt.Errorf("update businesses: %w", err)
			}
		}

		// 8. keys and job links
		kk := make([]string, 0, len(allKeys))
		kv := make([]int64, 0, len(allKeys))
		for k, idx := range localKey {
			if _, ok := known[k]; ok {
				continue
			}
			kk = append(kk, k)
			kv = append(kv, ents[idx].id)
		}
		if len(kk) > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO business_keys (key, business_id) SELECT * FROM unnest($1::text[], $2::bigint[])
				ON CONFLICT (key) DO NOTHING`, kk, kv); err != nil {
				return err
			}
		}
		entIDs := make([]int64, len(ents))
		for i, e := range ents {
			entIDs[i] = e.id
		}
		if maxInJob > 0 {
			// Quota: the job row lock serializes only the linking step of
			// capped jobs; uncapped jobs never take it.
			var linked int64
			if err := tx.QueryRow(ctx, `SELECT linked FROM jobs WHERE id = $1 FOR UPDATE`, jobID).Scan(&linked); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT business_id FROM job_businesses WHERE job_id = $1 AND business_id = ANY($2)`, jobID, entIDs)
			if err != nil {
				return err
			}
			already, err := pgx.CollectRows(rows, pgx.RowTo[int64])
			if err != nil {
				return err
			}
			in := make(map[int64]struct{}, len(already))
			for _, id := range already {
				in[id] = struct{}{}
			}
			room := int64(maxInJob) - linked
			kept := entIDs[:0:0]
			for _, id := range entIDs {
				if _, ok := in[id]; ok {
					continue
				}
				if room <= 0 {
					break
				}
				kept = append(kept, id)
				room--
			}
			entIDs = kept
		}
		rows, err = tx.Query(ctx, `INSERT INTO job_businesses (job_id, business_id)
			SELECT $1, id FROM unnest($2::bigint[]) WITH ORDINALITY AS u(id, o) ORDER BY o
			ON CONFLICT DO NOTHING RETURNING business_id`, jobID, entIDs)
		if err != nil {
			return err
		}
		fresh, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		if maxInJob > 0 && len(fresh) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE jobs SET linked = linked + $2 WHERE id = $1`, jobID, len(fresh)); err != nil {
				return err
			}
		}
		freshSet := make(map[int64]struct{}, len(fresh))
		for _, id := range fresh {
			freshSet[id] = struct{}{}
		}
		reported := make(map[int64]bool, len(ents))
		for i := range items {
			e := ents[entityOf[i]]
			_, isFresh := freshSet[e.id]
			newInJob := isFresh && !reported[e.id]
			if newInJob {
				reported[e.id] = true
			}
			res[i] = store.UpsertItem{
				BusinessID: e.id, NewInJob: newInJob, Complete: e.b.IsComplete(), CRMUseful: e.b.IsCRMUseful(),
				NeedsEnrich: e.b.Website != "" && e.b.EnrichedAt.IsZero(), Website: e.b.Website,
				NeedsDetails: e.b.DataID != "" && e.b.DetailsAt.IsZero(), DataID: e.b.DataID,
			}
		}
		return nil
	})
	return res, err
}

var businessCols = []string{"dedup_key", "name", "category", "city", "postal_code", "country", "phone", "website",
	"has_email", "useful", "completeness", "lat", "lng", "data", "enriched_at", "first_seen_at", "last_seen_at"}

func businessRow(b *domain.Business) ([]any, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	var enriched *time.Time
	if !b.EnrichedAt.IsZero() {
		enriched = &b.EnrichedAt
	}
	first := b.FirstSeenAt
	if first.IsZero() {
		first = time.Now().UTC()
	}
	last := b.LastSeenAt
	if last.IsZero() {
		last = first
	}
	return []any{b.DedupKey, b.Name, b.Category, b.City, b.PostalCode, b.Country, b.Phone, b.Website,
		len(b.Emails) > 0, b.IsCRMUseful(), int16(b.Completeness), b.Lat, b.Lng, data, enriched, first, last}, nil
}

func (s *Store) ApplyPatch(ctx context.Context, _ string, id int64, patch *domain.Business) (becameComplete, becameUseful bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var data []byte
		err := tx.QueryRow(ctx, `SELECT data FROM businesses WHERE id = $1 FOR UPDATE`, id).Scan(&data)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		var b domain.Business
		if err := json.Unmarshal(data, &b); err != nil {
			return err
		}
		b.ID = id
		wasC, wasU := b.IsComplete(), b.IsCRMUseful()
		b.Merge(patch)
		b.LastSeenAt = time.Now().UTC()
		r, err := businessRow(&b)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE businesses SET dedup_key=$2, name=$3, category=$4, city=$5, postal_code=$6, country=$7,
			phone=$8, website=$9, has_email=$10, useful=$11, completeness=$12, lat=$13, lng=$14, data=$15,
			enriched_at=$16, first_seen_at=$17, last_seen_at=$18 WHERE id=$1`, append([]any{id}, r...)...); err != nil {
			return err
		}
		if patch.SIRET != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO business_keys (key, business_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
				"siret:"+patch.SIRET, id); err != nil {
				return err
			}
		}
		becameComplete, becameUseful = !wasC && b.IsComplete(), !wasU && b.IsCRMUseful()
		return nil
	})
	return becameComplete, becameUseful, err
}

// filterSQL appends WHERE fragments for f, using b.* columns.
func filterSQL(f store.BusinessFilter, args []any) (string, []any) {
	var sb strings.Builder
	add := func(cond string, v any) {
		args = append(args, v)
		sb.WriteString(" AND ")
		sb.WriteString(strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.MinCompleteness > 0 {
		add("b.completeness >= ?", f.MinCompleteness)
	}
	if f.HasPhone {
		sb.WriteString(" AND b.phone <> ''")
	}
	if f.HasEmail {
		sb.WriteString(" AND b.has_email")
	}
	if f.HasWebsite {
		sb.WriteString(" AND b.website <> ''")
	}
	if f.OnlyUseful {
		sb.WriteString(" AND b.useful")
	}
	if f.City != "" {
		add("lower(b.city) = lower(?)", f.City)
	}
	if f.PostalPrefix != "" {
		add("b.postal_code LIKE ?", escapeLike(f.PostalPrefix)+"%")
	}
	if f.Category != "" {
		add("b.category ILIKE ?", "%"+escapeLike(f.Category)+"%")
	}
	if f.Query != "" {
		add("b.name ILIKE ?", "%"+escapeLike(f.Query)+"%")
	}
	return sb.String(), args
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Store) ListJobBusinesses(ctx context.Context, jobID string, afterSeq int64, limit int, f store.BusinessFilter) ([]store.BusinessRow, error) {
	where, args := filterSQL(f, []any{jobID, afterSeq, limit})
	rows, err := s.pool.Query(ctx, `SELECT jb.seq, b.id, b.data FROM job_businesses jb
		JOIN businesses b ON b.id = jb.business_id
		WHERE jb.job_id = $1 AND jb.seq > $2`+where+` ORDER BY jb.seq LIMIT $3`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.BusinessRow, 0, min(limit, 1024))
	for rows.Next() {
		var r store.BusinessRow
		var data []byte
		if err := rows.Scan(&r.Seq, &r.ID, &data); err != nil {
			return nil, err
		}
		id := r.ID
		if err := json.Unmarshal(data, &r.Business); err != nil {
			return nil, err
		}
		r.ID = id
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) StreamJobBusinesses(ctx context.Context, jobID string, f store.BusinessFilter, fn func(*store.BusinessRow) error) error {
	// Keyset pages instead of one long cursor: no long-lived transaction,
	// constant memory, and the export can run while the job is still writing.
	var after int64
	for {
		page, err := s.ListJobBusinesses(ctx, jobID, after, 2000, f)
		if err != nil {
			return err
		}
		for i := range page {
			if err := fn(&page[i]); err != nil {
				return err
			}
		}
		if len(page) < 2000 {
			return nil
		}
		after = page[len(page)-1].Seq
	}
}

func (s *Store) SearchBusinesses(ctx context.Context, afterID int64, limit int, f store.BusinessFilter) ([]domain.Business, error) {
	where, args := filterSQL(f, []any{afterID, limit})
	rows, err := s.pool.Query(ctx, `SELECT b.id, b.data FROM businesses b WHERE b.id > $1`+where+` ORDER BY b.id LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Business
	for rows.Next() {
		var id int64
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		var b domain.Business
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, err
		}
		b.ID = id
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) GetBusiness(ctx context.Context, id int64) (domain.Business, error) {
	var data []byte
	err := s.pool.QueryRow(ctx, `SELECT data FROM businesses WHERE id = $1`, id).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Business{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Business{}, err
	}
	var b domain.Business
	if err := json.Unmarshal(data, &b); err != nil {
		return b, err
	}
	b.ID = id
	return b, nil
}

func (s *Store) Stats(ctx context.Context) (store.Stats, error) {
	st := store.Stats{Jobs: map[string]int64{}, TasksPending: map[string]int64{}}
	// reltuples is an estimate but O(1); exact count(*) would scan millions of rows.
	if err := s.pool.QueryRow(ctx, `SELECT GREATEST(reltuples, 0)::bigint FROM pg_class WHERE relname = 'businesses'`).Scan(&st.Businesses); err != nil {
		return st, err
	}
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM jobs GROUP BY status`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return st, err
		}
		st.Jobs[k] = v
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT source, status, count(*) FROM tasks WHERE status IN ('pending','running') GROUP BY 1, 2`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var src, status string
		var v int64
		if err := rows.Scan(&src, &status, &v); err != nil {
			return st, err
		}
		if status == "pending" {
			st.TasksPending[src] = v
		} else {
			st.TasksRunning += v
		}
	}
	return st, rows.Err()
}

// ---------------------------------------------------------------- diagnostics

func (s *Store) ListTasks(ctx context.Context, jobID string, status domain.TaskStatus, limit int) ([]domain.Task, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id, job_id, kind, source, payload, priority, attempts, status, run_after, last_error
		FROM tasks WHERE job_id = $1 AND ($2 = '' OR status = $2)
		ORDER BY (status = 'failed') DESC, id LIMIT $3`, jobID, string(status), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		var (
			t            domain.Task
			kind, status string
			payload      []byte
		)
		if err := rows.Scan(&t.ID, &t.JobID, &kind, &t.Source, &payload, &t.Priority, &t.Attempts, &status, &t.RunAfter, &t.LastError); err != nil {
			return nil, err
		}
		t.Kind, t.Status = domain.TaskKind(kind), domain.TaskStatus(status)
		if err := json.Unmarshal(payload, &t.Payload); err != nil {
			return nil, fmt.Errorf("decode task payload: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) InsertEvents(ctx context.Context, events []domain.LogEvent) error {
	if len(events) == 0 {
		return nil
	}
	rows := make([][]any, len(events))
	for i := range events {
		e := &events[i]
		attrs := []byte("{}")
		if len(e.Attrs) > 0 {
			b, err := json.Marshal(e.Attrs)
			if err != nil {
				return fmt.Errorf("encode event attrs: %w", err)
			}
			attrs = b
		}
		var task *int64
		if e.TaskID != 0 {
			task = &e.TaskID
		}
		rows[i] = []any{e.Time, e.Level, e.Msg, e.JobID, task, e.Source, e.Worker, attrs}
	}
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"events"},
		[]string{"ts", "level", "msg", "job_id", "task_id", "source", "worker", "attrs"}, pgx.CopyFromRows(rows))
	return err
}

func (s *Store) ListEvents(ctx context.Context, f store.EventFilter) ([]domain.LogEvent, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var (
		sb   strings.Builder
		args []any
	)
	add := func(cond string, v any) {
		args = append(args, v)
		sb.WriteString(" AND ")
		sb.WriteString(strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if len(f.Levels) > 0 {
		add("level = ANY(?)", f.Levels)
	}
	if f.JobID != "" {
		add("job_id = ?", f.JobID)
	}
	if p, ok := strings.CutSuffix(f.Source, "*"); ok {
		add("source LIKE ?", escapeLike(p)+"%")
	} else if f.Source != "" {
		add("source = ?", f.Source)
	}
	if !f.Since.IsZero() {
		add("ts >= ?", f.Since)
	}
	if f.BeforeID > 0 {
		add("id < ?", f.BeforeID)
	}
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, `SELECT id, ts, level, msg, job_id, COALESCE(task_id, 0), source, worker, attrs
		FROM events WHERE true`+sb.String()+` ORDER BY id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.LogEvent
	for rows.Next() {
		var (
			e     domain.LogEvent
			attrs []byte
		)
		if err := rows.Scan(&e.ID, &e.Time, &e.Level, &e.Msg, &e.JobID, &e.TaskID, &e.Source, &e.Worker, &attrs); err != nil {
			return nil, err
		}
		e.Time = e.Time.UTC()
		if len(attrs) > 2 {
			if err := json.Unmarshal(attrs, &e.Attrs); err != nil {
				return nil, fmt.Errorf("decode event attrs: %w", err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) PurgeEvents(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM events WHERE ts < $1`, before)
	if err != nil {
		return 0, err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM workers WHERE seen_at < $1`, before); err != nil {
		return tag.RowsAffected(), err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) PutWorkerStatus(ctx context.Context, w domain.WorkerStatus) error {
	src, err := json.Marshal(w.Sources)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO workers (id, role, version, started_at, seen_at, sources)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET role = EXCLUDED.role, version = EXCLUDED.version,
			started_at = EXCLUDED.started_at, seen_at = EXCLUDED.seen_at, sources = EXCLUDED.sources`,
		w.ID, w.Role, w.Version, w.StartedAt, w.SeenAt, src)
	return err
}

func (s *Store) ListWorkerStatus(ctx context.Context, seenAfter time.Time) ([]domain.WorkerStatus, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, role, version, started_at, seen_at, sources FROM workers
		WHERE seen_at > $1 ORDER BY id`, seenAfter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.WorkerStatus
	for rows.Next() {
		var (
			w   domain.WorkerStatus
			src []byte
		)
		if err := rows.Scan(&w.ID, &w.Role, &w.Version, &w.StartedAt, &w.SeenAt, &src); err != nil {
			return nil, err
		}
		w.StartedAt, w.SeenAt = w.StartedAt.UTC(), w.SeenAt.UTC()
		if err := json.Unmarshal(src, &w.Sources); err != nil {
			return nil, fmt.Errorf("decode worker sources: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
