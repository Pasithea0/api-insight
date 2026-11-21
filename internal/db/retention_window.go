package db

// Retention windows have two zero-ish states, and conflating them is a bug
// that has now been made twice in this codebase.
//
//   - 0 means UNLIMITED: nothing expires. This is a real policy, set when
//     APP_RETENTION_DAYS=0 or when a project asks to keep everything.
//   - A window can also be absent — no keys at all, or no project selected —
//     which callers need to distinguish from "keeps everything".
//
// Every "the longest promise wins" fold in this service therefore has to
// compare windows with RetentionWider rather than `>`, and every caller that
// needs an absent value has to track that separately. A plain `>` reads 0 as
// the *narrowest* window and quietly shortens an unlimited project to whatever
// finite number happens to sit beside it — which is exactly how a project set
// to "unlimited" ended up capped at its neighbour's seven days.

// RetentionWider reports whether window a keeps events longer than window b.
//
// 0 means unlimited and outranks every finite window. Equal windows, including
// two unlimited ones, are not wider than each other.
func RetentionWider(a, b int) bool {
	switch {
	case a == b:
		return false
	case a <= 0:
		// a is unlimited; b is anything else (or they would be equal).
		return true
	case b <= 0:
		// b is unlimited and a is not.
		return false
	default:
		return a > b
	}
}

// LongestRetentionWindow folds the windows in play into the single window a
// shared event namespace has to honour.
//
// Events are stamped with a key's NAME, not its id, so keys that share a name
// share one namespace while each carries its own window. Honouring the
// shortest of them would delete data a sibling key promised to keep, so the
// longest wins — and unlimited is the longest there is.
//
// defaultDays is what a key that defers (retention_days = 0) inherits, and it
// may itself be unlimited. With no keys at all the namespace is just the
// default.
func LongestRetentionWindow(defaultDays int, keyDays []int) int {
	if len(keyDays) == 0 {
		return defaultDays
	}

	best := 0
	for i, days := range keyDays {
		effective := days
		if effective <= 0 {
			// A deferring key inherits the configured default, which is
			// unlimited when the default is 0.
			effective = defaultDays
		}
		if i == 0 || RetentionWider(effective, best) {
			best = effective
		}
	}
	return best
}
