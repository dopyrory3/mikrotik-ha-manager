// Package plan turns selected diff hunks into an ordered list of REST
// operations for dry-run display and apply (project.md §5.4), preserving
// firewall rule ordering via place-before. Implemented starting milestone 3
// (project.md §9).
package plan
