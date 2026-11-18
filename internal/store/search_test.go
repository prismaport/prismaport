package store

import (
	"strings"
	"testing"
)

func TestParseSessionSearch(t *testing.T) {
	evm := "0x" + strings.Repeat("ab", 32)
	cases := []struct {
		q    string
		want SessionSearch
	}{
		{"", SessionSearch{}},
		{"   ", SessionSearch{}},
		{"ses_01jbx", SessionSearch{Exact: "ses_01jbx", IDPrefix: "ses_01JBX"}},
		{" SES_01JB ", SessionSearch{Exact: "SES_01JB", IDPrefix: "ses_01JB"}},
		{"ses_", SessionSearch{Exact: "ses_", IDPrefix: "ses_"}},
		{"ses_01JB_X", SessionSearch{Exact: "ses_01JB_X"}},
		{"0xABCDEF", SessionSearch{Exact: "0xABCDEF"}},
		{"0xABCDEF12", SessionSearch{Exact: "0xABCDEF12", HashPrefix: "0xabcdef12", HashFold: true}},
		{"0xABCDEF1", SessionSearch{Exact: "0xABCDEF1"}},
		{evm, SessionSearch{Exact: evm, HashPrefix: evm, HashFold: true}},
		{"5KtPn1LGuxhF", SessionSearch{Exact: "5KtPn1LGuxhF", HashPrefix: "5KtPn1LGuxhF"}},
		{"25", SessionSearch{Exact: "25", Amount: "25"}},
		{"25.00", SessionSearch{Exact: "25.00", Amount: "25.00"}},
		{"12.345678", SessionSearch{Exact: "12.345678", Amount: "12.345678"}},
		{"25000000", SessionSearch{Exact: "25000000", Amount: "25000000", HashPrefix: "25000000"}},
		{"25.", SessionSearch{Exact: "25."}},
		{"-25", SessionSearch{Exact: "-25"}},
		{"1e3", SessionSearch{Exact: "1e3"}},
		{"25,00", SessionSearch{Exact: "25,00"}},
		{"%_' OR 1=1 --", SessionSearch{Exact: "%_' OR 1=1 --"}},
		{"0x12%", SessionSearch{Exact: "0x12%"}},
	}
	for _, c := range cases {
		t.Run(c.q, func(t *testing.T) {
			if got := ParseSessionSearch(c.q); got != c.want {
				t.Errorf("%q: %+v, want %+v", c.q, got, c.want)
			}
		})
	}
}

func TestPrefixEnd(t *testing.T) {
	for prefix, want := range map[string]string{"ses_01JB": "ses_01JC", "0xab": "0xac", "abz": "ab{", "Z": "[", "9": ":", "ses_": "ses`"} {
		t.Run(prefix, func(t *testing.T) {
			if got := prefixEnd(prefix); got != want {
				t.Errorf("%q: %q, want %q", prefix, got, want)
			}
			if !(prefix < want) || !(prefix+"zzzz" < want) {
				t.Errorf("%q: %q does not bound the prefix", prefix, want)
			}
		})
	}
}

func TestSessionSearchSQLIsParameterized(t *testing.T) {
	q := "0x12'; DROP TABLE sessions; --"
	var args []any
	sql := ParseSessionSearch(q).where(&args)
	if strings.Contains(sql, "DROP") || strings.Contains(sql, "'") {
		t.Fatalf("query text in the SQL: %s", sql)
	}
	if len(args) != 1 || args[0] != q {
		t.Fatalf("args: %v", args)
	}
	args = []any{"m", true}
	sql = ParseSessionSearch("0xABCDEF12345").where(&args)
	for _, want := range []string{"lower(p.tx_hash) ~>=~ $4", "lower(p.tx_hash) ~<~ $5", "s.id = $3"} {
		if !strings.Contains(sql, want) {
			t.Errorf("%s lacks %s", sql, want)
		}
	}
	if args[3] != "0xabcdef12345" || args[4] != "0xabcdef12346" {
		t.Errorf("hash bounds: %v", args)
	}
	if got := ParseSessionSearch("").where(&args); got != "true" {
		t.Errorf("empty search: %s", got)
	}
}
