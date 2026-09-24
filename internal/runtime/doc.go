// Package runtime renders and deploys the netwatch, VRRP on-master/on-backup
// script and scheduler templates pushed to both routers, all tagged with an
// "mtha:" comment prefix for idempotent deploy/verify/remove (project.md
// §5.5). Implemented starting milestone 4 (project.md §9).
package runtime
