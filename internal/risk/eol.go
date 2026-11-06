package risk

import "time"

// pgFinalRelease lists the final release (end of life) date of each
// supported Postgres major, from https://www.postgresql.org/support/versioning/
// (read on 2026-11-06). Update when a new major ships.
var pgFinalRelease = map[int]string{
	10: "2022-11-10",
	11: "2023-11-09",
	12: "2024-11-21",
	13: "2025-11-13",
	14: "2026-11-12",
	15: "2027-11-11",
	16: "2028-11-09",
	17: "2029-11-08",
	18: "2030-11-14",
}

// pgEOLDate returns the end-of-life date of a Postgres major, if known.
func pgEOLDate(major int) (time.Time, bool) {
	s, ok := pgFinalRelease[major]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	// The final release happens on that day; support lasts until its end.
	return t.Add(24*time.Hour - time.Second), true
}
