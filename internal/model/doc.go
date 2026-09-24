// Package model holds the canonical config model shared by drift and plan,
// and the per-section normalisation rules (project.md §5.3): dropping .id
// and dynamic fields, treating absent and default values as equal, and
// matching entries on their per-section identity key.
package model
