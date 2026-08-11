package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"code-agent/internal/session"
)

// escapeLike escapes SQLite LIKE metacharacters so user input is matched as a
// literal substring. The backslash escape is active via `ESCAPE '\'` on every
// LIKE below.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// searchTerms splits a query into whitespace-separated terms; a match must
// contain every term (AND semantics, mirroring FTS default behaviour).
func searchTerms(query string) []string {
	return strings.Fields(strings.TrimSpace(query))
}

// tableHasColumn reports whether the sessions table in this store defines the
// named column. Legacy databases predate the name/summary columns, and a query
// referencing a missing column fails wholesale — which would silently zero out
// the whole store's hits (NewReadOnly never runs migrations).
func (s *Store) tableHasColumn(ctx context.Context, name string) bool {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(sessions)")
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var cname, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if cname == name {
			return true
		}
	}
	return false
}

// SearchMessages returns up to limit keyword matches for query across every
// session in this store. A message matches when its content contains every
// whitespace-separated term of query (LIKE substring, ASCII case-insensitive);
// a session also matches when its name or summary does. The first term anchors
// the returned snippet, which is the matched text excerpt (name, summary, or
// message content window).
//
// This is the LIKE fallback of a full-text search — no FTS index yet: it is
// correct and cheap at personal-agent session counts, and friendlier to CJK
// queries than tokenizer-based indexes, which is why Hermes keeps a separate
// CJK index for the same gap. Callers (the cross-workspace index) rank and
// dedupe hits across stores.
func (s *Store) SearchMessages(ctx context.Context, query string, limit int) ([]session.MessageHit, error) {
	terms := searchTerms(query)
	if len(terms) == 0 || limit <= 0 {
		return nil, nil
	}
	like := func(t string) string { return "%" + escapeLike(t) + "%" }
	cond := func(col string, args *[]any) string {
		clauses := make([]string, 0, len(terms))
		for _, t := range terms {
			clauses = append(clauses, col+" LIKE ? ESCAPE '\\'")
			*args = append(*args, like(t))
		}
		return strings.Join(clauses, " AND ")
	}

	var b strings.Builder
	var args []any

	b.WriteString(`SELECT session_id, role, snippet, rank FROM (`)
	// Name matches (rank 0). Legacy stores predate the name column; skip the
	// branch when it is absent so a missing column can't kill the whole query.
	if s.tableHasColumn(ctx, "name") {
		b.WriteString(`SELECT id AS session_id, '' AS role, name AS snippet, 0 AS rank FROM sessions WHERE name <> '' AND `)
		b.WriteString(cond("name", &args))
		b.WriteString(` UNION ALL `)
	}
	// Summary matches (rank 1): anchor the snippet on the first term.
	if s.tableHasColumn(ctx, "summary") {
		b.WriteString(`SELECT id AS session_id, '' AS role, substr(summary, max(1, instr(summary, ?) - 40), 160) AS snippet, 1 AS rank FROM sessions WHERE summary <> '' AND `)
		args = append(args, terms[0])
		b.WriteString(cond("summary", &args))
		b.WriteString(` UNION ALL `)
	}
	// Message-content matches (rank 2): anchor on the first term, window 240 chars.
	b.WriteString(`SELECT session_id, role, substr(content, max(1, instr(content, ?) - 60), 240) AS snippet, 2 AS rank FROM messages WHERE content <> '' AND `)
	args = append(args, terms[0])
	b.WriteString(cond("content", &args))
	b.WriteString(`) ORDER BY rank`)
	b.WriteString(` LIMIT ?`)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hits := make([]session.MessageHit, 0, limit)
	for rows.Next() {
		var h session.MessageHit
		var rank int
		if err := rows.Scan(&h.SessionID, &h.Role, &h.Snippet, &rank); err != nil {
			return nil, err
		}
		switch rank {
		case 0:
			h.Kind = "name"
		case 1:
			h.Kind = "summary"
		default:
			h.Kind = "content"
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
