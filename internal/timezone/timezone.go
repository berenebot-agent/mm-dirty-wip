// Package timezone holds the account/user time zone preference helpers. Time
// zones affect display only: all persisted and API timestamps remain UTC.
//
// The IANA rule data is embedded via the time/tzdata import below so that
// conversions keep working on hosts without a system tzdata tree (for example a
// slim self-hosted image). The standard LoadLocation search order still applies,
// so a system tree, when present, is used ahead of the embedded copy.
package timezone

import (
	"fmt"
	"time"

	_ "time/tzdata"
)

// Default is the zone used when neither a user nor an account preference is set.
const Default = "UTC"

// Validate reports whether tz is acceptable as a stored preference. The empty
// string is allowed and means "inherit" (user falls back to account, account
// falls back to UTC). Any other value must name an IANA zone that LoadLocation
// can resolve.
func Validate(tz string) error {
	if tz == "" {
		return nil
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("unknown time zone %q", tz)
	}
	return nil
}

// Resolve returns the location for a user preference and an account default,
// preferring the user value, then the account value, then UTC. Unknown names
// are ignored (treated as unset) so a stale preference can never break a page.
func Resolve(userTZ, accountTZ string) *time.Location {
	for _, name := range []string{userTZ, accountTZ} {
		if name == "" {
			continue
		}
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	return time.UTC
}

// Options returns the selectable zone names for the UI, always with a leading
// UTC entry. The list is the generated IANA set.
func Options() []string {
	out := make([]string, 0, len(IANAZones)+1)
	out = append(out, Default)
	out = append(out, IANAZones...)
	return out
}
