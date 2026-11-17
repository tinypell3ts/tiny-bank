package kit

import "uuid"

// ParseUUID turns caller text into a UUID, accepting only the canonical form:
// lower-case with dashes, as tiny-bank's APIs return them. uuid.Parse also
// accepts braces, a urn:uuid: prefix, no dashes and upper case. Allowing those
// would let one account be reached under many URLs, which anything keyed on
// the path, like a cache or a rate limit, could be fooled by.
func ParseUUID(raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	return id, err == nil && id.String() == raw
}
