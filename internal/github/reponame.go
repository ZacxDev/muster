package github

import "regexp"

// repoNameRe is a GitHub owner/name: an owner of 1-39 letters, digits and
// hyphens (not starting with a hyphen), a slash, and a name of 1-100 letters,
// digits, '.', '_' and '-'.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)

// ValidRepoName reports whether s is a GitHub owner/name. It is THE shape
// check for a repository muster is told about by name: a task's repository
// (internal/api connectedRepo) and an officer's configured repository
// (internal/officers Parse) ask it, so the two cannot accept different shapes.
func ValidRepoName(s string) bool { return repoNameRe.MatchString(s) }
