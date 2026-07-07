package digest

// Generic is the no-op fallback parser: no findings — the digest carries
// only exit code, counts, and the log tail.  It's what makes *any* command
// usable in a manifest before a real parser exists.
func Generic([]byte) Findings { return Findings{} }
