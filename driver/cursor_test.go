package driver

import (
	"strings"
	"testing"
	"time"
)

// TestATimeSortsWithTheOnesAlreadyThere.
//
// SQLite compares TEXT by bytes. So a timestamp this driver writes and a
// timestamp something else wrote are ordered by their **separator** before
// anything else in them: 'T' is 0x54 and a space is 0x20, so every `T` row
// sorts after every space row whatever the instants are.
//
// That is not a cosmetic difference. A keyset cursor is `WHERE col > ?` with a
// timestamp bound as the argument, and one written in the other separator is
// either greater than every row or less than every one of them. Greater, and
// the query answers nothing; less, and it answers the whole table -- so the
// first page comes back again, with the cursor it was given, for ever.
//
// It happened: a sandbox seeded from a database `ncruces/go-sqlite3` had
// written -- RFC 3339, so 'T' -- and read here, where the separator used to be
// a space. Scrolling the list appended the same fifty rows until the tab ran
// out of memory. Nothing else was wrong, so nothing else reported it.
//
// So this is about the byte and not about the instant, and `parseTimeString`
// is deliberately not involved: reading has always accepted both.
func TestATimeSortsWithTheOnesAlreadyThere(t *testing.T) {
	// What everything else writes. `time.RFC3339Nano` is this, and so is what
	// `ncruces/go-sqlite3` and every JSON encoder put in a column.
	const elsewhere = "2024-01-02T15:04:05.123456789Z"

	at := time.Date(2024, 1, 2, 15, 4, 5, 123456789, time.UTC)

	for _, tc := range []struct {
		format TimeFormat
		desc   string
	}{
		{TimeFormatOffset, "the default"},
		{TimeFormatUTC, "utc"},
		{TimeFormatDatetime, "datetime"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			got := formatTimeString(at, tc.format, time.UTC)

			// The date and the time have to meet at the same byte, or the two
			// forms sort by that byte rather than by the moment.
			if i := strings.IndexAny(got, "T "); i < 0 || got[i] != 'T' {
				t.Fatalf("%q separates with %q; a row written elsewhere reads %q, and SQLite orders them by that byte",
					got, got[i:i+1], elsewhere)
			}

			// And the whole of it up to the seconds has to agree, which is
			// what makes the byte comparison a comparison of instants.
			if !strings.HasPrefix(elsewhere, got[:len("2024-01-02T15:04:05")]) {
				t.Errorf("%q does not begin like %q", got, elsewhere)
			}
		})
	}
}
