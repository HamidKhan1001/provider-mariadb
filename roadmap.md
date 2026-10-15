# provider-mariadb Roadmap

Plan to take `provider-mariadb` from its current feature-complete pre-alpha to a
provider that can run MariaDB in production on OpenEverest.

Everything below is expressed in terms of the three moving parts:

- **SDK** — `github.com/openeverest/openeverest/v2` (`Instance` API, `provider-runtime/controller`
  helpers, backup/BackupClass/BackupStorage types, maintenance gating).
- **Operator** — `github.com/mariadb-operator/mariadb-operator/v26` (`MariaDB`, `MaxScale`,
  `Backup`, `PhysicalBackup`, `PointInTimeRecovery`, `Restore` CRs). Baseline: **26.10.1**.
- **Provider** — this repo (`definition/`, `internal/provider/`, `charts/`).

The backlog lives in GitHub issues, labelled `priority/critical`,
`priority/high` and `priority/nice-to-have`. This document is the summary;
the issues are the source of truth for scope and acceptance criteria.

---

## 1. Current state

Source of truth: `internal/provider/`, `definition/`, [README](README.md#capabilities).

| Capability | Status | Where / notes |
|---|---|---|
| Provisioning — standalone | ✅ | `mariadb.go` `desiredMariaDB` |
| HA — Galera | ✅ | `galera` topology, odd replicas ≥ 3, soft anti-affinity, operator PDB |
| HA — async replication | ✅ | `replication` topology, ≥ 2 replicas, `semiSyncBootAsReplica` |
| Horizontal scaling | ⚠️ | standalone/Galera OK; **replication scale-out fails** ([#94]) |
| Vertical scaling, storage expansion | ✅ | `engine.resources`, `engine.storage.size` |
| Version upgrades | ✅ | `versions.yaml` bundles (10.11, 11.4, 11.8, 12.3); `mariaDBAutoUpgradeEnabled`; downgrades not rejected ([#97]) |
| Custom `my.cnf` | ✅ | engine `configuration` → `spec.myCnf` |
| Proxy (MaxScale) | ✅ | `proxy` component; failover stays with the operator |
| Scheduling | ✅ | affinity (no `podAffinity`), nodeSelector, tolerations, topology spread |
| TLS | ✅ | on by default, opt-in `required`, Galera SST TLS, CA in connection Secret; cert-manager open ([#39]) |
| Exposure | ✅ | ClusterIP / LoadBalancer / NodePort on the primary (HA) or general Service; read Service open ([#105]) |
| Monitoring | ✅ | `monitoring` component → mysqld-exporter + ServiceMonitor; discovery label ([#92]), alerts ([#103]) open |
| Logical backups (on demand + scheduled) | ✅ | `mariadb` BackupClass, `type: logical`, S3 only |
| Physical backups (on demand + scheduled) | ✅ | `type: physical` (`mariadb-backup`), S3 only |
| Restore — logical in place | ✅ | `SyncRestore` → operator `Restore` |
| Restore — new Instance from backup | ✅ | logical and physical via `spec.dataSource` ([#93]); in-place restores only from the Instance's own backups |
| PITR | ✅ | replication topology only (operator limitation, [#114]) |
| Maintenance gating / provider upgrades | ❌ | [#95] |
| Instance deletion cleanup | ⚠️ | PVCs are orphaned ([#96]); backup data never purged ([#100]) |
| `spec.userSecretRef` | ❌ | [#102] |
| Backup import / DR from bucket | ❌ | [#101] |

### Delivered phases (history)

| Phase | Scope | Issues / PRs |
|---|---|---|
| P1 | Pod anti-affinity & scheduling | #42, #86 |
| P2 | Galera topology | #35 |
| P2b | Async replication topology | #36, #90 |
| P3 | TLS / transport encryption | #38 |
| P4 | Logical + physical backups, schedules, restore | #33, #47, #49, #51 |
| P5 | Point-in-time recovery (replication) | #34, #50 |
| P6 | Monitoring, exposure, MaxScale proxy | #7, #28, #37 / #83 |
| — | Operator 26.10.1, MariaDB 12.3 default, auto-upgrade | #73, #74, #75 (follow-up tests/docs: #78) |

---

## 2. Production readiness — `priority/critical`

Must be closed before the pre-alpha banner can be removed. Ordered by risk.

| # | Item | Why it blocks production |
|---|---|---|
| ~~[#93]~~ | ✅ Seeding from a **logical** backup via `dataSource` | Fixed in #117 |
| [#94] | Replication **scale-out** | Operator needs `replica.bootstrapFrom`; advertised capability broken |
| [#95] | **Maintenance gating** + `CheckUpgrade` | `autoUpdateDataPlane: true` rolls every cluster on provider upgrade without approval |
| [#96] | **PVC lifecycle** on deletion | Orphaned volumes; same-name Instance reattaches stale datadir |
| [#97] | **Validate** unsafe updates | Version downgrade, storage shrink (Instance stuck), storageClass change (ignored) |
| [#98] | **Replica recovery** (after #94) | Broken replicas stay broken until rebuilt by hand |
| [#99], [#78] | **Failure-scenario + upgrade tests** | Failover, Galera recovery, scale-out, logical restore, provider upgrade are untested |

## 3. Day-2 operations — `priority/high`

| # | Item |
|---|---|
| [#100] | Honour `Backup.deletionPolicy: Delete` (purge S3 data) |
| [#101] | DR: import existing backups from a BackupStorage (`BackupImporter`) |
| [#102] | `spec.userSecretRef` initial credentials |
| [#103], [#92] | Alerting rules (`PrometheusRule`) and ServiceMonitor discovery labels |
| [#104] | Runbooks: Galera bootstrap (`forceClusterBootstrapInPod`), switchover, suspend, restore |

## 4. Good to have — `priority/nice-to-have`

| # | Item |
|---|---|
| [#105] | Read-only replica Service + read host in connection details |
| [#106] | Credential rotation (root + application user) |
| [#107] | Exact count-based backup retention (today approximated by age) |
| [#108] | S3 SSE-C encryption for backups and binlogs |
| [#109] | VolumeSnapshot physical backups |
| [#110] | Logical restore parameters (database subset) |
| [#111] | HA tuning parameters (semi-sync, max lag, failover delay, switchover, Galera SST/probes) |
| [#112] | `timeZone` engine parameter |
| [#76], [#77] | zstd compression for backups and PITR binlogs |
| [#39] | cert-manager integration |
| [#113] | Cross-cluster DR replica (operator multi-cluster topology) |
| [#114] | PITR on Galera/standalone — **blocked upstream** (`docs/pitr.md`) |
| [#115] | Refresh mysqld-exporter / MaxScale versions |

### Known platform constraints

- `BackupStorage` is S3-only in the OpenEverest API; the operator also supports Azure Blob,
  PVC and VolumeSnapshot storage. Azure support needs a platform API change first.
- PITR binlog archival is replication-only in mariadb-operator 26.10 (#114).

---

## 5. Suggested delivery order

1. Correctness bugs: #94, #96, #97 (#93 done).
2. Safety for upgrades: #95, then #78 / #99 test coverage.
3. Self-healing: #98.
4. Day-2: #100, #101, #103 + #92, #102, #104.
5. Nice-to-have items as demand appears.

## 6. Cross-cutting reminders

- After any `definition/` or RBAC change: `make generate`, then `make verify`
  (CI fails on uncommitted generated diffs).
- `SyncMariaDB` server-side applies a freshly built object (`desiredMariaDB`)
  declaring only provider-owned fields; the operator keeps the ones it defaults.
  Never apply an object read with `Get`, and keep declaring immutable fields
  (`bootstrapFrom`, `storageClassName`) so the apply never removes them.
- New spec/params must be optional with safe defaults (backward compatibility).
- Anything that changes the MariaDB pod template restarts pods: user-initiated
  changes are fine, provider-initiated ones must go through maintenance gating (#95).
- Add a chainsaw suite (separate CI job) per capability under `test/integration/`.
- Follow the MongoDB provider (`provider-percona-server-mongodb/`) for patterns:
  backup import, maintenance gating, upgrade preflight, `userSecretRef`, PVC cleanup.

[#39]: https://github.com/openeverest/provider-mariadb/issues/39
[#76]: https://github.com/openeverest/provider-mariadb/issues/76
[#77]: https://github.com/openeverest/provider-mariadb/issues/77
[#78]: https://github.com/openeverest/provider-mariadb/issues/78
[#92]: https://github.com/openeverest/provider-mariadb/issues/92
[#93]: https://github.com/openeverest/provider-mariadb/issues/93
[#94]: https://github.com/openeverest/provider-mariadb/issues/94
[#95]: https://github.com/openeverest/provider-mariadb/issues/95
[#96]: https://github.com/openeverest/provider-mariadb/issues/96
[#97]: https://github.com/openeverest/provider-mariadb/issues/97
[#98]: https://github.com/openeverest/provider-mariadb/issues/98
[#99]: https://github.com/openeverest/provider-mariadb/issues/99
[#100]: https://github.com/openeverest/provider-mariadb/issues/100
[#101]: https://github.com/openeverest/provider-mariadb/issues/101
[#102]: https://github.com/openeverest/provider-mariadb/issues/102
[#103]: https://github.com/openeverest/provider-mariadb/issues/103
[#104]: https://github.com/openeverest/provider-mariadb/issues/104
[#105]: https://github.com/openeverest/provider-mariadb/issues/105
[#106]: https://github.com/openeverest/provider-mariadb/issues/106
[#107]: https://github.com/openeverest/provider-mariadb/issues/107
[#108]: https://github.com/openeverest/provider-mariadb/issues/108
[#109]: https://github.com/openeverest/provider-mariadb/issues/109
[#110]: https://github.com/openeverest/provider-mariadb/issues/110
[#111]: https://github.com/openeverest/provider-mariadb/issues/111
[#112]: https://github.com/openeverest/provider-mariadb/issues/112
[#113]: https://github.com/openeverest/provider-mariadb/issues/113
[#114]: https://github.com/openeverest/provider-mariadb/issues/114
[#115]: https://github.com/openeverest/provider-mariadb/issues/115
