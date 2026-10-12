package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// Deliveries are files a host sealed to a recipient key the server does not have (X25519, see
// format.NewSealWriter): the server stores and relays opaque bytes. They live in the repository's
// datastore under deliveries/<job>/<attempt>/p-NNNNNN, in parts, because the Backend only writes whole
// objects and a delivery may be up to 4 GiB. That key space is outside chunks/, groups/ and
// quarantine/, which are the only prefixes GC, verify, reconcile and the orphan cleaner list.

// DeliveryPartSize is the size of one stored part; MaxDeliveryBytes bounds one delivery (tests lower both).
var (
	DeliveryPartSize       = 32 << 20
	MaxDeliveryBytes int64 = jobspec.MaxDeliveryBytes
)

const deliveriesRoot = "deliveries/"

func deliveryPrefix(id string, attempt int) string {
	return fmt.Sprintf("%s%s/%d/", deliveriesRoot, id, attempt)
}

func partKey(prefix string, i int) string { return fmt.Sprintf("%sp-%06d", prefix, i) }

func deletePrefix(b datastore.Backend, prefix string) error {
	var keys []string
	if err := b.List(prefix, func(key string, _ datastore.Info) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		return err
	}
	var first error
	for _, k := range keys {
		if err := b.Delete(k); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Service) backendByRepoID(ctx context.Context, repoID string) (datastore.Backend, error) {
	repo, err := s.Svc.RepositoryByID(ctx, repoID)
	if err != nil {
		return nil, err
	}
	st, err := s.Svc.Store(repo)
	if err != nil {
		return nil, err
	}
	return st.Backend(), nil
}

// PutDelivery stores the sealed file of a running delivery job, streaming it from r into parts
// (bounded by jobspec.MaxDeliveryBytes). The delivery becomes visible only after a clean end of
// the stream, and only if the job is still on this attempt. It returns the size and SHA-256 the
// server computed.
func (s *Service) PutDelivery(ctx context.Context, h *service.Host, id string, attempt int, r io.Reader) (int64, string, error) {
	if err := hostOK(h); err != nil {
		return 0, "", err
	}
	tx, err := s.Svc.DB.Pool().Begin(ctx)
	if err != nil {
		return 0, "", err
	}
	typ, _, err := owned(ctx, tx, h, id, attempt)
	if err != nil {
		tx.Rollback(ctx)
		return 0, "", err
	}
	info, _ := jobspec.Lookup(typ)
	if !info.Delivery {
		tx.Rollback(ctx)
		return 0, "", bad("a %s job has no delivery", typ)
	}
	prefix := deliveryPrefix(id, attempt)
	var key string
	err = tx.QueryRow(ctx, `SELECT storage_key FROM job_deliveries WHERE job_id=$1`, id).Scan(&key)
	if err == nil && key == prefix {
		tx.Rollback(ctx)
		return 0, "", fmt.Errorf("%w: this job's delivery was already uploaded", service.ErrConflict)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		tx.Rollback(ctx)
		return 0, "", err
	}
	tx.Rollback(ctx)
	b, err := s.backendByRepoID(ctx, h.RepoID)
	if err != nil {
		return 0, "", err
	}
	if key != "" { // left over from an earlier attempt
		if err := deletePrefix(b, key); err != nil {
			return 0, "", err
		}
		if _, err := s.Svc.DB.Pool().Exec(ctx, `DELETE FROM job_deliveries WHERE job_id=$1 AND storage_key=$2`, id, key); err != nil {
			return 0, "", err
		}
	}
	if err := deletePrefix(b, prefix); err != nil { // a broken earlier upload of this attempt
		return 0, "", err
	}
	size, sum, parts, err := storeParts(ctx, b, prefix, r)
	if err != nil {
		_ = deletePrefix(b, prefix)
		return 0, "", err
	}
	now := s.now()
	ct, err := s.Svc.DB.Pool().Exec(ctx, `INSERT INTO job_deliveries (job_id, repository_id, size, sha256, parts, storage_key, created_at, expires_at)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8 WHERE EXISTS (SELECT 1 FROM host_jobs WHERE id=$1 AND repository_id=$2 AND host_name=$9 AND state='running' AND attempt=$10)
		ON CONFLICT (job_id) DO NOTHING`, id, h.RepoID, size, sum, parts, prefix, now, now.Add(jobspec.DeliveryTTL), h.Name, attempt)
	if err == nil && ct.RowsAffected() == 0 {
		err = fmt.Errorf("%w: the job ended or moved to another attempt during the upload", service.ErrConflict)
	}
	if err != nil {
		_ = deletePrefix(b, prefix)
		return 0, "", err
	}
	s.publishID(ctx, id)
	return size, sum, nil
}

func storeParts(ctx context.Context, b datastore.Backend, prefix string, r io.Reader) (int64, string, int, error) {
	h := sha256.New()
	buf := make([]byte, DeliveryPartSize)
	var size int64
	parts := 0
	for {
		if err := ctx.Err(); err != nil {
			return 0, "", 0, err
		}
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			size += int64(n)
			if size > MaxDeliveryBytes {
				return 0, "", 0, bad("a delivery is limited to %d bytes", MaxDeliveryBytes)
			}
			h.Write(buf[:n])
			if err := b.Put(partKey(prefix, parts), append([]byte(nil), buf[:n]...), false); err != nil {
				return 0, "", 0, err
			}
			parts++
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return 0, "", 0, fmt.Errorf("%w: reading the upload: %v", service.ErrBadRequest, rerr)
		}
	}
	if size == 0 {
		return 0, "", 0, bad("empty delivery")
	}
	return size, hex.EncodeToString(h.Sum(nil)), parts, nil
}

