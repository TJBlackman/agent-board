package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

func must(err error, what string) {
	if err != nil {
		fmt.Printf("FAIL %s: %v\n", what, err)
		os.Exit(1)
	}
}

func main() {
	dir, _ := os.MkdirTemp("", "abspike")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "board.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(1000)"
	db, err := sql.Open("sqlite", dsn)
	must(err, "open")
	var ver, jm string
	var fk int
	must(db.QueryRow("select sqlite_version()").Scan(&ver), "version")
	must(db.QueryRow("pragma journal_mode").Scan(&jm), "journal_mode")
	must(db.QueryRow("pragma foreign_keys").Scan(&fk), "foreign_keys")
	fmt.Printf("sqlite_version=%s journal_mode=%s foreign_keys=%d\n", ver, jm, fk)
	rows, _ := db.Query("pragma compile_options")
	opts := []string{}
	for rows.Next() {
		var o string
		rows.Scan(&o)
		opts = append(opts, o)
	}
	rows.Close()
	fmt.Printf("compile_options=%v\n", opts)

	_, err = db.Exec(`create table entries(sequence integer primary key autoincrement, public_id text not null unique, body text not null);
	create table posts(entry_sequence integer primary key references entries(sequence), title text not null);
	create virtual table search_docs using fts5(title, description, body, parent_post_sequence unindexed, tokenize='unicode61 remove_diacritics 2');`)
	must(err, "schema")

	// AUTOINCREMENT no reuse after delete
	_, err = db.Exec(`insert into entries(public_id, body) values ('a','x'),('b','y'),('c','z')`)
	must(err, "insert")
	_, err = db.Exec(`delete from entries where sequence=3`)
	must(err, "delete")
	res, err := db.Exec(`insert into entries(public_id, body) values ('d','w')`)
	must(err, "insert2")
	id, _ := res.LastInsertId()
	fmt.Printf("autoincrement after delete of max: next=%d (expect 4)\n", id)

	// unique conflict handling
	_, err = db.Exec(`insert into entries(public_id, body) values ('a','dup')`)
	fmt.Printf("unique conflict error: %v\n", err)

	// FK enforcement
	_, err = db.Exec(`insert into posts(entry_sequence,title) values (999,'orphan')`)
	fmt.Printf("fk violation error: %v\n", err)

	// FTS5 explicit rowid, bm25 weights, phrase, grouping, ranking direction
	tx, _ := db.Begin()
	_, err = tx.Exec(`insert into search_docs(rowid,title,description,body,parent_post_sequence) values
	 (1,'Diagnosing Docker volume permissions','A reproducible startup failure','Observed permission denied on /data volume', 1),
	 (2,'','','Comment: also saw permission denied after restart with docker volume', 1),
	 (3,'Unrelated OAuth2 token refresh','http2 and oauth2 quirks','token refresh fails with error-code 401 in s3-client', 3),
	 (4,'','','Reply: volume permission is fixed by chown', 1),
	 (5,'Volume permission summary','permission denied summary','short', 5)`)
	must(err, "fts insert")
	must(tx.Commit(), "commit")
	q := func(match string) {
		rows, err := db.Query(`with hits as (
		  select rowid as seq, parent_post_sequence as post_seq, bm25(search_docs,3.0,2.0,1.0) as score from search_docs where search_docs match ?
		), ranked as (
		  select seq, post_seq, score, row_number() over (partition by post_seq order by score asc, seq asc) rn from hits
		) select seq, post_seq, score from ranked where rn=1 order by score asc, post_seq desc`, match)
		must(err, "fts query "+match)
		fmt.Printf("query %q:\n", match)
		for rows.Next() {
			var seq, ps int
			var sc float64
			rows.Scan(&seq, &ps, &sc)
			fmt.Printf("  doc=%d post=%d score=%.4f\n", seq, ps, sc)
		}
		rows.Close()
	}
	q(`volume AND "permission denied"`)
	q(`"permission denied"`)
	q(`oauth2`)
	q(`"error-code"`)
	q(`s3`)
	q(`"s3-client"`)
	// transactional visibility + rollback
	tx, _ = db.Begin()
	tx.Exec(`insert into search_docs(rowid,title,description,body,parent_post_sequence) values (9,'rollback me','','zebra-unique-term',9)`)
	tx.Rollback()
	var n int
	db.QueryRow(`select count(*) from search_docs where search_docs match 'zebra'`).Scan(&n)
	fmt.Printf("rolled back doc visible in fts: %d (expect 0)\n", n)

	// snippet function
	var snip string
	must(db.QueryRow(`select snippet(search_docs, 2, '[', ']', '...', 8) from search_docs where search_docs match 'chown'`).Scan(&snip), "snippet")
	fmt.Printf("snippet=%q\n", snip)

	// cancellation: busy loop query with context timeout
	_, err = db.Exec(`create table big(x integer)`)
	must(err, "big")
	tx, _ = db.Begin()
	st, _ := tx.Prepare(`insert into big(x) values(?)`)
	for i := 0; i < 200000; i++ {
		st.Exec(i)
	}
	st.Close()
	tx.Commit()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	start := time.Now()
	var cnt int64
	err = db.QueryRowContext(ctx, `select count(*) from big a, big b where a.x*b.x % 7 = 3`).Scan(&cnt)
	cancel()
	fmt.Printf("cancellation: err=%v elapsed=%s (expect ~50ms, context deadline)\n", err, time.Since(start).Round(time.Millisecond))

	// VACUUM INTO backup
	bk := filepath.Join(dir, "backup.db")
	_, err = db.Exec(`vacuum into ?`, bk)
	must(err, "vacuum into")
	fi, _ := os.Stat(bk)
	db2, err := sql.Open("sqlite", "file:"+bk+"?mode=ro")
	must(err, "open backup")
	must(db2.QueryRow(`select count(*) from search_docs where search_docs match 'volume'`).Scan(&n), "backup fts query")
	fmt.Printf("vacuum into backup: %d bytes, fts rows matching volume=%d\n", fi.Size(), n)
	var ic string
	must(db2.QueryRow(`pragma integrity_check`).Scan(&ic), "integrity")
	fmt.Printf("backup integrity_check=%s\n", ic)
	db2.Close()

	// concurrent readers during writer
	db.SetMaxOpenConns(8)
	errs := make(chan error, 20)
	for i := 0; i < 10; i++ {
		go func(i int) {
			var c int
			errs <- db.QueryRow(`select count(*) from big`).Scan(&c)
		}(i)
	}
	for i := 0; i < 10; i++ {
		go func(i int) {
			_, err := db.Exec(`insert into entries(public_id, body) values (?, 'c')`, fmt.Sprintf("conc-%d", i))
			errs <- err
		}(i)
	}
	fails := 0
	for i := 0; i < 20; i++ {
		if e := <-errs; e != nil {
			fails++
			fmt.Printf("  concurrent op error: %v\n", e)
		}
	}
	fmt.Printf("concurrent 10 readers + 10 writers with busy_timeout: failures=%d\n", fails)
	fmt.Println("SQLITE SPIKE OK")
}
