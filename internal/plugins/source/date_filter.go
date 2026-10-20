package source

import (
	"errors"
	"time"

	"github.com/emersion/go-imap/v2"
)

var errMissingInternalDate = errors.New("IMAP server omitted INTERNALDATE required for timestamp filtering")

func timestampSearchCriteria(since, until int64) imap.SearchCriteria {
	var criteria imap.SearchCriteria
	// IMAP search compares the calendar date without its timezone. Widen the
	// UTC dates to cover offset-shifted dates, then apply exact bounds locally.
	if since != 0 {
		criteria.Since = time.Unix(since, 0).UTC().AddDate(0, 0, -1)
	}
	if until != 0 {
		criteria.Before = time.Unix(until, 0).UTC().AddDate(0, 0, 2)
	}
	return criteria
}

func matchesTimestampBounds(date time.Time, since, until int64) (bool, error) {
	if since == 0 && until == 0 {
		return true, nil
	}
	if date.IsZero() {
		return false, errMissingInternalDate
	}
	if since != 0 && date.Before(time.Unix(since, 0)) {
		return false, nil
	}
	if until != 0 && !date.Before(time.Unix(until, 0)) {
		return false, nil
	}
	return true, nil
}
