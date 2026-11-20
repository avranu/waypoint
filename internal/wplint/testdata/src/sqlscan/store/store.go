package store

import (
	"context"
	"database/sql"
	"fmt"
)

const cols = "a, b"

func scanAB(r *sql.Row) error {
	var a, b int
	return r.Scan(&a, &b)
}

func Check(ctx context.Context, db *sql.DB) error {
	var a, b int
	_ = db.QueryRowContext(ctx, `SELECT a, b, c FROM t`).Scan(&a, &b) // want `the query selects 3 columns but 2 are scanned`
	_ = db.QueryRowContext(ctx, `SELECT a, b FROM t WHERE x = ?`, 1).Scan(&a, &b)
	_ = db.QueryRow("SELECT " + cols + " FROM t").Scan(&a) // want `the query selects 2 columns but 1 is scanned`
	_ = db.QueryRow(`SELECT a, (SELECT max(x), 1 FROM u) FROM t`).Scan(&a, &b)
	_ = db.QueryRow(`SELECT * FROM t`).Scan(&a)
	_ = scanAB(db.QueryRow(`SELECT a FROM t`)) // want `the query selects 1 column but 2 are scanned`
	q := `SELECT a, b, c FROM t`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&a, &b); err != nil { // want `the query selects 3 columns but 2 are scanned`
			return err
		}
	}
	_ = db.QueryRow(`INSERT INTO t(a) VALUES (1) RETURNING id, a`).Scan(&a) // want `the query selects 2 columns but 1 is scanned`
	return rows.Err()
}

// Branches assigns rows a query of a different width on each path, so no one
// width reaches the Scan.
func Branches(ctx context.Context, db *sql.DB, wide bool) error {
	var a, b int
	var rows *sql.Rows
	var err error
	if wide {
		rows, err = db.QueryContext(ctx, `SELECT a, b FROM t`)
	} else {
		rows, err = db.QueryContext(ctx, `SELECT a FROM t`)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if wide {
			err = rows.Scan(&a, &b)
		} else {
			err = rows.Scan(&a)
		}
		if err != nil {
			return err
		}
	}
	return rows.Err()
}

// SameWidth's paths agree, so the width is known.
func SameWidth(ctx context.Context, db *sql.DB, old bool) error {
	var a int
	var rows *sql.Rows
	var err error
	if old {
		rows, err = db.QueryContext(ctx, `SELECT a, b FROM old`)
	} else {
		rows, err = db.QueryContext(ctx, `SELECT a, b FROM t`)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&a); err != nil { // want `the query selects 2 columns but 1 is scanned`
			return err
		}
	}
	return rows.Err()
}

// Tail's format isn't known here, so it may add columns.
func Tail(db *sql.DB, tail string) error {
	var a, b, c int
	return db.QueryRow("SELECT a, b "+fmt.Sprintf(tail, 1)).Scan(&a, &b, &c)
}