// OpenDelivery opens the sealed file of a job for relaying to whoever holds the recipient key. The
// reader checks the SHA-256 at the end and fails instead of returning a clean EOF on a mismatch.
func (s *Service) OpenDelivery(ctx context.Context, jobID string) (io.ReadCloser, int64, string, error) {
	if err := checkID(jobID); err != nil {
		return nil, 0, "", err
	}
	var repoID, sum, prefix string
	var size int64
	var parts int
	var expired bool
	var state string
	err := s.Svc.DB.Pool().QueryRow(ctx, `SELECT d.repository_id::text, d.size, d.sha256, d.parts, d.storage_key, d.expires_at <= $2, j.state
		FROM job_deliveries d JOIN host_jobs j ON j.id=d.job_id WHERE d.job_id=$1`,
		jobID, s.now()).Scan(&repoID, &size, &sum, &parts, &prefix, &expired, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, "", fmt.Errorf("%w: this job has no delivery", service.ErrNotFound)
	}
	if err != nil {
		return nil, 0, "", err
	}
	if state != jobspec.StateSucceeded { // still running, or the host's checks failed
		return nil, 0, "", fmt.Errorf("%w: the job has not succeeded (%s); its delivery is not available", service.ErrConflict, state)
	}
	if expired {
		return nil, 0, "", fmt.Errorf("%w: the delivery expired", service.ErrNotFound)
	}
	b, err := s.backendByRepoID(ctx, repoID)
	if err != nil {
		return nil, 0, "", err
	}
	return &partReader{b: b, prefix: prefix, parts: parts, want: sum, size: size, h: sha256.New()}, size, sum, nil
}

type partReader struct {
	b      datastore.Backend
	prefix string
	parts  int
	next   int
	cur    []byte
	want   string
	size   int64
	read   int64
	h      hash.Hash
	err    error
}

func (p *partReader) Read(out []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	for len(p.cur) == 0 {
		if p.next == p.parts {
			if p.read != p.size || hex.EncodeToString(p.h.Sum(nil)) != p.want {
				p.err = errors.New("the stored delivery does not match its recorded size/SHA-256")
				return 0, p.err
			}
			p.err = io.EOF
			return 0, io.EOF
		}
		b, err := p.b.Get(partKey(p.prefix, p.next))
		if err != nil {
			p.err = fmt.Errorf("reading delivery part %d: %w", p.next, err)
			return 0, p.err
		}
		p.next++
		p.cur = b
		p.h.Write(b)
	}
	n := copy(out, p.cur)
	p.cur = p.cur[n:]
	p.read += int64(n)
	return n, nil
}

func (p *partReader) Close() error { return nil }

// ReapDeliveries deletes expired deliveries and the parts of uploads that can never complete (their
// job is no longer running on that attempt and no delivery row names them). It returns how many
// deliveries and orphaned prefixes it removed.
func (s *Service) ReapDeliveries(ctx context.Context) (int, error) {
	now := s.now()
	type exp struct{ job, repo, key string }
	rows, err := s.Svc.DB.Pool().Query(ctx, `SELECT job_id::text, repository_id::text, storage_key FROM job_deliveries WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	var expired []exp
	for rows.Next() {
		var e exp
		if err := rows.Scan(&e.job, &e.repo, &e.key); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, e)
	}
	rows.Close()
	n := 0
	var errs []error
	for _, e := range expired {
		b, err := s.backendByRepoID(ctx, e.repo)
		if err == nil {
			err = deletePrefix(b, e.key)
		}
		if err != nil {
			errs = append(errs, err)
			continue // keep the row: the next run tries again
		}
		if _, err := s.Svc.DB.Pool().Exec(ctx, `DELETE FROM job_deliveries WHERE job_id=$1 AND storage_key=$2`, e.job, e.key); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	repos, err := s.Svc.Repositories(ctx)
	if err != nil {
		return n, errors.Join(append(errs, err)...)
	}
	for _, r := range repos {
		m, err := s.reapOrphans(ctx, &r)
		n += m
		if err != nil {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

func (s *Service) reapOrphans(ctx context.Context, r *service.Repo) (int, error) {
	st, err := s.Svc.Store(r)
	if err != nil {
		return 0, err
	}
	b := st.Backend()
	prefixes := map[string]bool{}
	if err := b.List(deliveriesRoot, func(key string, _ datastore.Info) error {
		p := strings.Split(strings.TrimPrefix(key, deliveriesRoot), "/")
		if len(p) == 3 && uuidRe.MatchString(p[0]) {
			if _, err := strconv.Atoi(p[1]); err == nil {
				prefixes[deliveriesRoot+p[0]+"/"+p[1]+"/"] = true
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	if len(prefixes) == 0 {
		return 0, nil
	}
	// ONE statement, so it sees one snapshot: a prefix is live if a delivery row names it or its job
	// is running on that attempt (an upload in flight). Listed BEFORE this query, so an upload that
	// began after the listing is not in `prefixes` at all.
	live := map[string]bool{}
	rows, err := s.Svc.DB.Pool().Query(ctx, `SELECT storage_key FROM job_deliveries WHERE repository_id=$1
		UNION SELECT 'deliveries/' || id::text || '/' || attempt::text || '/' FROM host_jobs WHERE repository_id=$1 AND state='running'`, r.ID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return 0, err
		}
		live[k] = true
	}
	rows.Close()
	n := 0
	for p := range prefixes {
		if live[p] {
			continue
		}
		if err := deletePrefix(b, p); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
