// Package runtime provisions the VRRP interface(s) a pair definition
// describes and the netwatch/on-master/on-backup/scheduler automation
// layered on top of them (project.md §5.5, plus VRRP interface/VIP
// provisioning), pushing both to both routers idempotently. Every managed
// object is tagged — via a "mtha:" comment, or for the on-master/on-backup
// script bodies a leading "# mtha:" comment line — so it can be identified,
// verified and cleanly removed without disturbing anything mtha didn't
// create (project.md §7.3).
package runtime
