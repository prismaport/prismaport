package db

import (
	"strings"
	"testing"
)

func TestVersionList(t *testing.T) {
	for _, c := range []struct {
		in   []int64
		want string
	}{
		{[]int64{101, 102, 103, 104}, "101-104"},
		{[]int64{1, 2, 26, 29, 30}, "1-2, 26, 29-30"},
		{[]int64{100}, "100"},
		{nil, ""},
	} {
		t.Run(c.want, func(t *testing.T) {
			if got := versionList(c.in); got != c.want {
				t.Errorf("%v: %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestDiffSchemas(t *testing.T) {
	want := "table a\n  column id text not null\n  column x int\ntable b\n  column id text\n"
	if d := DiffSchemas(want, want); d != "" {
		t.Fatalf("equal schemas differ:\n%s", d)
	}
	got := "table a\n  column id text not null\n  column y int\ntable b\n  column id text\n  index b_id x\ntable c\n"
	d := DiffSchemas(want, got)
	for _, line := range []string{"  table a\n- " + "  column x int\n+   column y int\n", "  table b\n+   index b_id x\n", "+ table c\n"} {
		if !strings.Contains(d, line) {
			t.Errorf("diff misses %q:\n%s", line, d)
		}
	}
}

func TestStatusString(t *testing.T) {
	for _, c := range []struct {
		st   Status
		want string
	}{
		{Status{}, "empty"},
		{Status{Version: 100}, "baseline chain (from version 100); up to date"},
		{Status{Version: 100, Pending: []int64{101, 102}}, "pending 101-102"},
	} {
		t.Run(c.want, func(t *testing.T) {
			if got := c.st.String(); !strings.Contains(got, c.want) {
				t.Errorf("%+v: %q, want %q in it", c.st, got, c.want)
			}
		})
	}
}
