package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Beginner starts a transaction: a pool, a connection, or a transaction (a savepoint).
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// notExtensionMember filters out objects an extension owns; the first %s is the object's system
// catalog, the second its oid column.
const notExtensionMember = `NOT EXISTS (SELECT 1 FROM pg_depend e
	WHERE e.classid = '%s'::regclass AND e.objid = %s AND e.deptype = 'e')`

// versionTables are goose's own bookkeeping, not part of the schema.
var versionTables = []string{"goose_db_version", "goose_db_version_id_seq"}

// ReadSchema prints the definition of the public schema as canonical text: domains, enums,
// composite types, functions, then every table, view and sequence by name with its columns (in
// order), constraints, indexes, triggers and policies, then comments. Names are sorted and the
// catalog's own deparsing is used throughout, so two databases print the same text exactly when
// their schemas match. Catalog names (type name) sort bytewise; text sorts COLLATE "C", so the
// database's collation cannot change the order either. goose_db_version and objects owned by extensions are left out; owners
// and privileges, which differ between environments, are not part of it.
//
// It runs in a transaction (a savepoint under a transaction) with search_path set to public, so
// the role's search_path cannot change how names print.
func ReadSchema(ctx context.Context, db Beginner) (string, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("db: read schema: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO public`); err != nil {
		return "", fmt.Errorf("db: read schema: %w", err)
	}
	r := schemaReader{ctx: ctx, tx: tx}
	for _, section := range []func() error{r.domains, r.enums, r.composites, r.functions, r.relations, r.comments} {
		if err := section(); err != nil {
			return "", fmt.Errorf("db: read schema: %w", err)
		}
	}
	return r.b.String(), nil
}

type schemaReader struct {
	ctx context.Context
	tx  pgx.Tx
	b   strings.Builder
}

func (r *schemaReader) line(depth int, format string, args ...any) {
	r.b.WriteString(strings.Repeat("  ", depth))
	r.b.WriteString(strings.TrimRight(fmt.Sprintf(format, args...), " \t"))
	r.b.WriteByte('\n')
}

// block prints a multi-line definition (a function or view body) indented under its object.
func (r *schemaReader) block(depth int, text string) {
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		r.line(depth, "| %s", l)
	}
}

