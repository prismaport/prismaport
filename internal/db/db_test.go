package db_test

import (
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/db"
)

func TestRequireSessionConnection(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		pooler    bool
	}{
		{"neon direct", "postgresql://u:p@ep-example-00000000.c-6.us-east-2.aws.neon.tech/neondb?sslmode=require", false},
		{"neon pooled", "postgresql://u:p@ep-example-00000000-pooler.c-6.us-east-2.aws.neon.tech/neondb?sslmode=require", true},
		{"pooled fallback host", "postgres://u:p@db1.internal,ep-a-pooler.neon.tech/app", true},
		{"local", "postgres://u:p@127.0.0.1:55434/prismaport_dev?sslmode=disable", false},
		{"pooler outside the first label", "postgres://u:p@db.pooler-1.example.com/app", false},
		{"keyword form", "host=ep-x-pooler.neon.tech user=u dbname=app", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := db.RequireSessionConnection(tc.url)
			if tc.pooler && (err == nil || !strings.Contains(err.Error(), "transaction pooler")) {
				t.Fatalf("want a pooler error, got %v", err)
			}
			if !tc.pooler && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

func TestRequireSessionConnectionRejectsAnUnparsableURL(t *testing.T) {
	if err := db.RequireSessionConnection("postgres://u:p@host:notaport/app"); err == nil {
		t.Fatal("want a parse error")
	}
}

func TestParseConfigSizesThePoolExplicitly(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		want      int32
	}{
		{"no pool_max_conns", "postgres://u:p@127.0.0.1:55434/app?sslmode=disable", db.DefaultMaxConns},
		{"url pool_max_conns", "postgres://u:p@127.0.0.1:55434/app?sslmode=disable&pool_max_conns=3", 3},
		{"keyword pool_max_conns", "host=127.0.0.1 user=u dbname=app pool_max_conns=25", 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := db.ParseConfig(tc.url)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.MaxConns != tc.want {
				t.Fatalf("MaxConns %d, want %d", cfg.MaxConns, tc.want)
			}
		})
	}
}

func TestRequireMaxConns(t *testing.T) {
	small, err := db.ParseConfig("postgres://u:p@127.0.0.1/app?pool_max_conns=3")
	if err != nil {
		t.Fatal(err)
	}
	enough, err := db.ParseConfig("postgres://u:p@127.0.0.1/app?pool_max_conns=5")
	if err != nil {
		t.Fatal(err)
	}

	smallErr := db.RequireMaxConns(small, db.MinWorkerConns)
	enoughErr := db.RequireMaxConns(enough, db.MinWorkerConns)

	if smallErr == nil || !strings.Contains(smallErr.Error(), "pool_max_conns=3") {
		t.Fatalf("a 3-connection pool was accepted: %v", smallErr)
	}
	if enoughErr != nil {
		t.Fatalf("a 5-connection pool was refused: %v", enoughErr)
	}
}
