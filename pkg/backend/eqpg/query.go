package eqpg

import "strings"

var likePrefixReplacer = strings.NewReplacer(
	`\`, `\\`,
	`%`, `\%`,
	`_`, `\_`,
)

// likePrefix returns a PostgreSQL LIKE pattern for a literal string prefix.
func likePrefix(prefix string) string {
	return likePrefixReplacer.Replace(prefix) + "%"
}
