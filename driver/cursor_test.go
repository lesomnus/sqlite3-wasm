package driver

import (
	"sort"
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

			// A UTC instant ends in `Z`, not `+00:00`. They are the same
			// moment and different bytes, and 'Z' is 0x5A where '+' is 0x2B --
			// so the row a cursor names compares greater than the cursor, and
			// comes back on the page after its own. `datetime` carries no zone
			// at all, which is its point and is not this.
			if tc.format != TimeFormatDatetime && !strings.HasSuffix(got, "Z") {
				t.Errorf("%q does not end in Z; a row written elsewhere reads %q", got, elsewhere)
			}
		})
	}
}

// TestByteOrderIsTimeOrder, which is the property the whole format exists for.
//
// ISO 8601 is written the way it is so that sorting the text sorts the
// instants, and SQLite leans on exactly that: a keyset cursor is `WHERE col >
// ?` over a TEXT column, compared byte by byte with no notion of a date in it.
// There are date functions -- `datetime()`, `julianday()` -- but wrapping the
// column in one is a call per row and no index, which is the thing a cursor is
// for.
//
// So the format has to hold up the invariant on its own, and two ways of
// writing it did not. A space separator sorts after a 'T'. And a fraction with
// its trailing zeros dropped sorts by its length: `.1` is a tenth and `.15` is
// fifteen hundredths, but 'Z' is 0x5A where '5' is 0x35, so the tenth comes
// second. Both were written by this driver, and both put a row on the wrong
// side of a cursor.
//
// The instants below are chosen for that: fractions of every length, including
// none, and pairs that differ only there.
func TestByteOrderIsTimeOrder(t *testing.T) {
	base := time.Date(2024, 1, 2, 15, 4, 5, 0, time.UTC)
	ns := []int{
		0,
		1,
		100,
		1_000_000,   // .001
		100_000_000, // .1  -- trimmed, this is the one that used to sort last
		150_000_000, // .15
		999_999_999, // .999999999
		123_456_789,
		120_000_000, // .12
	}

	for _, format := range []TimeFormat{TimeFormatOffset, TimeFormatUTC} {
		t.Run(map[TimeFormat]string{TimeFormatOffset: "offset", TimeFormatUTC: "utc"}[format], func(t *testing.T) {
			type at struct {
				when time.Time
				text string
			}

			vs := make([]at, len(ns))
			for i, n := range ns {
				w := base.Add(time.Duration(n))
				vs[i] = at{when: w, text: formatTimeString(w, format, time.UTC)}
			}

			// Sorted the way SQLite would sort them, which is by the bytes.
			byText := append([]at(nil), vs...)
			sort.Slice(byText, func(i, j int) bool { return byText[i].text < byText[j].text })

			byTime := append([]at(nil), vs...)
			sort.Slice(byTime, func(i, j int) bool { return byTime[i].when.Before(byTime[j].when) })

			for i := range byText {
				if !byText[i].when.Equal(byTime[i].when) {
					t.Fatalf("position %d: bytes say %q, the clock says %q -- a cursor over this column skips a row or repeats one",
						i, byText[i].text, byTime[i].text)
				}
			}

			// Every value the same width, which is what makes the above true
			// for instants this test did not think of.
			n := len(vs[0].text)
			for _, v := range vs {
				if len(v.text) != n {
					t.Errorf("%q is %d bytes and %q is %d", v.text, len(v.text), vs[0].text, n)
				}
			}
		})
	}
}
