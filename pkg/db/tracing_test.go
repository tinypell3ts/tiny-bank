package db

import "testing"

func TestSummaryOf(t *testing.T) {
	for sql, want := range map[string]string{
		"UPDATE transfers SET lease = gen_random_uuid() WHERE id = $1":                           "UPDATE transfers",
		"UPDATE accounts AS a SET balance = v.balance FROM unnest($1::uuid[])":                   "UPDATE accounts",
		"\n\t  insert into journals (idempotency_key) values ($1)":                               "INSERT journals",
		"SELECT count(*), min(created_at) FROM transfers WHERE state = 'pending'":                "SELECT transfers",
		"SELECT id, balance FROM accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE":             "SELECT accounts FOR UPDATE",
		"SELECT pg_advisory_xact_lock(hashtextextended($1, 0))":                                  "SELECT pg_advisory_xact_lock",
		"SELECT id FROM accounts WHERE id = ANY($1) ORDER BY id FOR NO KEY UPDATE":               "SELECT accounts FOR NO KEY UPDATE",
		"SELECT id FROM account_shards s JOIN accounts a ON a.id = s.id for  no key update of s": "SELECT account_shards FOR NO KEY UPDATE",
		"SELECT id FROM transfers LIMIT $1 FOR UPDATE SKIP LOCKED":                               "SELECT transfers FOR UPDATE",
		"SELECT id FROM accounts WHERE id = $1 FOR KEY SHARE":                                    "SELECT accounts FOR KEY SHARE",
		"SELECT j.id FROM (SELECT $1::text AS key) k LEFT JOIN journals j":                       "SELECT", // a subquery: no plain name
		"SELECT $1::int": "SELECT",
		"begin":          "BEGIN",
		"commit;":        "COMMIT",
		"  ":             "QUERY",
	} {
		if got := summaryOf(sql); got != want {
			t.Errorf("summaryOf(%q) = %q, want %q", sql, got, want)
		}
	}
}
