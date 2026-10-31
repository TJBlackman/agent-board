package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/tursodatabase/turso-go"
)

func main() {
	dir, _ := os.MkdirTemp("", "tursospike")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "board.db")
	db, err := sql.Open("turso", path)
	if err != nil {
		fmt.Println("FAIL open:", err)
		os.Exit(1)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow("select sqlite_version()").Scan(&v); err != nil {
		fmt.Println("FAIL version:", err)
		os.Exit(1)
	}
	fmt.Println("turso version:", v)
	_, err = db.Exec(`create table entries(sequence integer primary key autoincrement, public_id text not null unique, body text not null)`)
	fmt.Println("create autoincrement table:", err)
	_, err = db.Exec(`create virtual table search_docs using fts5(title, body)`)
	fmt.Println("fts5 virtual table:", err)
	_, err = db.Exec(`insert into entries(public_id, body) values ('a','x')`)
	fmt.Println("insert:", err)
	_, err = db.Exec(`insert into entries(public_id, body) values ('a','dup')`)
	fmt.Println("unique conflict:", err)
	_, err = db.Exec(`select fts_create_index('entries_fts','entries','body')`)
	fmt.Println("turso fts_create_index:", err)
	var n int
	err = db.QueryRow(`select count(*) from entries where fts_match('entries_fts', body, 'x')`).Scan(&n)
	fmt.Println("turso fts_match:", n, err)
	fmt.Println("TURSO SPIKE DONE")
}
