package archive

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
)

var ErrBackupObjectLimit = errors.New("backup_object_limit")

type backupLimits struct{ Objects, BatchRows int }

var defaultBackupLimits = backupLimits{Objects: 2000000, BatchRows: 128}

// revisionIndex stores only whole-content digest mappings on a private scratch
// file. No source payload/value is ever written here. Page cache is bounded;
// committing small batches avoids an archive-sized transaction cache.
type revisionIndex struct {
	ctx                     context.Context
	db                      *sql.DB
	tx                      *sql.Tx
	putStmt, getStmt        *sql.Stmt
	path                    string
	limits                  backupLimits
	total, batch, peakBatch int
}

func newRevisionIndex(ctx context.Context, dest string, limits backupLimits) (idx *revisionIndex, err error) {
	if limits.Objects < 1 || limits.BatchRows < 1 || limits.BatchRows > 4096 {
		return nil, errors.New("invalid_backup_limits")
	}
	f, e := os.CreateTemp(filepath.Dir(dest), ".redaction-index-*.sqlite")
	if e != nil {
		return nil, e
	}
	path := f.Name()
	f.Close()
	idx = &revisionIndex{ctx: ctx, path: path, limits: limits}
	created := idx
	defer func() {
		if err != nil {
			created.close()
		}
	}()
	idx.db, err = sql.Open("sqlite", DSN(path, false))
	if err != nil {
		return nil, err
	}
	idx.db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode=OFF", "PRAGMA synchronous=OFF", "PRAGMA cache_size=-1024", "PRAGMA temp_store=FILE", `CREATE TABLE revisions(old_digest TEXT PRIMARY KEY,new_digest TEXT NOT NULL) WITHOUT ROWID`} {
		if _, err = idx.db.ExecContext(ctx, q); err != nil {
			return nil, err
		}
	}
	err = idx.begin()
	return idx, err
}
func (i *revisionIndex) begin() error {
	var e error
	i.tx, e = i.db.BeginTx(i.ctx, nil)
	if e != nil {
		return e
	}
	i.putStmt, e = i.tx.PrepareContext(i.ctx, `INSERT INTO revisions VALUES(?,?)`)
	if e != nil {
		return e
	}
	i.getStmt, e = i.tx.PrepareContext(i.ctx, `SELECT new_digest FROM revisions WHERE old_digest=?`)
	return e
}
func (i *revisionIndex) put(old, next string) error {
	if i.total >= i.limits.Objects {
		return ErrBackupObjectLimit
	}
	if _, e := i.putStmt.ExecContext(i.ctx, old, next); e != nil {
		return e
	}
	i.total++
	i.batch++
	if i.batch > i.peakBatch {
		i.peakBatch = i.batch
	}
	if i.batch == i.limits.BatchRows {
		i.putStmt.Close()
		i.getStmt.Close()
		if e := i.tx.Commit(); e != nil {
			return e
		}
		i.tx = nil
		i.batch = 0
		return i.begin()
	}
	return nil
}
func (i *revisionIndex) get(old string) (string, error) {
	var next string
	e := i.getStmt.QueryRowContext(i.ctx, old).Scan(&next)
	if e == sql.ErrNoRows {
		return old, nil
	}
	return next, e
}
func (i *revisionIndex) close() {
	if i == nil {
		return
	}
	if i.putStmt != nil {
		i.putStmt.Close()
	}
	if i.getStmt != nil {
		i.getStmt.Close()
	}
	if i.tx != nil {
		i.tx.Rollback()
	}
	if i.db != nil {
		i.db.Close()
	}
	if i.path != "" {
		os.Remove(i.path)
		os.Remove(i.path + "-journal")
	}
}
