package storage

import (
	"context"
)

// TaxonomyItem is one category or tag with its distinct post count.
type TaxonomyItem struct {
	Name      string `json:"name"`
	PostCount int64  `json:"post_count"`
}

// Taxonomy is the complete discovery response payload.
type Taxonomy struct {
	Categories []TaxonomyItem `json:"categories"`
	Tags       []TaxonomyItem `json:"tags"`
	Revision   int64          `json:"-"`
}

// LoadTaxonomy reads every category and tag with counts in one transaction.
func (db *DB) LoadTaxonomy(ctx context.Context) (*Taxonomy, error) {
	tx, err := db.beginRead(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	st, err := LoadState(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := &Taxonomy{Categories: []TaxonomyItem{}, Tags: []TaxonomyItem{}, Revision: st.ContentRevision}
	rows, err := tx.QueryContext(ctx, `SELECT c.slug, (SELECT count(*) FROM posts p WHERE p.category_id=c.id) FROM categories c ORDER BY c.slug`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var it TaxonomyItem
		if err := rows.Scan(&it.Name, &it.PostCount); err != nil {
			rows.Close()
			return nil, err
		}
		out.Categories = append(out.Categories, it)
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, `SELECT t.slug, (SELECT count(*) FROM post_tags pt WHERE pt.tag_id=t.id) FROM tags t ORDER BY t.slug`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var it TaxonomyItem
		if err := rows.Scan(&it.Name, &it.PostCount); err != nil {
			rows.Close()
			return nil, err
		}
		out.Tags = append(out.Tags, it)
	}
	rows.Close()
	return out, rows.Err()
}