func (r *schemaReader) rows(sql string, args []any, scan func(pgx.Rows) error) error {
	rows, err := r.tx.Query(r.ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (r *schemaReader) domains() error {
	type domain struct {
		oid        uint32
		name, base string
		notNull    bool
		dflt, coll *string
	}
	var ds []domain
	err := r.rows(`
		SELECT t.oid, t.typname, format_type(t.typbasetype, t.typtypmod), t.typnotnull, t.typdefault,
			CASE WHEN t.typcollation <> b.typcollation THEN quote_ident(c.collname) END
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		JOIN pg_type b ON b.oid = t.typbasetype
		LEFT JOIN pg_collation c ON c.oid = t.typcollation
		WHERE n.nspname = 'public' AND t.typtype = 'd' AND `+fmt.Sprintf(notExtensionMember, "pg_type", "t.oid")+`
		ORDER BY t.typname`, nil, func(rows pgx.Rows) error {
		var d domain
		if err := rows.Scan(&d.oid, &d.name, &d.base, &d.notNull, &d.dflt, &d.coll); err != nil {
			return err
		}
		ds = append(ds, d)
		return nil
	})
	if err != nil {
		return err
	}
	for _, d := range ds {
		head := "domain " + d.name + " " + d.base
		if d.coll != nil {
			head += " collate " + *d.coll
		}
		if d.notNull {
			head += " not null"
		}
		if d.dflt != nil {
			head += " default " + *d.dflt
		}
		r.line(0, "%s", head)
		// Domain NOT NULL is typnotnull above; newer servers also catalog it as a constraint.
		err := r.rows(`SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
			WHERE contypid = $1 AND contype = 'c' ORDER BY conname`, []any{d.oid}, func(rows pgx.Rows) error {
			var name, def string
			if err := rows.Scan(&name, &def); err != nil {
				return err
			}
			r.line(1, "constraint %s %s", name, def)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *schemaReader) enums() error {
	return r.rows(`
		SELECT t.typname, string_agg(quote_literal(e.enumlabel), ', ' ORDER BY e.enumsortorder)
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		JOIN pg_enum e ON e.enumtypid = t.oid
		WHERE n.nspname = 'public' AND t.typtype = 'e' AND `+fmt.Sprintf(notExtensionMember, "pg_type", "t.oid")+`
		GROUP BY t.typname ORDER BY t.typname`, nil, func(rows pgx.Rows) error {
		var name, labels string
		if err := rows.Scan(&name, &labels); err != nil {
			return err
		}
		r.line(0, "enum %s (%s)", name, labels)
		return nil
	})
}

// composites prints the stand-alone composite types (CREATE TYPE ... AS), not tables' row types.
func (r *schemaReader) composites() error {
	return r.rows(`
		SELECT t.typname, string_agg(quote_ident(a.attname) || ' ' || format_type(a.atttypid, a.atttypmod), ', ' ORDER BY a.attnum)
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		JOIN pg_class c ON c.oid = t.typrelid AND c.relkind = 'c'
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		WHERE n.nspname = 'public' AND t.typtype = 'c' AND `+fmt.Sprintf(notExtensionMember, "pg_type", "t.oid")+`
		GROUP BY t.typname ORDER BY t.typname`, nil, func(rows pgx.Rows) error {
		var name, attrs string
		if err := rows.Scan(&name, &attrs); err != nil {
			return err
		}
		r.line(0, "type %s (%s)", name, attrs)
		return nil
	})
}

func (r *schemaReader) functions() error {
	return r.rows(`
		SELECT p.oid::regprocedure::text, p.prokind,
			CASE WHEN p.prokind IN ('f', 'p') THEN pg_get_functiondef(p.oid) ELSE '' END
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public' AND `+fmt.Sprintf(notExtensionMember, "pg_proc", "p.oid")+`
		ORDER BY p.oid::regprocedure::text COLLATE "C"`, nil, func(rows pgx.Rows) error {
		var sig, kind, def string
		if err := rows.Scan(&sig, &kind, &def); err != nil {
			return err
		}
		switch kind {
		case "a":
			r.line(0, "aggregate %s", sig)
		case "w":
			r.line(0, "window function %s", sig)
		default:
			r.line(0, "function %s", sig)
			r.block(1, def)
		}
		return nil
	})
}

type relation struct {
	oid                         uint32
	name, kind, persistence     string
	rls, forceRLS, isPartition  bool
	options, partKey, partBound *string
	viewDef, inherits           *string
}

func (r *schemaReader) relations() error {
	var rels []relation
	err := r.rows(`
		SELECT c.oid, c.relname, c.relkind::text, c.relpersistence::text, c.relrowsecurity, c.relforcerowsecurity,
			c.relispartition, array_to_string(c.reloptions, ', '),
			CASE WHEN c.relkind = 'p' THEN pg_get_partkeydef(c.oid) END,
			CASE WHEN c.relispartition THEN pg_get_expr(c.relpartbound, c.oid) END,
			CASE WHEN c.relkind IN ('v', 'm') THEN pg_get_viewdef(c.oid) END,
			(SELECT string_agg(i.inhparent::regclass::text, ', ' ORDER BY i.inhseqno) FROM pg_inherits i WHERE i.inhrelid = c.oid)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'f', 'v', 'm', 'S')
			AND c.relname::text <> ALL ($1::text[]) AND `+fmt.Sprintf(notExtensionMember, "pg_class", "c.oid")+`
		ORDER BY c.relname`, []any{versionTables}, func(rows pgx.Rows) error {
		var x relation
		if err := rows.Scan(&x.oid, &x.name, &x.kind, &x.persistence, &x.rls, &x.forceRLS, &x.isPartition,
			&x.options, &x.partKey, &x.partBound, &x.viewDef, &x.inherits); err != nil {
			return err
		}
		rels = append(rels, x)
		return nil
	})
	if err != nil {
		return err
	}
	for _, x := range rels {
		if err := r.relation(x); err != nil {
			return fmt.Errorf("%s: %w", x.name, err)
		}
	}
	return nil
}

var relKinds = map[string]string{
	"r": "table", "p": "partitioned table", "f": "foreign table", "v": "view", "m": "materialized view", "S": "sequence",
}

func (r *schemaReader) relation(x relation) error {
	head := relKinds[x.kind] + " " + x.name
	switch x.persistence {
	case "u":
		head = "unlogged " + head
	case "t":
		head = "temporary " + head
	}
	if x.kind == "S" {
		return r.sequence(x.oid, head)
	}
	r.line(0, "%s", head)
	if x.options != nil && *x.options != "" {
		r.line(1, "with (%s)", *x.options)
	}
	if x.rls {
		r.line(1, "row level security")
	}
	if x.forceRLS {
		r.line(1, "force row level security")
	}
	if x.partKey != nil {
		r.line(1, "partition by %s", *x.partKey)
	}
	if x.partBound != nil {
		r.line(1, "partition %s", *x.partBound)
	}
	if x.inherits != nil {
		r.line(1, "inherits %s", *x.inherits)
	}
	if x.viewDef != nil {
		r.block(1, *x.viewDef)
	}
	steps := []func(uint32) error{r.columns, r.constraints, r.indexes, r.triggers, r.policies}
	if x.kind == "v" {
		steps = []func(uint32) error{r.columns, r.triggers}
	}
	for _, step := range steps {
		if err := step(x.oid); err != nil {
			return err
		}
	}
	return nil
}

func (r *schemaReader) columns(rel uint32) error {
	return r.rows(`
		SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull, pg_get_expr(d.adbin, d.adrelid),
			a.attidentity::text, a.attgenerated::text,
			CASE WHEN a.attcollation <> t.typcollation THEN quote_ident(c.collname) END
		FROM pg_attribute a
		JOIN pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		LEFT JOIN pg_collation c ON c.oid = a.attcollation
		WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, []any{rel}, func(rows pgx.Rows) error {
		var name, typ, identity, generated string
		var notNull bool
		var dflt, coll *string
		if err := rows.Scan(&name, &typ, &notNull, &dflt, &identity, &generated, &coll); err != nil {
			return err
		}
		col := "column " + name + " " + typ
		if coll != nil {
			col += " collate " + *coll
		}
		if notNull {
			col += " not null"
		}
		switch {
		case generated == "s" && dflt != nil:
			col += " generated always as (" + *dflt + ") stored"
		case generated == "v" && dflt != nil:
			col += " generated always as (" + *dflt + ") virtual"
		case dflt != nil:
			col += " default " + *dflt
		}
		switch identity {
		case "a":
			col += " generated always as identity"
		case "d":
			col += " generated by default as identity"
		}
		r.line(1, "%s", col)
		return nil
	})
}

// constraints prints check, foreign key, primary key, unique and exclusion constraints, with
// their deferrability and NOT VALID. NOT NULL is on the column; constraint triggers are under
// triggers.
func (r *schemaReader) constraints(rel uint32) error {
	return r.rows(`
		SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = $1 AND contype IN ('c', 'f', 'p', 'u', 'x')
		ORDER BY conname`, []any{rel}, func(rows pgx.Rows) error {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return err
		}
		r.line(1, "constraint %s %s", name, def)
		return nil
	})
}

func (r *schemaReader) indexes(rel uint32) error {
	return r.rows(`
		SELECT c.relname, pg_get_indexdef(i.indexrelid), i.indisvalid
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE i.indrelid = $1
		ORDER BY c.relname`, []any{rel}, func(rows pgx.Rows) error {
		var name, def string
		var valid bool
		if err := rows.Scan(&name, &def, &valid); err != nil {
			return err
		}
		if !valid {
			def += " -- INVALID"
		}
		r.line(1, "index %s %s", name, def)
		return nil
	})
}

func (r *schemaReader) triggers(rel uint32) error {
	return r.rows(`
		SELECT tgname, pg_get_triggerdef(oid), tgenabled::text FROM pg_trigger
		WHERE tgrelid = $1 AND NOT tgisinternal
		ORDER BY tgname`, []any{rel}, func(rows pgx.Rows) error {
		var name, def, enabled string
		if err := rows.Scan(&name, &def, &enabled); err != nil {
			return err
		}
		if enabled != "O" {
			def += " -- enabled=" + enabled
		}
		r.line(1, "trigger %s %s", name, def)
		return nil
	})
}

func (r *schemaReader) policies(rel uint32) error {
	return r.rows(`
		SELECT p.polname, p.polcmd::text, p.polpermissive,
			coalesce((SELECT string_agg(r, ', ' ORDER BY r COLLATE "C")
				FROM (SELECT CASE WHEN o = 0 THEN 'public' ELSE quote_ident(pg_get_userbyid(o)) END AS r FROM unnest(p.polroles) o) x), ''),
			pg_get_expr(p.polqual, p.polrelid), pg_get_expr(p.polwithcheck, p.polrelid)
		FROM pg_policy p WHERE p.polrelid = $1
		ORDER BY p.polname`, []any{rel}, func(rows pgx.Rows) error {
		var name, cmd, roles string
		var permissive bool
		var using, check *string
		if err := rows.Scan(&name, &cmd, &permissive, &roles, &using, &check); err != nil {
			return err
		}
		pol := fmt.Sprintf("policy %s for %s permissive=%t to %s", name, cmd, permissive, roles)
		if using != nil {
			pol += " using (" + *using + ")"
		}
		if check != nil {
			pol += " with check (" + *check + ")"
		}
		r.line(1, "%s", pol)
		return nil
	})
}

func (r *schemaReader) sequence(rel uint32, head string) error {
	var typ string
	var start, inc, minV, maxV, cache int64
	var cycle bool
	err := r.tx.QueryRow(r.ctx, `
		SELECT format_type(seqtypid, NULL), seqstart, seqincrement, seqmin, seqmax, seqcache, seqcycle
		FROM pg_sequence WHERE seqrelid = $1`, rel).Scan(&typ, &start, &inc, &minV, &maxV, &cache, &cycle)
	if err != nil {
		return err
	}
	seq := fmt.Sprintf("%s %s start %d increment %d min %d max %d cache %d", head, typ, start, inc, minV, maxV, cache)
	if cycle {
		seq += " cycle"
	}
	// A serial column's sequence depends on it automatically ('a'); an identity column's
	// internally ('i').
	err = r.rows(`
		SELECT d.refobjid::regclass::text || '.' || quote_ident(a.attname), d.deptype::text
		FROM pg_depend d
		JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		WHERE d.classid = 'pg_class'::regclass AND d.objid = $1 AND d.refclassid = 'pg_class'::regclass
			AND d.refobjsubid > 0 AND d.deptype IN ('a', 'i')
		ORDER BY (d.refobjid::regclass::text || '.' || quote_ident(a.attname)) COLLATE "C"`, []any{rel}, func(rows pgx.Rows) error {
		var col, dep string
		if err := rows.Scan(&col, &dep); err != nil {
			return err
		}
		if dep == "i" {
			seq += " identity of " + col
		} else {
			seq += " owned by " + col
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.line(0, "%s", seq)
	return nil
}

// comments prints every comment on an object of the public schema, but an extension's.
func (r *schemaReader) comments() error {
	return r.rows(`
		SELECT o.type, o.identity, d.description
		FROM pg_description d
		CROSS JOIN LATERAL pg_identify_object(d.classoid, d.objoid, d.objsubid) o
		WHERE o.schema = 'public' AND split_part(o.identity, '.', 2) <> ALL ($1::text[])
			AND NOT EXISTS (SELECT 1 FROM pg_depend e WHERE e.classid = d.classoid AND e.objid = d.objoid AND e.deptype = 'e')
		ORDER BY o.type COLLATE "C", o.identity COLLATE "C"`, []any{versionTables}, func(rows pgx.Rows) error {
		var typ, identity, text string
		if err := rows.Scan(&typ, &identity, &text); err != nil {
			return err
		}
		r.line(0, "comment on %s %s", typ, identity)
		r.block(1, text)
		return nil
	})
}

// DiffSchemas lists the lines that differ between two ReadSchema texts, each under the object
// it belongs to: "-" for a line only in want, "+" for one only in got. It is empty when they
// are equal.
func DiffSchemas(want, got string) string {
	a := strings.Split(strings.TrimRight(want, "\n"), "\n")
	b := strings.Split(strings.TrimRight(got, "\n"), "\n")
	// Longest common subsequence over lines; schemas run to a few thousand lines at most.
	n, m := len(a), len(b)
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out strings.Builder
	owner, printed := "", ""
	emit := func(sign, l string) {
		if !strings.HasPrefix(l, " ") {
			owner = l
		} else if owner != printed {
			out.WriteString("  " + owner + "\n")
		}
		printed = owner
		out.WriteString(sign + " " + l + "\n")
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			if !strings.HasPrefix(a[i], " ") {
				owner = a[i]
			}
			i++
			j++
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			emit("-", a[i])
			i++
		default:
			emit("+", b[j])
			j++
		}
	}
	return out.String()
}
