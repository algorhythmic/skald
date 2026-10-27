// skald-secret-scan emits ONLY aggregate pattern counts. Never add row/error
// context to its output: the caller's own transcript may itself be archived.
package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/algorhythmic/skald/internal/secretredact"
	"io"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
)

func scan(ctx context.Context, path string) (map[string]int64, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// mode=ro refuses creation/writes. No BEGIN/read transaction is held across
	// the archive; each SELECT ends before the next table starts. WAL is honored.
	counts := secretredact.Counts()
	for _, table := range []struct {
		query  string
		packed bool
	}{
		{"SELECT original_bytes,decoded_length FROM content_storage", true},
		{"SELECT CASE WHEN decoded_length>=0 THEN compressed_envelope ELSE CAST(envelope_json AS BLOB) END,decoded_length FROM normalization_storage", true},
		{"SELECT CAST(title AS BLOB),-1 FROM session_titles", false},
		{"SELECT CAST(project AS BLOB),-1 FROM session_projects", false},
	} {
		rows, e := db.QueryContext(ctx, table.query)
		if e != nil {
			return nil, errors.New("scan_failed")
		}
		for rows.Next() {
			var b []byte
			var n sql.NullInt64
			if e = rows.Scan(&b, &n); e != nil {
				rows.Close()
				return nil, e
			}
			if table.packed && n.Valid && n.Int64 >= 0 {
				if n.Int64 > 64<<20 {
					rows.Close()
					return nil, errors.New("scan_failed")
				}
				r, e := zlib.NewReader(bytes.NewReader(b))
				if e != nil {
					rows.Close()
					return nil, e
				}
				b, e = io.ReadAll(io.LimitReader(r, n.Int64+1))
				r.Close()
				if e != nil || int64(len(b)) != n.Int64 {
					rows.Close()
					return nil, errors.New("scan_failed")
				}
			}
			secretredact.Inspect(b, counts)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
	}
	return counts, nil
}
func main() {
	archive := flag.String("archive", "", "archive path (default: Skald XDG data directory)")
	flag.Parse()
	if *archive == "" {
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			h, e := os.UserHomeDir()
			if e != nil {
				os.Exit(1)
			}
			base = filepath.Join(h, ".local", "share")
		}
		*archive = filepath.Join(base, "skald", "archive.sqlite")
	}
	abs, e := filepath.Abs(*archive)
	if e != nil {
		os.Exit(1)
	}
	c, e := scan(context.Background(), abs)
	if e != nil {
		fmt.Fprintln(os.Stderr, "scan_failed; no counts emitted")
		os.Exit(1)
	}
	if json.NewEncoder(os.Stdout).Encode(c) != nil {
		os.Exit(1)
	}
}
